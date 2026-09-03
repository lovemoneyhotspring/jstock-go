"""J-Quants API（V2）の HTTP クライアント。

足の取得（:mod:`wbcore.data.jquants_provider`）と蓄積
（:mod:`wbcore.data.jquants_archive`）が共有する薄い層。認証ヘッダ、
``pagination_key`` の追跡、レート制限の遵守（送信間隔の制御と 429 の再試行）、
5xx の再試行だけを担い、応答の解釈は呼び出し側に任せる。

レート制限（Standard: 120 回/分）:
    - **送る前に**間隔を空ける（:class:`Throttle`、既定 100 回/分）。一括の
      バックフィルや EDINET の遡り（数千リクエスト）はこれで上限内に収まる
    - **それでも 429 なら** ``Retry-After``（無ければ 1 分）待って再試行する。
      窓は 1 分なので、数秒の指数バックオフでは足りない
    - 署名付き URL からのダウンロードは API の回数に数えない

端点: ``https://api.jquants.com/v2``。応答は
``{"data": [...], "pagination_key": "..."}``。一括ダウンロードは
``/bulk/list`` → ``/bulk/get`` → 署名付き URL（5 分有効）→ ``csv.gz``。
"""

from __future__ import annotations

import threading
import time
from typing import Any

from tenacity import retry, retry_if_exception_type, stop_after_attempt, wait_exponential

from wbcore.credentials import MissingCredentialsError, load_api_key
from wbcore.data.provider import MarketDataError
from wbcore.logging import get_logger, register_secret

log = get_logger(__name__)

#: API のベース URL。
BASE_URL = "https://api.jquants.com/v2"

#: API キーを置く環境変数（``.env`` でも同じ名前）。
API_KEY_VAR = "WBJP_JQUANTS_API_KEY"

#: 既定の送信上限（回/分）。Standard プランの 120 より少し下げ、他のプロセス
#: （``accum sync`` と ``jquants sync`` が同時に走る等）の分を残す。
DEFAULT_RATE_PER_MINUTE = 100

#: 429 を受けたときに待つ秒数（``Retry-After`` が無いとき）。制限は 1 分の窓なので 1 分。
RATE_LIMIT_WAIT = 60.0


class RateLimited(MarketDataError):
    """429。待って再試行する。"""

    def __init__(self, message: str, retry_after: float | None = None) -> None:
        super().__init__(message)
        self.retry_after = retry_after


#: 待って再試行する例外。4xx（認証・引数の誤り）は含めない。
_RETRYABLE = (RateLimited, OSError, ConnectionError, TimeoutError)


def _wait(retry_state: Any) -> float:
    """再試行までの待ち。429 は ``Retry-After``（無ければ 1 分）、それ以外は指数。"""
    outcome = retry_state.outcome
    exc = outcome.exception() if outcome is not None else None
    if isinstance(exc, RateLimited):
        return float(exc.retry_after or RATE_LIMIT_WAIT)
    return float(wait_exponential(multiplier=2, min=2, max=60)(retry_state))


class Throttle:
    """送信間隔を空けて、分あたりの回数を上限内に収める。

    「前回の送信から ``60 / rate`` 秒空ける」だけの単純な間隔制御。バーストは
    許さないが、実装が小さく、複数の呼び出し元（足の取得と蓄積）が同じ
    インスタンスを共有すればプロセス内で上限を守れる。``rate=0`` で無効。
    """

    def __init__(self, rate_per_minute: float) -> None:
        self.interval = 60.0 / rate_per_minute if rate_per_minute > 0 else 0.0
        self._lock = threading.Lock()
        self._next = 0.0

    def wait(self) -> None:
        if self.interval <= 0:
            return
        with self._lock:
            now = time.monotonic()
            delay = self._next - now
            if delay > 0:
                time.sleep(delay)
                now = time.monotonic()
            self._next = now + self.interval


class JQuantsClient:
    """認証付きの GET。1 インスタンスが 1 つの HTTP セッションを持つ。"""

    def __init__(
        self,
        api_key: str,
        *,
        base_url: str = BASE_URL,
        max_attempts: int = 8,
        timeout: int = 60,
        session: Any | None = None,
        rate_per_minute: float = DEFAULT_RATE_PER_MINUTE,
    ) -> None:
        if not api_key:
            raise MissingCredentialsError(
                f"J-Quants の API キーがありません。{API_KEY_VAR} を環境変数か .env に設定してください"
            )
        register_secret(api_key)
        self.base_url = base_url.rstrip("/")
        self.max_attempts = max_attempts
        self.timeout = timeout
        self._api_key = api_key
        self._session = session
        self.throttle = Throttle(rate_per_minute)

    @classmethod
    def from_env(cls) -> JQuantsClient:
        """環境変数 / ``.env`` の API キーで組み立てる。"""
        return cls(load_api_key(API_KEY_VAR) or "")

    @property
    def session(self) -> Any:
        if self._session is None:
            import requests

            session = requests.Session()
            session.headers.update({"x-api-key": self._api_key, "User-Agent": "wbjp"})
            self._session = session
        return self._session

    # -- 公開 ---------------------------------------------------------------

    def get(self, path: str, params: dict[str, str] | None = None) -> dict[str, Any]:
        """1 ページぶん。JSON のオブジェクトを返す。"""
        return self._get(f"{self.base_url}{path}", dict(params or {}))

    def get_all(self, path: str, params: dict[str, str] | None = None) -> list[dict[str, Any]]:
        """``pagination_key`` を辿って ``data`` の全行を集める。"""
        rows: list[dict[str, Any]] = []
        query = dict(params or {})
        while True:
            payload = self.get(path, query)
            batch = payload.get("data")
            if isinstance(batch, list):
                rows.extend(batch)
            key = payload.get("pagination_key")
            if not key:
                return rows
            query["pagination_key"] = str(key)

    def bulk_list(self, endpoint: str) -> list[dict[str, Any]]:
        """一括ダウンロードできるファイルの一覧（``Key`` / ``Size`` / ``LastModified``）。"""
        payload = self.get("/bulk/list", {"endpoint": endpoint})
        data = payload.get("data")
        return list(data) if isinstance(data, list) else []

    def bulk_download(self, key: str) -> bytes:
        """``/bulk/get`` で署名付き URL を貰い、``csv.gz`` の中身をそのまま返す。"""
        payload = self.get("/bulk/get", {"key": key})
        url = payload.get("url")
        if not isinstance(url, str) or not url:
            raise MarketDataError(f"一括ダウンロードの URL が返りませんでした: key={key}")
        return self._download(url)

    # -- 内部 ---------------------------------------------------------------

    def _get(self, url: str, params: dict[str, str]) -> dict[str, Any]:
        @retry(
            stop=stop_after_attempt(self.max_attempts),
            wait=_wait,
            retry=retry_if_exception_type(_RETRYABLE),
            reraise=True,
        )
        def _call() -> dict[str, Any]:
            self.throttle.wait()
            response = self.session.get(url, params=params, timeout=self.timeout)
            self._check(response, url, params)
            payload: Any = response.json()
            if not isinstance(payload, dict):
                raise MarketDataError(f"J-Quants の応答が不正です: {type(payload).__name__}")
            return dict(payload)

        try:
            return _call()
        except MarketDataError:
            raise
        except Exception as exc:
            raise MarketDataError(f"J-Quants からの取得に失敗しました: {exc}") from exc

    def _download(self, url: str) -> bytes:
        @retry(
            stop=stop_after_attempt(self.max_attempts),
            wait=_wait,
            retry=retry_if_exception_type(_RETRYABLE),
            reraise=True,
        )
        def _call() -> bytes:
            # 署名付き URL（別ホスト）。API キーは送らず、レート制限にも数えない
            response = self.session.get(url, headers={"x-api-key": ""}, timeout=self.timeout * 5)
            self._check(response, url, {})
            return bytes(response.content)

        try:
            return _call()
        except MarketDataError:
            raise
        except Exception as exc:
            raise MarketDataError(f"一括ファイルのダウンロードに失敗しました: {exc}") from exc

    @staticmethod
    def _check(response: Any, url: str, params: dict[str, str]) -> None:
        status = int(response.status_code)
        if status == 429:
            retry_after = _retry_after(response)
            log.warning(
                "jquants_rate_limited",
                code="jquants.rate_limited",
                url=url,
                symbol=params.get("code"),
                retry_after=retry_after,
            )
            raise RateLimited("J-Quants のレート制限に達しました", retry_after)
        if status >= 500:
            raise ConnectionError(f"J-Quants がエラーを返しました: HTTP {status}")
        if status >= 400:
            raise MarketDataError(
                f"J-Quants への問い合わせが拒否されました: HTTP {status} {response.text[:200]}"
            )


def _retry_after(response: Any) -> float | None:
    """``Retry-After`` ヘッダ（秒）。無ければ None。"""
    headers = getattr(response, "headers", None) or {}
    try:
        value = headers.get("Retry-After")
    except AttributeError:
        return None
    try:
        return float(value) if value else None
    except (TypeError, ValueError):
        return None
