"""立花証券 e支店 API（e_api_v4r10）のラッパー。

出典（一次資料。自動要約は使っていない）:
    - API リファレンス https://www.e-shiten.jp/e_api/mfds_json_api_ref_text.html（Shift_JIS）
    - 公式サンプル https://www.e-shiten.jp/e_api/e_api_sample_v4r10.py / .txt

通信の要点:
    1. **ログイン** ``{base}auth/`` に ``{"p_no","p_sd_date","sJsonOfmt":"5","sCLMID":"CLMAuthLoginRequest",
       "sAuthId":...}`` を POST（``Content-Type: application/json``、ボディは JSON 文字列）。
    2. 応答の仮想URL（``sUrlRequest`` / ``sUrlMaster`` / ``sUrlPrice`` / ``sUrlEvent`` …）は
       利用設定画面で登録した**公開鍵で暗号化**されている。base64 → RSA-OAEP(SHA-256) で
       秘密鍵復号して使う。
    3. 以後の電文は仮想URL（取引系は ``sUrlRequest``、時価は ``sUrlPrice``）に同じ形式で送る。
       ``p_no`` は**通番**（送るたびに +1、その日の間は保存して続きから）。公式サンプルは
       通番と仮想URLを ``YYYYMMDD`` 付きファイルに保存し、同じ日は再ログインしない。
       ここでも ``state/tachibana/session-<env>-<YYYYMMDD>.json``（0600）に保存する。
    4. 応答は **Shift_JIS**。まず ``p_errno``（``"0"`` 以外は基盤側のエラー。``-62`` 稼働時間外、
       ``2`` セッション切断）、次に ``sResultCode``（``"0"`` 以外は業務エラー）を見る。

発注の要点（``CLMKabuNewOrder``）:
    - ``sBaibaiKubun``: **1＝売、3＝買**（5 現渡、7 現引）
    - ``sGenkinShinyouKubun``: 0 現物、2 新規（制度信用）、4 返済（制度信用）、6/8 一般信用
    - 返済は ``sTatebiType="1"``（個別指定）と ``aCLMKabuHensaiData``（建玉番号・順位・数量）。
      建玉番号は ``CLMShinyouTategyokuList.sOrderTategyokuNumber``
    - ``sZyoutoekiKazeiC``: **1＝特定、3＝一般**、5 NISA、6 N成長
    - ``sOrderPrice``: ``"0"`` 成行、``"*"`` 指定なし、他は指値。``sGyakusasiPrice`` は ``"*"``
    - ``sSecondPassword``（第二暗証番号）は毎回必須
    - 空売り価格規制: 51 単元以上の信用新規売りは成行不可（指値のみ）。事前に弾く

**client_order_id が無い**: 新規注文にクライアント指定の注文IDは無く、採番はブローカー側の
``sOrderNumber``（＋``sEigyouDay``）だけ。:class:`OrderAck.broker_order_id` に
``"<sOrderNumber>/<sEigyouDay>"`` を返し、台帳がそれを保存して :meth:`get_order` の
``broker_order_id`` に渡すことで、別プロセス（cron の close / verify）からも照会できる。
渡されなければ同一プロセスで発注した注文しか解決できない。
"""

from __future__ import annotations

import base64
import datetime as dt
import json
import os
import stat
from collections.abc import Callable, Iterable
from dataclasses import asdict, dataclass
from decimal import Decimal
from pathlib import Path
from typing import Any, ClassVar, Self

import requests

from wbcore.broker.base import (
    Broker,
    BrokerError,
    InsufficientFundsError,
    OrderNotFoundError,
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
    TradeType,
)
from wbcore.logging import get_logger, register_secret

log = get_logger(__name__)


class _CLM:
    """電文ID（sCLMID）。"""

    LOGIN = "CLMAuthLoginRequest"
    BALANCE_SUMMARY = "CLMZanKaiSummary"
    CASH_POSITIONS = "CLMGenbutuKabuList"
    MARGIN_POSITIONS = "CLMShinyouTategyokuList"
    ORDER_LIST = "CLMOrderList"
    ORDER_DETAIL = "CLMOrderListDetail"
    NEW_ORDER = "CLMKabuNewOrder"
    CANCEL_ORDER = "CLMKabuCancelOrder"
    MARKET_PRICE = "CLMMfdsGetMarketPrice"


#: 環境ごとの接続先（末尾のスラッシュまで）。ログインは ``auth/`` を足す。
BASE_URLS: dict[Environment, str] = {
    Environment.UAT: "https://demo-kabuka.e-shiten.jp/e_api_v4r10/",
    Environment.PROD: "https://kabuka.e-shiten.jp/e_api_v4r10/",
}
AUTH_PATH = "auth/"
#: 共通項目 ``sJsonOfmt``（公式サンプルの値）。
JSON_FORMAT = "5"
#: 応答の文字コード。
RESPONSE_ENCODING = "shift_jis"

MARKET_CODE_TSE = "00"
#: 空売り価格規制: 個人は 50 単元以内なら適用除外。それを超える信用新規売りは成行不可。
SHORT_SALE_MARKET_LIMIT = Decimal(50 * 100)
#: 時価問合で取る情報コード。始値・現在値・現在値時刻・前日終値。
MARKET_PRICE_COLUMNS = "pDOP,pDPP,tDPP:T,pPRP"
MARKET_PRICE_BATCH = 120

#: レート制限（未公表。常駐サーバを叩き潰さない控えめな値）。
LIMITS: dict[str, Limit] = {
    "account": Limit(2, 1.0),
    "order_write": Limit(2, 1.0),
    "order_read": Limit(4, 1.0),
    "price": Limit(4, 1.0),
}

JST = Market.JP.timezone

# -- コード表（リファレンスの CLMKabuNewOrder / CLMOrderList の項目説明より） ------------

_SIDE_CODE: dict[Side, str] = {Side.SELL: "1", Side.BUY: "3"}
_SIDE_FROM_CODE: dict[str, Side] = {"1": Side.SELL, "3": Side.BUY}

_TAX_CODE: dict[TaxAccountType, str] = {
    TaxAccountType.SPECIFIC: "1",
    TaxAccountType.GENERAL: "3",
    TaxAccountType.NISA: "5",
}
_TAX_FROM_CODE: dict[str, TaxAccountType] = {
    "1": TaxAccountType.SPECIFIC,
    "3": TaxAccountType.GENERAL,
    "5": TaxAccountType.NISA,
    "6": TaxAccountType.NISA,
}

#: 現金信用区分。制度信用（6ヶ月）を使う。一般信用（6/8）は新規売りができないので使わない。
_TRADE_CODE: dict[TradeType, str] = {
    TradeType.CASH: "0",
    TradeType.MARGIN_OPEN: "2",
    TradeType.MARGIN_CLOSE: "4",
}
_TRADE_FROM_CODE: dict[str, TradeType] = {
    "0": TradeType.CASH,
    "2": TradeType.MARGIN_OPEN,
    "6": TradeType.MARGIN_OPEN,
    "4": TradeType.MARGIN_CLOSE,
    "8": TradeType.MARGIN_CLOSE,
}

#: ``sOrderStatusCode`` → 状態。終局を誤認すると二重発注になるので、確信の無い値は UNKNOWN（＝板に残っている扱い）。
_STATUS_MAP: dict[str, OrderStatus] = {
    "0": OrderStatus.PENDING,  # 受付未済
    "1": OrderStatus.SUBMITTED,  # 未約定
    "2": OrderStatus.REJECTED,  # 受付エラー
    "3": OrderStatus.SUBMITTED,  # 訂正中
    "4": OrderStatus.SUBMITTED,  # 訂正完了
    "5": OrderStatus.SUBMITTED,  # 訂正失敗（注文は残る）
    "6": OrderStatus.SUBMITTED,  # 取消中
    "7": OrderStatus.CANCELLED,  # 取消完了
    "8": OrderStatus.SUBMITTED,  # 取消失敗（注文は残る）
    "9": OrderStatus.PARTIALLY_FILLED,  # 一部約定
    "10": OrderStatus.FILLED,  # 全部約定
    "11": OrderStatus.EXPIRED,  # 一部失効（約定分は filled_quantity に残る）
    "12": OrderStatus.EXPIRED,  # 全部失効
    "13": OrderStatus.PENDING,  # 発注待ち
    "14": OrderStatus.REJECTED,  # 無効
    "15": OrderStatus.SUBMITTED,  # 切替注文 / 逆指注文(切替中)
    "16": OrderStatus.SUBMITTED,  # 切替完了 / 逆指注文(未約定)
    "17": OrderStatus.REJECTED,  # 切替注文失敗
    "19": OrderStatus.EXPIRED,  # 繰越失効
    "50": OrderStatus.PENDING,  # 発注中
}

#: ``sOrderOrderPriceKubun`` → 注文種別。
_ORDER_TYPE_FROM_CODE: dict[str, OrderType] = {
    "1": OrderType.MARKET,
    "2": OrderType.LIMIT,
    "3": OrderType.MARKET,  # 引け成行
    "4": OrderType.LIMIT,  # 引け指値
}

#: 結果コードのうち認証系（セッションを捨てて再ログインが要る）。
_AUTH_ERROR_CODES = {"10001", "10002", "10031", "10033", "10035", "10038", "10039"}


def parse_side(value: Any) -> Side:
    """``sBaibaiKubun`` を解釈する。1＝売、3＝買。現渡(5)・現引(7)は扱わないので落とす。

    売買を取り違えると建玉計算の符号が反転する。推測で丸めず例外にする。
    """
    key = str(value).strip()
    if key in _SIDE_FROM_CODE:
        return _SIDE_FROM_CODE[key]
    raise BrokerError(f"注文の売買区分を解釈できません: {value!r}（sBaibaiKubun。1=売 3=買 以外）")


def parse_status(value: Any) -> OrderStatus:
    """``sOrderStatusCode`` を解釈する。未知の値は UNKNOWN。"""
    return _STATUS_MAP.get(str(value).strip(), OrderStatus.UNKNOWN)


def parse_trade(value: Any) -> TradeType:
    """``sGenkinSinyouKubun`` を解釈する。未知の値は現物扱いにせず落とす。"""
    key = str(value).strip()
    if key in _TRADE_FROM_CODE:
        return _TRADE_FROM_CODE[key]
    raise BrokerError(f"現金信用区分を解釈できません: {value!r}（sGenkinSinyouKubun）")


def _decimal(value: Any, default: Decimal = Decimal(0)) -> Decimal:
    if value is None or value == "" or value == "*":
        return default
    try:
        return Decimal(str(value))
    except (ValueError, ArithmeticError):
        return default


def _plain(value: Decimal) -> str:
    return format(value, "f")


def _parse_jst_datetime(value: Any) -> dt.datetime | None:
    """``YYYYMMDDHHMMSS``（JST）を UTC の aware datetime に。"""
    text = str(value or "").strip()
    if not text or text.startswith("0000"):
        return None
    try:
        return dt.datetime.strptime(text, "%Y%m%d%H%M%S").replace(tzinfo=JST).astimezone(dt.UTC)
    except ValueError:
        return None


def decrypt_virtual_url(private_key_pem: bytes, value: str) -> str:
    """ログイン応答の仮想URLを復号する（base64 → RSA-OAEP / MGF1 / SHA-256）。公式サンプルと同じ。"""
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import padding

    key = serialization.load_pem_private_key(private_key_pem, password=None)
    if not hasattr(key, "decrypt"):
        raise BrokerError("立花証券の秘密鍵が RSA ではありません（PEM を確認してください）")
    plain = key.decrypt(
        base64.b64decode(value),
        padding.OAEP(mgf=padding.MGF1(algorithm=hashes.SHA256()), algorithm=hashes.SHA256(), label=None),
    )
    return plain.decode("ascii").strip()


@dataclass
class _Session:
    """その日のログイン状態。通番と復号済みの仮想URL。"""

    day: str
    p_no: int = 0
    url_request: str = ""
    url_master: str = ""
    url_price: str = ""
    url_event: str = ""
    url_event_ws: str = ""

    @property
    def is_valid(self) -> bool:
        return bool(self.url_request and self.url_price)

    @staticmethod
    def path(directory: Path, env: Environment, day: str) -> Path:
        return directory / f"session-{env.value}-{day}.json"

    @classmethod
    def load(cls, directory: Path, env: Environment, day: str) -> _Session | None:
        path = cls.path(directory, env, day)
        if not path.is_file():
            return None
        try:
            raw = json.loads(path.read_text(encoding="utf-8"))
            session = cls(**raw)
        except (ValueError, TypeError) as exc:
            log.warning("立花証券のセッション保存を読めず作り直します", path=str(path), error=str(exc))
            return None
        return session if session.day == day and session.is_valid else None

    def save(self, directory: Path, env: Environment) -> None:
        directory.mkdir(parents=True, exist_ok=True)
        path = self.path(directory, env, self.day)
        tmp = path.with_suffix(".json.tmp")
        tmp.write_text(json.dumps(asdict(self)), encoding="utf-8")
        os.chmod(tmp, stat.S_IRUSR | stat.S_IWUSR)  # 仮想URLはセッションの鍵。自分だけ
        tmp.replace(path)

    def discard(self, directory: Path, env: Environment) -> None:
        self.path(directory, env, self.day).unlink(missing_ok=True)


class TachibanaBroker(Broker):
    """立花証券 e支店 API（e_api_v4r10）経由の発注。日本株専用（現物・制度信用）。"""

    name: ClassVar[str] = "tachibana"

    #: 認証情報の名前空間。環境変数は ``TACHIBANA_<ENV>_AUTH_ID`` 等、キーチェーンは ``tachibana/<env>``。
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
        session_dir: Path | None = None,
    ) -> Self:
        """認証情報を解決して組み立てる。ネットワークは触らない（初回の電文でログインする）。"""
        if market is not Market.JP:
            raise BrokerError(f"立花証券は日本株専用です（market={market.value} には未対応）")
        credentials = load_tachibana_credentials(env, namespace=cls.credential_namespace)
        if session_dir is None:
            from wbcore.settings import AppSettings

            session_dir = AppSettings().state_dir / "tachibana"
        if extended_hours:
            (notify or log.warning)("立花証券では時間外取引（PTS）を扱いません。立会時間のみ")
        return cls(credentials, env, market=market, tax_type=tax_type, session_dir=session_dir)

    def __init__(
        self,
        credentials: TachibanaCredentials,
        env: Environment,
        *,
        market: Market = Market.JP,
        tax_type: TaxAccountType = TaxAccountType.SPECIFIC,
        session_dir: Path | None = None,
        timeout: float = 15.0,
        balance_cache_ttl: float = 2.0,
        clock: Callable[[], dt.datetime] | None = None,
    ) -> None:
        self._credentials = credentials
        self._env = env
        self._base_url = BASE_URLS[env]
        self._market = market
        self._tax_type = tax_type
        self._session_dir = session_dir or Path("state") / "tachibana"
        self._timeout = timeout
        self._clock = clock or (lambda: dt.datetime.now(tz=JST))
        self._http: requests.Session | None = None
        self._session: _Session | None = None
        #: 発注した注文の client_order_id → (sOrderNumber, sEigyouDay)。プロセス内メモリ。
        self._native_order_ids: dict[str, tuple[str, str]] = {}

        register_secret(credentials.auth_id, credentials.order_password)

        self._account_limiter = RateLimiter(LIMITS["account"])
        self._order_read_limiter = RateLimiter(LIMITS["order_read"])
        self._order_write_limiter = RateLimiter(LIMITS["order_write"])
        self._price_limiter = RateLimiter(LIMITS["price"])
        self._balance_cache: Cached[Balance] = Cached(self._fetch_balance, balance_cache_ttl)
        self._positions_cache: Cached[list[Position]] = Cached(
            self._fetch_positions, balance_cache_ttl
        )

    # -- 口座の識別 ---------------------------------------------------------

    @property
    def account_id(self) -> str:
        """API に口座番号は現れない。認証IDの末尾でログを識別する。"""
        return f"tachibana:***{self._credentials.auth_id[-4:]}"

    @property
    def market(self) -> Market:
        return self._market

    @property
    def currency(self) -> str:
        return self._market.currency

    # -- セッション ---------------------------------------------------------

    @property
    def http(self) -> requests.Session:
        if self._http is None:
            self._http = requests.Session()
        return self._http

    def _today(self) -> str:
        return self._clock().strftime("%Y%m%d")

    def _sd_date(self) -> str:
        return self._clock().strftime("%Y.%m.%d-%H:%M:%S.000")

    def session(self) -> _Session:
        """その日のセッション。保存があれば読み、無ければログインする。"""
        if self._session is not None and self._session.day == self._today():
            return self._session
        loaded = _Session.load(self._session_dir, self._env, self._today())
        if loaded is not None:
            log.info(
                "立花証券のセッションを再利用", env=self._env.value, day=loaded.day, p_no=loaded.p_no
            )
            self._session = loaded
            return loaded
        self._session = self._login()
        return self._session

    def _login(self) -> _Session:
        session = _Session(day=self._today())
        log.info("立花証券にログインします", env=self._env.value, url=self._base_url)
        payload = self._post(self._base_url + AUTH_PATH, {"sCLMID": _CLM.LOGIN, "sAuthId": self._credentials.auth_id}, session)
        self._raise_for_response(_CLM.LOGIN, payload)
        pem = self._credentials.private_key_pem
        try:
            session.url_request = decrypt_virtual_url(pem, str(payload.get("sUrlRequest") or ""))
            session.url_master = decrypt_virtual_url(pem, str(payload.get("sUrlMaster") or ""))
            session.url_price = decrypt_virtual_url(pem, str(payload.get("sUrlPrice") or ""))
            session.url_event = decrypt_virtual_url(pem, str(payload.get("sUrlEvent") or ""))
            session.url_event_ws = decrypt_virtual_url(pem, str(payload.get("sUrlEventWebSocket") or ""))
        except Exception as exc:
            raise BrokerError(
                "立花証券の仮想URLを復号できません。登録した公開鍵と秘密鍵が対か、"
                f"交付書面が未読でないか（sKinsyouhouMidokuFlg）を確認してください: {exc}"
            ) from exc
        if not session.is_valid:
            raise BrokerError(f"立花証券のログイン応答に仮想URLがありません: {payload!r}")
        session.save(self._session_dir, self._env)
        return session

    def _discard_session(self) -> None:
        if self._session is not None:
            self._session.discard(self._session_dir, self._env)
        self._session = None

    # -- 電文の送受信 -------------------------------------------------------

    def _post(self, url: str, body: dict[str, Any], session: _Session) -> dict[str, Any]:
        """1電文を送り、応答を dict にして返す。通番を進めて保存する。"""
        session.p_no += 1
        message = {
            "p_no": str(session.p_no),
            "p_sd_date": self._sd_date(),
            "sJsonOfmt": JSON_FORMAT,
            **body,
        }
        text = json.dumps(message, ensure_ascii=False)
        try:
            response = self.http.post(
                url,
                data=text.encode("utf-8"),
                headers={"Content-Type": "application/json"},
                timeout=self._timeout,
            )
        except requests.RequestException as exc:
            raise BrokerError(f"立花証券 API への接続に失敗しました（{body.get('sCLMID')}）: {exc}") from exc
        finally:
            if session.is_valid:
                session.save(self._session_dir, self._env)
        if response.status_code != 200:
            raise BrokerError(
                f"立花証券 API が HTTP {response.status_code} を返しました（{body.get('sCLMID')}）"
            )
        raw = response.content
        try:
            payload: dict[str, Any] = json.loads(raw.decode(RESPONSE_ENCODING))
        except (UnicodeDecodeError, ValueError):
            try:
                payload = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, ValueError):
                raise BrokerError(
                    f"立花証券 API の応答を解釈できません（{body.get('sCLMID')}）: {raw[:200]!r}"
                ) from None
        errno = str(payload.get("p_errno", "0")).strip()
        if errno not in ("", "0"):
            err = str(payload.get("p_err") or "")
            log.error("立花証券 API 基盤エラー", clmid=body.get("sCLMID"), p_errno=errno, p_err=err)
            if errno == "2":
                self._discard_session()
                raise _SessionLost(f"立花証券のセッションが切断されました（p_errno=2）: {err}")
            if errno == "-62":
                raise BrokerError(f"立花証券 API の情報提供時間外です（p_errno=-62）: {err}")
            raise BrokerError(f"立花証券 API 基盤エラー（p_errno={errno}）: {err}")
        return payload

    def _request(self, clmid: str, params: dict[str, Any], *, interface: str = "request") -> dict[str, Any]:
        """ログイン後の電文。``interface`` は ``request``（取引系）か ``price``（時価）。

        セッション切断（p_errno=2）なら 1 回だけログインし直して送り直す。
        """
        for attempt in (1, 2):
            session = self.session()
            url = session.url_price if interface == "price" else session.url_request
            try:
                payload = self._post(url, {"sCLMID": clmid, **params}, session)
            except _SessionLost:
                if attempt == 2:
                    raise
                continue
            self._raise_for_response(clmid, payload)
            return payload
        raise AssertionError("unreachable")

    def _raise_for_response(self, clmid: str, payload: dict[str, Any]) -> None:
        code = str(payload.get("sResultCode", "0")).strip()
        if code in ("", "0"):
            return
        text = str(payload.get("sResultText") or "")
        log.error("立花証券 API 業務エラー", clmid=clmid, code=code, detail=text)
        message = f"{clmid} に失敗（{code}）: {text}"
        if code in _AUTH_ERROR_CODES:
            self._discard_session()
            raise BrokerError(f"立花証券の認証に失敗しました。{message}")
        if clmid == _CLM.ORDER_DETAIL and "注文" in text:
            raise OrderNotFoundError(message)
        if "不足" in text or "余力" in text:
            raise InsufficientFundsError(message)
        if code.startswith("11"):
            raise OrderRejectedError(message)
        raise BrokerError(message)

    # -- 口座 ---------------------------------------------------------------

    def get_balance(self) -> Balance:
        return self._balance_cache.get()

    def get_positions(self) -> list[Position]:
        return self._positions_cache.get()

    def invalidate_cache(self) -> None:
        self._balance_cache.invalidate()
        self._positions_cache.invalidate()

    def _fetch_balance(self) -> Balance:
        """可能額サマリー。``sGenbutuKabuKaituke``＝現物買付可能額、``sSinyouSinkidate``＝信用新規建可能額。"""
        self._account_limiter.acquire()
        payload = self._request(_CLM.BALANCE_SUMMARY, {})
        cash = _decimal(payload.get("sGenbutuKabuKaituke"))
        return Balance(
            currency="JPY",
            cash_balance=cash,
            buying_power=cash,
            margin_buying_power=_decimal(payload.get("sSinyouSinkidate")),
        )

    def _fetch_positions(self) -> list[Position]:
        """現物の保有（``CLMGenbutuKabuList``）と信用建玉（``CLMShinyouTategyokuList``、売建は負）。"""
        self._account_limiter.acquire()
        cash = self._request(_CLM.CASH_POSITIONS, {"sIssueCode": ""})
        positions: list[Position] = []
        for entry in _rows(cash.get("aGenbutuKabuList")):
            quantity = _decimal(entry.get("sUriOrderZanKabuSuryou"))
            if quantity == 0:
                continue
            positions.append(
                Position(
                    symbol=str(entry.get("sUriOrderIssueCode", "")),
                    quantity=quantity,
                    available_quantity=_decimal(entry.get("sUriOrderUritukeKanouSuryou"), quantity),
                    cost_price=_decimal(entry.get("sUriOrderGaisanBokaTanka")),
                    last_price=_decimal(entry.get("sUriOrderHyoukaTanka")),
                    currency="JPY",
                    tax_type=_TAX_FROM_CODE.get(str(entry.get("sUriOrderZyoutoekiKazeiC")), self._tax_type),
                )
            )
        self._account_limiter.acquire()
        margin = self._request(_CLM.MARGIN_POSITIONS, {"sIssueCode": ""})
        for entry in _rows(margin.get("aShinyouTategyokuList")):
            quantity = _decimal(entry.get("sOrderTategyokuSuryou"))
            if quantity == 0:
                continue
            sign = Decimal(-1) if parse_side(entry.get("sOrderBaibaiKubun")) is Side.SELL else Decimal(1)
            positions.append(
                Position(
                    symbol=str(entry.get("sOrderIssueCode", "")),
                    quantity=sign * quantity,
                    available_quantity=sign * _decimal(entry.get("sOrderHensaiKanouSuryou"), quantity),
                    cost_price=_decimal(entry.get("sOrderTategyokuTanka")),
                    last_price=_decimal(entry.get("sOrderHyoukaTanka")),
                    currency="JPY",
                    tax_type=_TAX_FROM_CODE.get(str(entry.get("sOrderZyoutoekiKazeiC")), self._tax_type),
                    trade=TradeType.MARGIN_OPEN,
                )
            )
        return positions

    def margin_positions(self, symbol: str) -> list[dict[str, Any]]:
        """銘柄の信用建玉（生の行）。返済の建玉指定に使う。"""
        self._account_limiter.acquire()
        payload = self._request(_CLM.MARGIN_POSITIONS, {"sIssueCode": symbol})
        return [e for e in _rows(payload.get("aShinyouTategyokuList")) if str(e.get("sOrderIssueCode")) == symbol]

    # -- 注文 ---------------------------------------------------------------

    def get_open_orders(self) -> list[Order]:
        """未約定＋一部約定（``sOrderSyoukaiStatus="5"``）。当日（＋繰越）分しか返らない。"""
        self._order_read_limiter.acquire()
        payload = self._request(
            _CLM.ORDER_LIST, {"sIssueCode": "", "sSikkouDay": "", "sOrderSyoukaiStatus": "5"}
        )
        orders = []
        for entry in _rows(payload.get("aOrderList")):
            number = str(entry.get("sOrderOrderNumber", ""))
            day = str(entry.get("sOrderSikkouDay", ""))
            orders.append(
                self._to_order(
                    entry,
                    number=number,
                    day=day,
                    filled_key="sOrderYakuzyouSuryo",
                    fill_price_key="sOrderYakuzyouPrice",
                    symbol_key="sOrderIssueCode",
                )
            )
        return orders

    def get_order(self, client_order_id: str, *, broker_order_id: str | None = None) -> Order | None:
        """注文を照会する。``broker_order_id``（``"<sOrderNumber>/<sEigyouDay>"``）が要る。

        同一プロセスで発注した注文はメモリから引ける。どちらも無ければ
        「見つからない」ではなく **分からない** ので例外——None を返すと
        「無いから再送してよい」と誤認される。
        """
        native = _split_native_id(broker_order_id) or self._native_order_ids.get(client_order_id)
        if native is None:
            raise BrokerError(
                f"client_order_id={client_order_id!r} の立花証券の注文番号が分かりません"
                "（台帳の broker_order_id を get_order(broker_order_id=...) に渡してください）"
            )
        number, day = native
        self._order_read_limiter.acquire()
        try:
            payload = self._request(_CLM.ORDER_DETAIL, {"sOrderNumber": number, "sEigyouDay": day})
        except OrderNotFoundError:
            return None
        return self._to_order(
            payload,
            number=number,
            day=day,
            filled_key="sYakuzyouSuryou",
            fill_price_key="sYakuzyouPrice",
            symbol_key="sIssueCode",
            client_order_id=client_order_id,
        )

    def preview(self, request: OrderRequest) -> OrderPreview:
        """見積り電文は無い。指値なら概算代金、成行は 0。手数料は信用 0 円、現物は未実装で 0。"""
        return OrderPreview(estimated_cost=request.notional or Decimal(0), estimated_fee=Decimal(0))

    def place(self, request: OrderRequest) -> OrderAck:
        payload_in = self._to_payload(request)
        self._order_write_limiter.acquire()
        log.info(
            "発注します",
            symbol=request.symbol,
            side=request.side.value,
            trade=request.trade.value,
            order_type=request.order_type.value,
            quantity=str(request.quantity),
            limit_price=str(request.limit_price) if request.limit_price else None,
            client_order_id=request.client_order_id,
            reason=request.reason,
        )
        payload = self._request(_CLM.NEW_ORDER, payload_in)
        self.invalidate_cache()
        number = str(payload.get("sOrderNumber") or "")
        day = str(payload.get("sEigyouDay") or "")
        if not (number and day):
            log.error(
                "発注応答に注文番号／営業日がありません。この注文は照会・取消できません",
                client_order_id=request.client_order_id,
                payload=payload,
            )
            return OrderAck(request.client_order_id, None, OrderStatus.SUBMITTED)
        self._native_order_ids[request.client_order_id] = (number, day)
        return OrderAck(request.client_order_id, f"{number}/{day}", OrderStatus.SUBMITTED)

    def cancel(self, client_order_id: str) -> None:
        native = self._native_order_ids.get(client_order_id)
        if native is None:
            raise BrokerError(
                f"client_order_id={client_order_id!r} の立花証券の注文番号が分からないため取消できません"
                "（同一プロセスで発注した注文のみ）"
            )
        number, day = native
        self._order_write_limiter.acquire()
        log.info("注文を取り消します", client_order_id=client_order_id, order_number=number)
        self._request(
            _CLM.CANCEL_ORDER,
            {"sOrderNumber": number, "sEigyouDay": day, "sSecondPassword": self._credentials.order_password},
        )
        self.invalidate_cache()

    # -- 時価 ---------------------------------------------------------------

    def market_prices(self, symbols: Iterable[str]) -> dict[str, dict[str, Any]]:
        """時価問合（``sUrlPrice``）。銘柄 → ``{"open", "last", "prev_close", "at"}``。"""
        wanted = list(dict.fromkeys(s for s in symbols if s))
        found: dict[str, dict[str, Any]] = {}
        for start in range(0, len(wanted), MARKET_PRICE_BATCH):
            batch = wanted[start : start + MARKET_PRICE_BATCH]
            self._price_limiter.acquire()
            payload = self._request(
                _CLM.MARKET_PRICE,
                {"sTargetIssueCode": ",".join(batch), "sTargetColumn": MARKET_PRICE_COLUMNS},
                interface="price",
            )
            for row in _rows(payload.get("aCLMMfdsMarketPrice")):
                symbol = str(row.get("sIssueCode", ""))
                if not symbol:
                    continue
                found[symbol] = {
                    "open": _decimal(row.get("pDOP")),
                    "last": _decimal(row.get("pDPP")),
                    "prev_close": _decimal(row.get("pPRP")),
                    "at": self._price_time(row.get("tDPP:T")),
                }
        return found

    def _price_time(self, value: Any) -> dt.datetime:
        """``tDPP:T``（現在値時刻）を UTC に。形式が読めなければ今。"""
        text = str(value or "").strip()
        today = self._clock().date()
        for fmt in ("%H:%M:%S", "%H%M%S", "%H:%M", "%H%M"):
            try:
                t = dt.datetime.strptime(text, fmt).time()
            except ValueError:
                continue
            return dt.datetime.combine(today, t, tzinfo=JST).astimezone(dt.UTC)
        return self._clock().astimezone(dt.UTC)

    # -- 変換 ---------------------------------------------------------------

    def _to_payload(self, request: OrderRequest) -> dict[str, Any]:
        if not request.order_type.is_placeable:
            raise ValueError(f"発注できない注文種別です: {request.order_type.value}")
        if request.order_type.is_stop:
            raise ValueError("逆指値はこのシステムでは発注しない（エンジン側で合成する）")
        if request.time_in_force is not TimeInForce.DAY:
            raise ValueError(f"立花証券で {request.time_in_force.value} は未対応です（当日限りのみ）")
        is_market = request.order_type is OrderType.MARKET
        if (
            request.trade is TradeType.MARGIN_OPEN
            and request.side is Side.SELL
            and is_market
            and request.quantity > SHORT_SALE_MARKET_LIMIT
        ):
            raise OrderRejectedError(
                f"{request.symbol}: 51 単元以上の信用新規売りは成行では出せません（空売り価格規制）。"
                f"数量 {request.quantity} を減らすか指値にしてください"
            )
        payload: dict[str, Any] = {
            "sZyoutoekiKazeiC": _TAX_CODE[request.tax_type or self._tax_type],
            "sIssueCode": request.symbol,
            "sSizyouC": MARKET_CODE_TSE,
            "sBaibaiKubun": _SIDE_CODE[request.side],
            "sCondition": "0",
            "sOrderPrice": "0" if is_market else _plain(request.limit_price),  # type: ignore[arg-type]
            "sOrderSuryou": _plain(request.quantity),
            "sGenkinShinyouKubun": _TRADE_CODE[request.trade],
            "sOrderExpireDay": "0",
            "sGyakusasiOrderType": "0",
            "sGyakusasiZyouken": "0",
            "sGyakusasiPrice": "*",
            "sTatebiType": "1" if request.trade is TradeType.MARGIN_CLOSE else "*",
            "sTategyokuZyoutoekiKazeiC": "*",
            "sSecondPassword": self._credentials.order_password,
        }
        if request.trade is TradeType.MARGIN_CLOSE:
            payload["aCLMKabuHensaiData"] = self._repayment_list(request)
        return payload

    def _repayment_list(self, request: OrderRequest) -> list[dict[str, str]]:
        """返済する建玉を個別指定する。反対側の建玉を建日の新しい順（当日優先）に割り当てる。"""
        wanted_side = Side.SELL if request.side is Side.BUY else Side.BUY  # 買戻しなら売建
        rows = [
            e
            for e in self.margin_positions(request.symbol)
            if parse_side(e.get("sOrderBaibaiKubun")) is wanted_side
        ]
        today = self._today()
        rows.sort(key=lambda e: (str(e.get("sOrderTategyokuDay")) != today, str(e.get("sOrderTategyokuDay"))))
        remaining = request.quantity
        allocation: list[dict[str, str]] = []
        for entry in rows:
            if remaining <= 0:
                break
            available = _decimal(entry.get("sOrderHensaiKanouSuryou"))
            if available <= 0:
                continue
            take = min(available, remaining)
            allocation.append(
                {
                    "sTategyokuNumber": str(entry.get("sOrderTategyokuNumber")),
                    "sTatebiZyuni": str(len(allocation) + 1),
                    "sOrderSuryou": _plain(take),
                }
            )
            remaining -= take
        if remaining > 0:
            raise OrderRejectedError(
                f"{request.symbol}: 返済できる{'売建' if wanted_side is Side.SELL else '買建'}が "
                f"{request.quantity - remaining} 株しかありません（要求 {request.quantity} 株）"
            )
        return allocation

    def _to_order(
        self,
        entry: dict[str, Any],
        *,
        number: str,
        day: str,
        filled_key: str,
        fill_price_key: str,
        symbol_key: str,
        client_order_id: str | None = None,
    ) -> Order:
        fill_price = _decimal(entry.get(fill_price_key))
        price_kubun = str(entry.get("sOrderOrderPriceKubun", "")).strip()
        limit = _decimal(entry.get("sOrderOrderPrice"))
        return Order(
            client_order_id=client_order_id or self._client_order_id_for(number) or f"{number}/{day}",
            broker_order_id=f"{number}/{day}",
            symbol=str(entry.get(symbol_key, "")),
            side=parse_side(entry.get("sOrderBaibaiKubun")),
            order_type=_ORDER_TYPE_FROM_CODE.get(price_kubun, OrderType.OTHER),
            quantity=_decimal(entry.get("sOrderOrderSuryou")),
            filled_quantity=_decimal(entry.get(filled_key)),
            status=parse_status(entry.get("sOrderStatusCode")),
            limit_price=limit if price_kubun in ("2", "4") and limit > 0 else None,
            avg_fill_price=fill_price if fill_price > 0 else None,
            created_at=_parse_jst_datetime(entry.get("sOrderOrderDateTime")),
            time_in_force=TimeInForce.DAY,
            trade=parse_trade(entry.get("sGenkinSinyouKubun", "0")),
        )

    def _client_order_id_for(self, number: str) -> str | None:
        for client_id, (native, _) in self._native_order_ids.items():
            if native == number:
                return client_id
        return None


class _SessionLost(BrokerError):
    """p_errno=2（セッション切断）。1 回だけログインし直す。"""


def _rows(value: Any) -> list[dict[str, Any]]:
    """リスト項目は「情報が無い場合は ''」。dict のリストだけを返す。"""
    if not isinstance(value, list):
        return []
    return [row for row in value if isinstance(row, dict)]


def _split_native_id(value: str | None) -> tuple[str, str] | None:
    if not value or "/" not in value:
        return None
    number, day = value.split("/", 1)
    return (number, day) if number and day else None
