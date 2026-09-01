"""立花証券 e支店 API（e_api_v4r10）のラッパー。

電文ID・パラメータ名・コード値は、公開されている API リファレンス
（https://www.e-shiten.jp/e_api/mfds_json_api_ref_text.html、
v4r10 = 「4.10」）を自動要約ツール経由で参照し転記したものです。
**要約を挟んでいるため転記ミスの可能性があります。実弾発注に使う前に、
必ず上記ページ（Shift_JIS）を人の目で直接確認してください。**

確認できたこと（要点）:
    - 通信は**ローカル常駐クライアント不要**。環境ごとに固定のホスト型
      HTTPS エンドポイントを直接叩く（:data:`BASE_URLS`）。以前のドラフトに
      書いていた「Windows常駐クライアント前提」という記述は誤りだったので
      撤回する
    - ログイン（``CLMAuthLoginRequest``、パラメータは ``sAuthId`` のみ）の
      応答に含まれる ``sUrlRequest`` が、以後の取引系電文すべての送信先になる
      （URL 自体にトークンが埋め込まれている方式）
    - 新規注文・取消注文には ``sSecondPassword``（二次パスワード）が必須

**未解決の重大な制約 — 発注の冪等性:**
    ``CLMKabuNewOrder`` のリクエストパラメータに、クライアント側で指定する
    注文ID（Webull の ``client_order_id`` に相当するもの）が**存在しない**。
    採番はブローカー側の ``sOrderNumber`` のみで、注文が受理されるまで
    分からない。

    このリポジトリの冪等性設計（
    :func:`wbcore.domain.models.make_client_order_id` —
    「同じ判断からは必ず同じIDが出るので、同じサイクルを再実行しても
    ブローカー側が重複と認識できる」）は、**ブローカーが client_order_id を
    受け取って重複排除してくれること**が前提になっている。立花証券には
    その仕組みが無いため、この実装はプロセス内メモリで
    ``client_order_id → (sOrderNumber, sEigyouDay)`` を覚えておくだけの
    弱い代替に留まる（:meth:`TachibanaBroker.place` 参照）。cron は
    「1サイクルだけ実行して終了する」設計（``docs/DEPLOY.md``）なので、
    **プロセスをまたぐ再実行では二重発注を検知できない。** 本番投入前に、
    台帳（``state/*.db``）側で ``sOrderNumber`` を永続化する仕組みを
    別途用意することを強く推奨する。
"""

from __future__ import annotations

import datetime as dt
from collections.abc import Callable
from decimal import Decimal
from typing import Any, ClassVar, Self

import requests

from wbcore.broker.base import (
    Broker,
    BrokerError,
    InsufficientFundsError,
    OrderRejectedError,
)
from wbcore.broker.ratelimit import Cached, Limit, RateLimiter
from wbcore.credentials import Environment, TachibanaCredentials, load_tachibana_credentials
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
from wbcore.logging import get_logger, register_secret

log = get_logger(__name__)


class _CLM:
    """電文ID（CLMID）。e_api_v4r10 リファレンスの電文一覧より。"""

    LOGIN = "CLMAuthLoginRequest"
    LOGOUT = "CLMAuthLogoutRequest"
    BALANCE_SUMMARY = "CLMZanKaiSummary"
    POSITIONS = "CLMGenbutuKabuList"
    OPEN_ORDERS = "CLMOrderList"
    ORDER_DETAIL = "CLMOrderListDetail"
    NEW_ORDER = "CLMKabuNewOrder"
    CANCEL_ORDER = "CLMKabuCancelOrder"


#: 環境ごとの接続先。ローカル常駐クライアントは不要（モジュール docstring 参照）。
#: 出典: https://www.e-shiten.jp/e_api/mfds_json_api_ref_text.html
BASE_URLS: dict[Environment, str] = {
    Environment.UAT: "https://demo-kabuka.e-shiten.jp/e_api_v4r10/",
    Environment.PROD: "https://kabuka.e-shiten.jp/e_api_v4r10/",
}

#: 東証の市場コード（``sSizyouC``）。他市場（名証等）を扱うようになったら増やす。
MARKET_CODE_TSE = "00"

#: 現金信用区分（``sGenkinShinyouKubun``）。このシステムは現物取引しかしないため固定。
CASH_TRADE = "0"

#: 執行条件（``sCondition``）。成行・指値以外（寄付・引け・不成）はこのシステムでは使わない。
CONDITION_PLAIN = "0"

#: 注文価格（``sOrderPrice``）に入れる「成行」の値。
MARKET_ORDER_PRICE = "0"

#: 逆指値関連パラメータの「指定なし」値。日本株では逆指値そのものを発注できない
#: （:meth:`TachibanaBroker._to_payload` で弾く）ため常にこの値を送る。
NO_STOP = "0"

#: レート制限。**未公表につき暫定。** 公表値が分かれば差し替える。
LIMITS: dict[str, Limit] = {
    "account": Limit(1, 1.0),
    "order_write": Limit(1, 1.0),
    "order_read": Limit(2, 1.0),
}

#: ``sBaibaiKubun``（売買区分）→ :class:`Side`。"5"（逆日歩・逆張）と "7"
#: （貸株）はこのシステムの :class:`Side` に対応が無いため対象外（未知として扱う）。
_SIDE_MAP: dict[str, Side] = {"1": Side.BUY, "3": Side.SELL}
_SIDE_CODE: dict[Side, str] = {Side.BUY: "1", Side.SELL: "3"}

#: 課税区分（``sZyoutoekiKazeiC``）。"6"（新NISA）は :class:`TaxAccountType` に
#: 対応が無いため、当面 NISA と同じ "5" に倒す（つみたて/一般で別勘定が要るなら要拡張）。
_TAX_TYPE_CODE: dict[TaxAccountType, str] = {
    TaxAccountType.GENERAL: "1",
    TaxAccountType.SPECIFIC: "3",
    TaxAccountType.NISA: "5",
}

#: ``sOrderStatusCode`` → :class:`OrderStatus`。
#:
#: **確度の高いものだけ**を埋めてある。リファレンスには "引けエラー"
#: "約定外" "部分取消" 等、細かい状態コードが他にもあったが、それらの終局性
#: （もう板に残っていないと言い切れるか）を自動要約からは断定できなかった。
#: 未知の値は :func:`parse_status` が ``UNKNOWN``（＝板に残っている扱い）に
#: 倒すので、無理に埋めて「本当は生きている注文を終了扱いにする」より安全。
_STATUS_MAP: dict[str, OrderStatus] = {
    "7": OrderStatus.CANCELLED,  # 取消済み
    "9": OrderStatus.PARTIALLY_FILLED,  # 部分約定
    "10": OrderStatus.FILLED,  # 全約定
    "12": OrderStatus.CANCELLED,  # 全取消
    "14": OrderStatus.EXPIRED,  # 失効
}


def parse_side(value: Any) -> Side:
    """``sBaibaiKubun`` を解釈する。未知の値は例外にする。

    売買を取り違えると建玉計算の符号が反転するため、"5"（逆張）"7"（貸株）
    のような未対応区分は、それらしく丸めず落とす
    （:func:`wbcore.broker.webull.parse_side` と同じ方針）。
    """
    key = str(value).strip()
    if key in _SIDE_MAP:
        return _SIDE_MAP[key]
    raise BrokerError(
        f"注文の売買区分を解釈できません: {value!r}（sBaibaiKubun）。"
        "現物の買い(1)・売り(3)以外は未対応です"
    )


def parse_status(value: Any) -> OrderStatus:
    """``sOrderStatusCode`` を解釈する。未知・未確認の値は ``UNKNOWN`` にする。"""
    return _STATUS_MAP.get(str(value).strip(), OrderStatus.UNKNOWN)


def _decimal(value: Any, default: Decimal = Decimal(0)) -> Decimal:
    """API の数値を Decimal にする。float を経由しないことで精度を保つ。"""
    if value is None or value == "":
        return default
    try:
        return Decimal(str(value))
    except (ValueError, ArithmeticError):
        return default


def _plain(value: Decimal) -> str:
    """Decimal を指数表記なしの文字列にする（API に渡す用）。"""
    return format(value, "f")


class TachibanaBroker(Broker):
    """立花証券 e支店 API（e_api_v4r10）経由の発注。

    モジュール docstring の「未解決の重大な制約」を読んでから使うこと。
    日本株専用（米国株の取り扱いは無い）。
    """

    name: ClassVar[str] = "tachibana"

    #: 認証情報の名前空間。環境変数は ``TACHIBANA_<ENV>_USER_ID`` 等、
    #: キーチェーンは ``tachibana/<env>``。
    credential_namespace: ClassVar[str] = "TACHIBANA"

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
        if market is not Market.JP:
            raise BrokerError(f"立花証券は日本株専用です（market={market.value} には未対応）")

        credentials = load_tachibana_credentials(env, namespace=cls.credential_namespace)
        (notify or log.warning)(
            "TachibanaBroker: 電文ID・フィールド名は公開リファレンスの自動要約経由の"
            "転記です。実弾発注の前に一次資料で必ず照合してください"
        )
        return cls(
            credentials,
            env,
            market=market,
            tax_type=tax_type,
            extended_hours=extended_hours,
        )

    def __init__(
        self,
        credentials: TachibanaCredentials,
        env: Environment,
        *,
        market: Market = Market.JP,
        tax_type: TaxAccountType = TaxAccountType.SPECIFIC,
        extended_hours: bool = False,
        timeout: float = 10.0,
        balance_cache_ttl: float = 2.0,
    ) -> None:
        self._credentials = credentials
        self._env = env
        self._base_url = BASE_URLS[env]
        self._market = market
        self._tax_type = tax_type
        # 立花証券に時間外取引（PTS 等）を足すならここで分岐する。現状は未対応。
        self._extended_hours = extended_hours
        self._timeout = timeout
        self._session: requests.Session | None = None
        #: ログイン応答の「実行URL(REQUEST)」。取引系電文はすべてここへ送る。
        self._url_request: str | None = None

        #: 発注した注文の (client_order_id → (sOrderNumber, sEigyouDay))。
        #: プロセス内メモリのみ。モジュール docstring の「未解決の重大な制約」参照。
        self._native_order_ids: dict[str, tuple[str, str]] = {}

        register_secret(
            credentials.user_id,
            credentials.login_password,
            credentials.order_password,
            credentials.account_id,
        )

        self._account_limiter = RateLimiter(LIMITS["account"])
        self._order_read_limiter = RateLimiter(LIMITS["order_read"])
        self._order_write_limiter = RateLimiter(LIMITS["order_write"])

        self._balance_cache: Cached[Balance] = Cached(self._fetch_balance, balance_cache_ttl)
        self._positions_cache: Cached[list[Position]] = Cached(
            self._fetch_positions, balance_cache_ttl
        )

    # -- 接続 -----------------------------------------------------------

    @property
    def account_id(self) -> str:
        return self._credentials.account_id

    @property
    def market(self) -> Market:
        return self._market

    @property
    def currency(self) -> str:
        return self._market.currency

    @property
    def _http(self) -> requests.Session:
        """コネクションを使い回す ``requests.Session``。遅延初期化。"""
        if self._session is None:
            self._session = requests.Session()
        return self._session

    def _login(self) -> str:
        """ログイン電文を送り、取引系電文の送信先URL（``sUrlRequest``）を得る。

        ``login_password`` はこの電文には現れない（:mod:`wbcore.credentials`
        の :data:`~wbcore.credentials._TACHIBANA_CREDENTIAL_FIELDS` 参照）。
        送らないこと自体が未確認のため見落としの可能性がある。
        """
        log.info("立花証券に接続します", env=self._env.value, url=self._base_url)
        response = self._post(self._base_url, {"sCLMID": _CLM.LOGIN, "sAuthId": self._credentials.user_id})
        self._raise_for_response(_CLM.LOGIN, response)
        url_request = response.get("sUrlRequest")
        if not url_request:
            raise BrokerError(f"立花証券へのログイン応答に sUrlRequest がありません: {response!r}")
        return str(url_request)

    def _ensure_session(self) -> str:
        if self._url_request is None:
            self._url_request = self._login()
        return self._url_request

    def invalidate_cache(self) -> None:
        """残高・建玉のキャッシュを捨てる。発注後に呼ぶ。"""
        self._balance_cache.invalidate()
        self._positions_cache.invalidate()

    # -- 電文の送受信 -----------------------------------------------------

    def _post(self, url: str, body: dict[str, Any]) -> dict[str, Any]:
        """1電文を JSON で POST し、応答を dict にして返す（エラー翻訳はしない）。"""
        try:
            response = self._http.post(url, json=body, timeout=self._timeout)
        except requests.RequestException as exc:
            raise BrokerError(
                f"立花証券 API への接続に失敗しました（{body.get('sCLMID')}）: {exc}"
            ) from exc
        try:
            payload: dict[str, Any] = response.json()
        except ValueError:
            raise BrokerError(
                f"立花証券 API の応答を解釈できません（{body.get('sCLMID')}）: "
                f"{response.text[:200]!r}"
            ) from None
        return payload

    def _request(self, clmid: str, params: dict[str, Any]) -> dict[str, Any]:
        """ログイン後の取引系電文を送る（``sUrlRequest`` 宛て）。"""
        url = self._ensure_session()
        payload = self._post(url, {"sCLMID": clmid, **params})
        self._raise_for_response(clmid, payload)
        return payload

    def _raise_for_response(self, clmid: str, payload: dict[str, Any]) -> None:
        """応答の ``sResultCode`` をドメイン例外に翻訳する。

        確認できているのはログイン関連の3コードのみ（10001/10002: 認証エラー、
        10010: 通信エラー）。残高不足など個別の注文エラーコード体系は未確認
        のため、それ以外の非ゼロコードは一律 :class:`OrderRejectedError` に
        している。分かり次第、分岐を追加すること。
        """
        code = str(payload.get("sResultCode", "0")).strip()
        if code in ("", "0", "00"):
            return
        message = str(payload.get("sResultText") or code)
        log.error("立花証券 API 呼び出しに失敗", clmid=clmid, code=code, detail=message)
        if code in ("10001", "10002"):
            self._url_request = None
            raise BrokerError(f"立花証券への認証に失敗しました（{code}）: {message}")
        if code == "10010":
            raise BrokerError(f"立花証券との通信に失敗しました（{code}）: {message}")
        if "残高" in message or "余力" in message:
            raise InsufficientFundsError(message)
        raise OrderRejectedError(message)

    # -- 口座 -------------------------------------------------------------

    def get_balance(self) -> Balance:
        return self._balance_cache.get()

    def get_positions(self) -> list[Position]:
        return self._positions_cache.get()

    def _fetch_balance(self) -> Balance:
        """残高サマリー（``CLMZanKaiSummary``）を取得する。

        **フィールド対応の確度は中程度。** 自動要約の結果、"買付余力"欄に
        信用新規可能額と読める名前（``sSinyouSinkidate``）が返っており、
        現金残高欄の名前（``sGenbutuKabuKaituke``）も「現物株買付可能額」
        （＝買付余力寄り）に読める。つまり要約段階でラベルと項目が
        入れ替わっている可能性がある。実弾で使う前に一次資料で確認し、
        必要ならキー名を入れ替えること。
        """
        self._account_limiter.acquire()
        payload = self._request(_CLM.BALANCE_SUMMARY, {})
        return Balance(
            currency="JPY",
            cash_balance=_decimal(payload.get("sGenbutuKabuKaituke")),
            buying_power=_decimal(payload.get("sGenbutuKabuKaituke")),
            market_value=_decimal(payload.get("sTotalGaisanHyoukagakuGoukei")),
            unrealized_pnl=_decimal(payload.get("sTotalGaisanHyoukaSonekiGoukei")),
        )

    def _fetch_positions(self) -> list[Position]:
        """現物株保有銘柄一覧（``CLMGenbutuKabuList``）を取得する。"""
        self._account_limiter.acquire()
        payload = self._request(_CLM.POSITIONS, {})
        rows = payload.get("aGenbutuKabuList") or []
        if not isinstance(rows, list):
            rows = [rows]

        positions: list[Position] = []
        for entry in rows:
            if not isinstance(entry, dict):
                continue
            quantity = _decimal(entry.get("sUriOrderZanKabuSuryou"))
            if quantity == 0:
                continue
            positions.append(
                Position(
                    symbol=str(entry.get("sUriOrderIssueCode", "")),
                    quantity=quantity,
                    available_quantity=_decimal(
                        entry.get("sUriOrderUritukeKanouSuryou"), quantity
                    ),
                    cost_price=_decimal(entry.get("sUriOrderGaisanBokaTanka")),
                    last_price=_decimal(entry.get("sUriOrderHyoukaTanka")),
                    currency=self.currency,
                    tax_type=self._tax_type,
                )
            )
        return positions

    # -- 注文 -------------------------------------------------------------

    def get_open_orders(self) -> list[Order]:
        self._order_read_limiter.acquire()
        payload = self._request(_CLM.OPEN_ORDERS, {})
        rows = payload.get("aOrderList") or payload.get("aOrderListDetail") or []
        if not isinstance(rows, list):
            rows = [rows]
        return [self._to_order(entry) for entry in rows if isinstance(entry, dict)]

    def get_order(self, client_order_id: str) -> Order | None:
        """注文を照会する。

        **立花証券には client_order_id 相当のフィールドが無い。** このメソッドは
        自分（同一プロセス）が :meth:`place` した注文しか解決できない。
        別プロセス（＝別の cron 実行）が発注した注文は、たとえ実在しても
        解決できず :class:`BrokerError` になる——「無い」と偽って ``None`` を
        返すと、まだ生きている注文を「見つからなかった＝再送してよい」と
        誤認しかねないため、分からないことを分からないまま例外にしている。
        モジュール docstring の「未解決の重大な制約」を参照。
        """
        native = self._native_order_ids.get(client_order_id)
        if native is None:
            raise BrokerError(
                f"client_order_id={client_order_id!r} に対応する立花証券の注文番号が"
                "分かりません（このブローカーインスタンスが発注した注文のみ照会できます。"
                "モジュール docstring の「未解決の重大な制約」を参照）"
            )
        order_number, eigyou_day = native
        self._order_read_limiter.acquire()
        payload = self._request(
            _CLM.ORDER_DETAIL, {"sOrderNumber": order_number, "sEigyouDay": eigyou_day}
        )
        return self._to_order(payload, client_order_id=client_order_id)

    def preview(self, request: OrderRequest) -> OrderPreview:
        """発注前の見積り。

        リファレンスの電文一覧に専用の見積り電文が見当たらなかった
        （自動要約の見落としの可能性はある）。ブローカー照会はせず、
        指値なら概算約定代金だけを自前計算する簡易版。成行は価格が
        未定なので 0 を返す。手数料表も未確認のため常に 0。
        """
        cost = request.notional or Decimal(0)
        return OrderPreview(estimated_cost=cost, estimated_fee=Decimal(0))

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
        payload = self._request(_CLM.NEW_ORDER, self._to_payload(request))
        self.invalidate_cache()

        order_number = str(payload.get("sOrderNumber") or "")
        eigyou_day = str(payload.get("sEigyouDay") or "")
        if order_number and eigyou_day:
            self._native_order_ids[request.client_order_id] = (order_number, eigyou_day)
        else:
            # 注文番号が取れないと get_order/cancel が今後この注文を解決できない。
            # 黙って進めると「発注はしたが二度と照会・取消できない注文」を
            # 抱えることになるため、ここで気付けるようにする。
            log.error(
                "発注応答に sOrderNumber / sEigyouDay がありません。"
                "今後この注文を client_order_id では照会・取消できません",
                client_order_id=request.client_order_id,
                payload=payload,
            )

        return OrderAck(
            client_order_id=request.client_order_id,
            broker_order_id=order_number or None,
            status=OrderStatus.SUBMITTED,
        )

    def cancel(self, client_order_id: str) -> None:
        native = self._native_order_ids.get(client_order_id)
        if native is None:
            raise BrokerError(
                f"client_order_id={client_order_id!r} に対応する立花証券の注文番号が"
                "分からないため取消できません（モジュール docstring の"
                "「未解決の重大な制約」を参照）"
            )
        order_number, eigyou_day = native

        self._order_write_limiter.acquire()
        log.info("注文を取り消します", client_order_id=client_order_id, order_number=order_number)
        self._request(
            _CLM.CANCEL_ORDER,
            {
                "sOrderNumber": order_number,
                "sEigyouDay": eigyou_day,
                "sSecondPassword": self._credentials.order_password,
            },
        )
        self.invalidate_cache()

    # -- 変換 -------------------------------------------------------------

    def _to_payload(self, request: OrderRequest) -> dict[str, Any]:
        """発注リクエストを ``CLMKabuNewOrder`` の電文パラメータに変換する。"""
        if not request.order_type.is_placeable:
            raise ValueError(f"発注できない注文種別です: {request.order_type.value}")
        if request.order_type.is_stop:
            raise ValueError("立花証券は日本株専用のため逆指値は発注できません")
        if request.time_in_force is not TimeInForce.DAY:
            raise ValueError(
                f"立花証券で {request.time_in_force.value} は未対応です（当日限りのみ）"
            )

        is_market = request.order_type is OrderType.MARKET
        return {
            "sZyoutoekiKazeiC": _TAX_TYPE_CODE[request.tax_type or self._tax_type],
            "sIssueCode": request.symbol,
            "sSizyouC": MARKET_CODE_TSE,
            "sBaibaiKubun": _SIDE_CODE[request.side],
            "sCondition": CONDITION_PLAIN,
            "sOrderPrice": MARKET_ORDER_PRICE if is_market else _plain(request.limit_price),  # type: ignore[arg-type]
            "sOrderSuryou": _plain(request.quantity),
            "sGenkinShinyouKubun": CASH_TRADE,
            "sOrderExpireDay": "0",  # 当日限り
            "sGyakusasiOrderType": NO_STOP,
            "sGyakusasiZyouken": NO_STOP,
            "sGyakusasiPrice": NO_STOP,
            "sSecondPassword": self._credentials.order_password,
        }

    def _to_order(self, entry: dict[str, Any], *, client_order_id: str | None = None) -> Order:
        order_number = str(entry.get("sOrderNumber", ""))
        resolved_client_id = (
            client_order_id
            or self._client_order_id_for(order_number)
            # 自分が発注したものでなければ client_order_id を持たない。
            # 空文字ではなく sOrderNumber を暫定の識別子として使う
            # （ドメインの Order.client_order_id は必須のため）。
            or order_number
        )
        filled_price = _decimal(entry.get("sYakuzyouPrice"))
        return Order(
            client_order_id=resolved_client_id,
            broker_order_id=order_number or None,
            symbol=str(entry.get("sOrderIssueCode", "")),
            side=parse_side(entry.get("sOrderBaibaiKubun")),
            order_type=OrderType.OTHER,  # sCondition から成行/指値を厳密に逆引きできる確証が無い
            quantity=_decimal(entry.get("sOrderOrderSuryou")),
            filled_quantity=_decimal(entry.get("sOrderYakuzyouSuryou")),
            status=parse_status(entry.get("sOrderStatusCode")),
            limit_price=(
                _decimal(entry["sOrderOrderPrice"])
                if entry.get("sOrderOrderPrice") not in (None, "", "0")
                else None
            ),
            avg_fill_price=filled_price if filled_price > 0 else None,
            created_at=_parse_datetime(entry.get("sOrderOrderDateTime")),
            time_in_force=TimeInForce.DAY,
        )

    def _client_order_id_for(self, order_number: str) -> str | None:
        for client_id, (native_number, _) in self._native_order_ids.items():
            if native_number == order_number:
                return client_id
        return None


def _parse_datetime(value: Any) -> dt.datetime | None:
    """``YYYYMMDDHHMMSS`` 形式の日時を解釈する。"""
    if not value:
        return None
    try:
        return dt.datetime.strptime(str(value), "%Y%m%d%H%M%S").replace(tzinfo=dt.UTC)
    except ValueError:
        return None
