"""Webull OpenAPI のラッパー。

SDK がやってくれないこと（＝ここで引き受けること）:

    1. **遅延初期化** — ``TradeClient()`` はコンストラクタの中で
       ``/openapi/config`` にネットワークアクセスする。素直にモジュール
       読み込み時に作ると、テストもオフライン起動も不可能になる。
    2. **レート制限** — 残高・建玉・未約定は 2 回/2 秒。呼び出しを直列化し、
       必要なら待つ。
    3. **例外の翻訳** — SDK の ``ServerException`` をドメイン例外に変換する。
       あわせて、認証情報を含む例外メッセージがそのまま外へ出ないようにする。
    4. **秘匿情報の登録** — 読み込んだ App Key / Secret をログのマスク対象に
       登録する。SDK はエラー時にヘッダを丸ごと出力するため、これが無いと漏れる。

市場ごとの違い:
    - 日本株: 注文種別は成行と指値のみ。逆指値は :mod:`wbjp.risk.stops` で
      合成する。発注には ``market="JP"`` と ``account_tax_type`` が要る。
    - 米国株: STOP_LOSS / STOP_LOSS_LIMIT を置ける。``market="US"`` で発注し、
      残高・建玉は USD の行を読む。時間外取引は設定で明示しない限り使わない。

同じ口座に日本株と米国株が混在するため、このクラスは **1つの市場だけ**を
見る。残高も建玉も、その市場の通貨の行以外は無視する。
"""

from __future__ import annotations

import datetime as dt
from collections.abc import Callable, Iterable
from decimal import Decimal
from typing import Any, ClassVar, Self

from wbcore.broker.base import (
    Broker,
    BrokerError,
    InsufficientFundsError,
    OrderRejectedError,
    RateLimitExceededError,
)
from wbcore.broker.ratelimit import LIMITS, Cached, RateLimiter
from wbcore.credentials import ENDPOINTS, Credentials, Environment, load_credentials
from wbcore.domain.models import (
    Balance,
    Market,
    Order,
    OrderAck,
    OrderPreview,
    OrderRequest,
    OrderStatus,
    OrderType,
    Position,
    Side,
    TaxAccountType,
    TimeInForce,
)
from wbcore.logging import get_logger, harden_third_party_logging, register_secret

log = get_logger(__name__)

#: 発注に固定で入れる値（市場共通）。
INSTRUMENT_TYPE = "EQUITY"
COMBO_TYPE = "NORMAL"
ENTRUST_TYPE = "QTY"

#: 取引セッション。CORE は立会時間のみ。
#: 米国株で時間外を許す場合の値は UAT で要確認（SDK に列挙が無い）。
TRADING_SESSION_CORE = "CORE"
TRADING_SESSION_EXTENDED = "ALL"

#: API から返る文字列 → ドメインの有効期限。
_TIF_MAP = {"DAY": TimeInForce.DAY, "GTC": TimeInForce.GTC}

#: API が返す状態文字列 → ドメインの状態。
_STATUS_MAP = {
    "PENDING": OrderStatus.PENDING,
    "SUBMITTED": OrderStatus.SUBMITTED,
    "WORKING": OrderStatus.SUBMITTED,
    "QUEUED": OrderStatus.SUBMITTED,
    "PARTIAL_FILLED": OrderStatus.PARTIALLY_FILLED,
    "PARTIALLY_FILLED": OrderStatus.PARTIALLY_FILLED,
    "FILLED": OrderStatus.FILLED,
    "CANCELLED": OrderStatus.CANCELLED,
    "CANCELED": OrderStatus.CANCELLED,
    "REJECTED": OrderStatus.REJECTED,
    "FAILED": OrderStatus.REJECTED,
    "EXPIRED": OrderStatus.EXPIRED,
}


def parse_status(value: str | None) -> OrderStatus:
    """API の状態文字列を解釈する。未知の値は UNKNOWN にする。

    未知の状態を「終了」と誤認すると、まだ生きている注文を無視して
    二重発注しかねない。UNKNOWN は :attr:`OrderStatus.is_open` が True に
    なるため、安全側（まだ残っているかもしれない）に倒れる。
    """
    if not value:
        return OrderStatus.UNKNOWN
    return _STATUS_MAP.get(value.strip().upper(), OrderStatus.UNKNOWN)


def parse_order_type(value: str | None) -> OrderType:
    """API の注文種別を解釈する。未知の値は OTHER にする。

    口座には自分が出した注文以外も並ぶ（UAT の共有テスト口座には他人の
    ``TRAILING_STOP_LOSS`` などが入っている）。未知の種別で例外を投げると、
    ``get_open_orders`` を通る日次サイクルが毎回落ちる。読めない種別は
    OTHER に倒す。OTHER は :attr:`OrderType.is_stop` が False なので
    建玉計算では「板に乗っている普通の注文」として保守的に数えられる。
    """
    if not value:
        return OrderType.OTHER
    try:
        return OrderType(value.strip().upper())
    except ValueError:
        return OrderType.OTHER


def parse_side(value: Any) -> Side:
    """API の売買区分を解釈する。

    ここは **推測してはいけない。** 売買を取り違えると
    :func:`~wbjp.engine.reconcile.effective_quantity` の符号が反転し、
    未約定注文を打ち消すはずが積み増す方向に働く。読めなければ落とす。
    """
    try:
        return Side(str(value).strip().upper())
    except ValueError as exc:
        raise BrokerError(
            f"注文の売買区分を解釈できません: {value!r}。建玉計算が狂うため処理を中止します"
        ) from exc


def _decimal(value: Any, default: Decimal = Decimal(0)) -> Decimal:
    """API の数値（文字列で返ってくる）を Decimal にする。

    精度を保つため float を経由しない。
    """
    if value is None or value == "":
        return default
    try:
        return Decimal(str(value))
    except ValueError, ArithmeticError:
        return default


#: 公開テスト口座を使っているときの注意文。
PUBLIC_ACCOUNT_NOTICE = "公開テスト口座を使用中です。残高・建玉は他の利用者により変動します"


class WebullBroker(Broker):
    """Webull OpenAPI 経由の発注。"""

    name: ClassVar[str] = "webull"

    #: 認証情報の名前空間。環境変数は ``WBJP_<ENV>_APP_KEY``、キーチェーンは ``wbjp/<env>``。
    credential_namespace: ClassVar[str] = "WBJP"

    @classmethod
    def connect(
        cls,
        env: Environment,
        *,
        market: Market,
        tax_type: TaxAccountType = TaxAccountType.SPECIFIC,
        extended_hours: bool = False,
        notify: Callable[[str], None] | None = None,
    ) -> Self:
        """認証情報を解決し、環境に応じた接続先で組み立てる。

        dry-run でも本物のブローカーを使う。残高・建玉・未約定の実データが
        無ければ判断が意味を成さないため。発注だけを呼び出し側で止める。
        """
        credentials = load_credentials(env, namespace=cls.credential_namespace)
        if credentials.is_public_test_account:
            (notify or log.warning)(PUBLIC_ACCOUNT_NOTICE)
        broker = cls(
            credentials,
            env,
            ENDPOINTS[env].trade,
            market=market,
            tax_type=tax_type,
            extended_hours=extended_hours,
        )
        broker.check_key_expiry()
        return broker

    def __init__(
        self,
        credentials: Credentials,
        env: Environment,
        endpoint: str,
        *,
        market: Market = Market.JP,
        tax_type: TaxAccountType = TaxAccountType.SPECIFIC,
        extended_hours: bool = False,
        balance_cache_ttl: float = 2.0,
    ) -> None:
        self._credentials = credentials
        self._env = env
        self._endpoint = endpoint
        self._market = market
        self._tax_type = tax_type
        self._extended_hours = extended_hours
        self._client: Any = None
        self._api_client: Any = None
        self._data_client: Any = None

        # SDK がエラー時にヘッダを吐くので、実値をマスク対象に入れておく
        register_secret(credentials.app_key, credentials.app_secret, credentials.account_id)

        self._account_limiter = RateLimiter(LIMITS["account"])
        self._order_read_limiter = RateLimiter(LIMITS["order_read"])
        self._order_write_limiter = RateLimiter(LIMITS["order_write"])
        self._preview_limiter = RateLimiter(LIMITS["preview"])

        self._balance_cache: Cached[Balance] = Cached(self._fetch_balance, balance_cache_ttl)
        self._positions_cache: Cached[list[Position]] = Cached(
            self._fetch_positions, balance_cache_ttl
        )

    # -- 接続 ---------------------------------------------------------------

    @property
    def client(self) -> Any:
        """SDK の TradeClient。初回アクセス時に接続する。

        コンストラクタでネットワークを叩かせないための遅延初期化。
        """
        if self._client is None:
            self._client = self._connect()
        return self._client

    def _connect(self) -> Any:
        from webull.core.client import ApiClient
        from webull.trade.trade_client import TradeClient

        # SDK はインポート時に自前のログハンドラを付け、propagate を切る。
        # そのままだと認証情報を含む出力がマスクを迂回して stderr に出るため、
        # 読み込んだ直後に必ず無力化する。
        harden_third_party_logging()

        log.info(
            "Webull に接続します",
            env=self._env.value,
            endpoint=self._endpoint,
            test_account=self._credentials.is_public_test_account,
        )
        if self._credentials.is_public_test_account:
            log.warning(
                "公開されている共有テスト口座を使用中です。残高や建玉は他の利用者によって変動します"
            )

        try:
            api_client = ApiClient(
                self._credentials.app_key,
                self._credentials.app_secret,
                self._env.value,
            )
            api_client.add_endpoint(self._env.value, self._endpoint)
            _suppress_sdk_own_logging(api_client)
            client = TradeClient(api_client)
            self._api_client = api_client
        except Exception as exc:
            raise self._translate(exc, "接続") from exc

        # 取りこぼしがあった場合に備えて、構築後にもう一度均す
        harden_third_party_logging()
        return client

    @property
    def data_client(self) -> Any:
        """SDK の DataClient（銘柄マスタなど）。取引と同じ接続を使い回す。"""
        if self._data_client is None:
            from webull.data.data_client import DataClient

            _ = self.client  # 先に接続して api_client を作る
            self._data_client = DataClient(self._api_client)
            harden_third_party_logging()
        return self._data_client

    #: 銘柄マスタ照会のカテゴリ。ETF も株式のカテゴリに含まれる（実測）。
    _INSTRUMENT_CATEGORY: ClassVar[dict[Market, str]] = {
        Market.JP: "JP_STOCK",
        Market.US: "US_STOCK",
    }

    def lot_sizes(self, symbols: Iterable[str]) -> dict[str, Decimal]:
        """銘柄マスタの ``lot_size``。マスタに無い銘柄（新規上場など）は返らない。"""
        wanted = [s for s in dict.fromkeys(symbols)]
        if not wanted:
            return {}
        category = self._INSTRUMENT_CATEGORY[self._market]
        try:
            response = self.data_client.instrument.get_instrument(
                symbols=",".join(wanted), category=category
            )
            rows = response.json() if hasattr(response, "json") else response
        except Exception as exc:
            raise self._translate(exc, "銘柄情報の照会") from exc
        found: dict[str, Decimal] = {}
        for row in rows or []:
            symbol, lot = row.get("symbol"), row.get("lot_size")
            if symbol in wanted and lot not in (None, ""):
                found[str(symbol)] = Decimal(str(lot))
        return found

    @property
    def account_id(self) -> str:
        return self._credentials.account_id

    @property
    def market(self) -> Market:
        return self._market

    @property
    def currency(self) -> str:
        return self._market.currency

    def check_key_expiry(self, today: dt.date | None = None) -> int | None:
        """API キーの残り有効日数を確認し、近ければ警告する。

        Webull のキーは既定 45 日で失効する。失効に気付かずに運用が
        止まるのを防ぐ。
        """
        days = self._credentials.days_until_expiry(today)
        if days is None:
            return None
        if days < 0:
            log.error("API キーは失効しています", days_ago=-days)
        elif days <= 7:
            log.warning("API キーの失効が近づいています", days_left=days)
        return days

    # -- 口座 ---------------------------------------------------------------

    def get_balance(self) -> Balance:
        return self._balance_cache.get()

    def get_positions(self) -> list[Position]:
        return self._positions_cache.get()

    def _fetch_balance(self) -> Balance:
        self._account_limiter.acquire()
        payload = self._call(
            lambda: self.client.account_v2.get_account_balance(self.account_id), "残高照会"
        )

        # この市場の通貨の明細だけを見る（同じ口座に JPY と USD の行が並ぶ）
        for entry in payload.get("account_currency_assets", []):
            if entry.get("currency") == self.currency:
                return Balance(
                    currency=self.currency,
                    cash_balance=_decimal(entry.get("cash_balance")),
                    buying_power=_decimal(entry.get("buying_power")),
                    market_value=_decimal(entry.get("market_value")),
                    unrealized_pnl=_decimal(entry.get("unrealized_profit_loss")),
                )

        if self._market is not Market.JP:
            # 通貨別の行が無いのに合計だけ返すと、円の残高でドル建ての
            # 買付余力を見積もることになる。黙って0にせず落とす。
            raise BrokerError(
                f"残高照会に {self.currency} 建ての明細がありません。"
                "口座で米国株取引が有効か、為替振替が済んでいるか確認してください"
            )

        return Balance(
            currency=payload.get("total_asset_currency", "JPY"),
            cash_balance=_decimal(payload.get("total_cash_balance")),
            buying_power=_decimal(payload.get("total_cash_balance")),
            market_value=_decimal(payload.get("total_market_value")),
            unrealized_pnl=_decimal(payload.get("total_unrealized_profit_loss")),
        )

    def _fetch_positions(self) -> list[Position]:
        self._account_limiter.acquire()
        payload = self._call(
            lambda: self.client.account_v2.get_account_position(self.account_id), "建玉照会"
        )

        positions = []
        for entry in payload if isinstance(payload, list) else []:
            # この市場の建玉だけを対象にする（同じ口座に日本株と米国株が混在する）
            if not self._belongs_to_market(entry):
                continue
            quantity = _decimal(entry.get("quantity"))
            if quantity == 0:
                continue
            positions.append(
                Position(
                    symbol=str(entry.get("symbol", "")),
                    quantity=quantity,
                    available_quantity=_decimal(entry.get("available_quantity"), quantity),
                    cost_price=_decimal(entry.get("cost_price")),
                    last_price=_decimal(entry.get("last_price")),
                    currency=entry.get("currency") or self.currency,
                    tax_type=_parse_tax_type(entry.get("account_tax_type")),
                )
            )
        return positions

    def _belongs_to_market(self, entry: dict[str, Any]) -> bool:
        """建玉・注文の明細がこの市場のものか。

        ``market`` があればそれを、無ければ通貨で判断する。どちらも無い
        行は日本株口座の既定（JP）として扱う。
        """
        market = entry.get("market")
        if market:
            return str(market).upper() == self._market.value
        currency = entry.get("currency")
        if currency:
            return str(currency).upper() == self.currency
        return self._market is Market.JP

    def invalidate_cache(self) -> None:
        """残高・建玉のキャッシュを捨てる。発注後に呼ぶ。"""
        self._balance_cache.invalidate()
        self._positions_cache.invalidate()

    # -- 注文 ---------------------------------------------------------------

    def get_open_orders(self) -> list[Order]:
        self._order_read_limiter.acquire()
        payload = self._call(
            lambda: self.client.order_v3.get_order_open(self.account_id), "未約定照会"
        )
        return [
            self._to_order(leg)
            for leg in flatten_order_legs(payload)
            if self._belongs_to_market(leg)
        ]

    def get_order(self, client_order_id: str) -> Order | None:
        self._order_read_limiter.acquire()
        try:
            payload = self._call(
                lambda: self.client.order_v3.get_order_detail(self.account_id, client_order_id),
                "注文照会",
            )
        except BrokerError:
            return None
        legs = flatten_order_legs(payload)
        return self._to_order(legs[0]) if legs else None

    def preview(self, request: OrderRequest) -> OrderPreview:
        self._preview_limiter.acquire()
        payload = self._call(
            lambda: self.client.order_v3.preview_order(
                self.account_id, [self._to_payload(request)]
            ),
            "発注プレビュー",
        )
        if isinstance(payload, list):
            payload = payload[0] if payload else {}
        return OrderPreview(
            estimated_cost=_decimal(payload.get("estimated_cost")),
            estimated_fee=_decimal(payload.get("estimated_transaction_fee")),
        )

    def place(self, request: OrderRequest) -> OrderAck:
        self._order_write_limiter.acquire()
        log.info(
            "発注します",
            symbol=request.symbol,
            side=request.side.value,
            order_type=request.order_type.value,
            quantity=str(request.quantity),
            limit_price=str(request.limit_price) if request.limit_price else None,
            client_order_id=request.client_order_id,
            reason=request.reason,
        )

        payload = self._call(
            lambda: self.client.order_v3.place_order(self.account_id, [self._to_payload(request)]),
            "発注",
        )
        self.invalidate_cache()

        entry = payload[0] if isinstance(payload, list) and payload else payload or {}
        return OrderAck(
            client_order_id=request.client_order_id,
            broker_order_id=entry.get("order_id") or entry.get("orderId"),
            status=parse_status(entry.get("order_status") or entry.get("status"))
            if entry
            else OrderStatus.SUBMITTED,
        )

    def cancel(self, client_order_id: str) -> None:
        self._order_write_limiter.acquire()
        log.info("注文を取り消します", client_order_id=client_order_id)
        self._call(
            lambda: self.client.order_v3.cancel_order(self.account_id, client_order_id),
            "注文取消",
        )
        self.invalidate_cache()

    # -- 変換 ---------------------------------------------------------------

    def _to_payload(self, request: OrderRequest) -> dict[str, str]:
        """発注リクエストを Webull の JSON に変換する。

        価格・数量は **文字列** で渡す。float を経由すると
        ``2500.0000000001`` のような値になり、呼値に乗らず弾かれる。
        """
        # OTHER は他人の注文を読むための受け皿。発注経路には絶対に流さない。
        if not request.order_type.is_placeable:
            raise ValueError(f"発注できない注文種別です: {request.order_type.value}")
        if request.order_type.is_stop and self._market is Market.JP:
            raise ValueError("日本株では逆指値を発注できません（API 非対応）")

        extended = self._extended_hours and self._market is Market.US
        payload = {
            "combo_type": COMBO_TYPE,
            "client_order_id": request.client_order_id,
            "symbol": request.symbol,
            "instrument_type": INSTRUMENT_TYPE,
            "market": self._market.value,
            "order_type": request.order_type.value,
            "quantity": _plain(request.quantity),
            "side": request.side.value,
            "time_in_force": request.time_in_force.value,
            "entrust_type": ENTRUST_TYPE,
            "support_trading_session": (
                TRADING_SESSION_EXTENDED if extended else TRADING_SESSION_CORE
            ),
            "account_tax_type": (request.tax_type or self._tax_type).value,
        }
        if self._market is Market.US:
            payload["trade_currency"] = self.currency
            payload["extended_hours_trading"] = "true" if extended else "false"
        if request.limit_price is not None:
            payload["limit_price"] = _plain(request.limit_price)
        if request.stop_price is not None:
            payload["stop_price"] = _plain(request.stop_price)
        return payload

    def _to_order(self, entry: dict[str, Any]) -> Order:
        filled_price = _decimal(entry.get("filled_price") or entry.get("avg_fill_price"))
        return Order(
            client_order_id=str(entry.get("client_order_id", "")),
            broker_order_id=entry.get("order_id") or entry.get("orderId"),
            symbol=str(entry.get("symbol", "")),
            side=parse_side(entry.get("side")),
            order_type=parse_order_type(entry.get("order_type")),
            quantity=_decimal(entry.get("total_quantity") or entry.get("quantity")),
            filled_quantity=_decimal(entry.get("filled_quantity")),
            status=parse_status(entry.get("status") or entry.get("order_status")),
            limit_price=(_decimal(entry["limit_price"]) if entry.get("limit_price") else None),
            stop_price=(_decimal(entry["stop_price"]) if entry.get("stop_price") else None),
            avg_fill_price=filled_price if filled_price > 0 else None,
            created_at=_parse_epoch_ms(entry.get("place_time")),
            time_in_force=_TIF_MAP.get(
                str(entry.get("time_in_force") or "").upper(), TimeInForce.DAY
            ),
        )

    # -- 呼び出しと例外 -----------------------------------------------------

    def _call(self, operation: Any, description: str) -> Any:
        """SDK を呼び、応答を JSON にして返す。"""
        try:
            response = operation()
        except BrokerError:
            # すでに翻訳済み（遅延接続がここで失敗した場合など）。もう一度
            # _translate に通すと、BrokerError には error_code も http_status
            # も無いため「残高照会に失敗しました（）」まで情報が削れる。
            raise
        except Exception as exc:
            raise self._translate(exc, description) from exc

        status_code = getattr(response, "status_code", 200)
        if status_code != 200:
            text = getattr(response, "text", "")
            raise self._from_status(status_code, f"{description}に失敗: {text}")

        try:
            return response.json()
        except ValueError, AttributeError:
            return {}

    def _translate(self, exc: Exception, description: str) -> BrokerError:
        """SDK の例外をドメイン例外に変換する。

        SDK の例外メッセージにはリクエスト全体（＝認証情報）が載るため、
        そのまま外へは出さない。詳細はマスクを通したログにのみ残す。
        """
        status_code = getattr(exc, "http_status", None) or getattr(exc, "status_code", None)
        code = getattr(exc, "error_code", None) or getattr(exc, "code", "")
        message = getattr(exc, "message", None) or getattr(exc, "error_msg", "") or str(exc)

        log.error(
            "Webull API 呼び出しに失敗",
            operation=description,
            error_code=str(code),
            http_status=status_code,
            detail=message,
        )
        return self._from_status(status_code, f"{description}に失敗しました（{code}）")

    @staticmethod
    def _from_status(status_code: int | None, message: str) -> BrokerError:
        match status_code:
            case 429:
                return RateLimitExceededError(message)
            case 401 | 403:
                return BrokerError(f"{message}: 認証情報または権限を確認してください")
            case _ if "INSUFFICIENT" in message.upper():
                return InsufficientFundsError(message)
            case 400 | 422:
                return OrderRejectedError(message)
            case _:
                return BrokerError(message)


def _suppress_sdk_own_logging(api_client: Any) -> None:
    """SDK が独自のログ出力を仕込むのを抑止する。

    **なぜ必要か（実測で確認した挙動）**

    ``TradeClient.__init__`` は ``_init_logger`` を呼び、ログが未設定だと
    判断すると次の2つを勝手に追加する:

        1. stdout への ``StreamHandler``
        2. **カレントディレクトリの** ``webull_trade_sdk.log`` への
           ``TimedRotatingFileHandler``（INFO、72世代）

    どちらも ``propagate`` とは無関係にこちらのマスク経路を通らない。
    API がエラーを返すと SDK はリクエストヘッダを丸ごと出力するため、
    **認証情報が平文でディスクに残る**ことになる。

    ``_init_logger`` は ``_stream_logger_set`` と ``_file_logger_set`` の
    いずれかが真なら何もしない。そこで構築前に立てておく。
    非公開属性だが、これが SDK 側に用意された唯一の抑止経路であり、
    代替はディスクへの認証情報の書き出しを許すことになる。
    """
    api_client._stream_logger_set = True
    api_client._file_logger_set = True


def flatten_order_legs(payload: Any) -> list[dict[str, Any]]:
    """注文照会のレスポンスから、実データの入った明細を取り出す。

    Webull は注文をコンボ構造で返す。外側は ``client_order_id`` と
    ``combo_type`` しか持たず、銘柄・数量・状態は入れ子の ``orders``
    配列の中にある。外側だけを読むと、銘柄が空で数量 0 の注文が
    並んでいるように見えてしまう（実測で確認済み）。

    単一注文でも複数レッグでも同じ形で扱えるよう、常に明細の
    リストに均す。外側の ``client_order_id`` は、明細側に無い場合の
    ために引き継ぐ。
    """
    if not payload:
        return []

    entries = payload if isinstance(payload, list) else [payload]
    legs: list[dict[str, Any]] = []

    for entry in entries:
        if not isinstance(entry, dict):
            continue
        nested = entry.get("orders")
        if isinstance(nested, list) and nested:
            for leg in nested:
                if isinstance(leg, dict):
                    legs.append({"client_order_id": entry.get("client_order_id"), **leg})
        else:
            legs.append(entry)

    return legs


def _parse_epoch_ms(value: Any) -> dt.datetime | None:
    """ミリ秒エポック（文字列で返ってくる）を日時にする。"""
    if not value:
        return None
    try:
        return dt.datetime.fromtimestamp(int(value) / 1000, tz=dt.UTC)
    except ValueError, TypeError, OSError:
        return None


def _plain(value: Decimal) -> str:
    """指数表記にならない文字列にする。

    ``str(Decimal("1E+3"))`` は ``"1E+3"`` になり、API に弾かれる。
    """
    normalized = value.normalize()
    _, _, exponent = normalized.as_tuple()
    if isinstance(exponent, int) and exponent > 0:
        normalized = normalized.quantize(Decimal(1))
    return f"{normalized:f}"


def _parse_tax_type(value: Any) -> TaxAccountType:
    try:
        return TaxAccountType(str(value).upper())
    except ValueError:
        return TaxAccountType.GENERAL
