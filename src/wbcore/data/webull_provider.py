"""Webull の市場データ API から米国株の足を取得する。

Webull JP の市場データ API は ``US_STOCK`` / ``US_ETF`` にしか対応して
いない（日本株の足は返さない）。米国株ではこちらが一次情報源になる。
yfinance と違って規約上の心配が無く、ブローカーと同じ値を見られる。

制約:
    - 1回の呼び出しで最大 1200 本。日足なら約5年ぶん、5分足なら約15営業日ぶん。
    - 銘柄ごとに 1 リクエスト（バッチ API もあるが、失敗時に切り分け
      しづらいので使わない）。
    - **端点はまだ UAT で疎通確認していない。** 応答の項目名は SDK の
      ドキュメント（``open/high/low/close/volume/time``）に合わせてあるが、
      実機で違えば :func:`_parse_bars` を直す。

日本株の設定でこのプロバイダを選ぶと :class:`MarketDataError` を投げる。
黙って空を返すと「データが無い」のか「対応していない」のか区別できない。
"""

from __future__ import annotations

import datetime as dt
from typing import Any, ClassVar, Self

import polars as pl

from wbcore.credentials import ENDPOINTS, Credentials, Environment, load_credentials
from wbcore.data.provider import (
    Interval,
    MarketDataError,
    MarketDataProvider,
    empty_bars,
    normalize_bars,
)
from wbcore.domain.models import Market
from wbcore.logging import (
    get_logger,
    harden_third_party_logging,
    register_secret,
    suppress_sdk_own_logging,
)

log = get_logger(__name__)

#: 1回の呼び出しで取れる最大本数（SDK のドキュメントより）。
MAX_BARS_PER_CALL = 1200

#: 米国株は株式と ETF で category が分かれる。設定で ETF を明示できない
#: ため、株式で空なら ETF として取り直す。
_US_CATEGORIES = ("US_STOCK", "US_ETF")

#: この実装の間隔 → API の ``timespan``。
_TIMESPANS: dict[Interval, str] = {
    Interval.M1: "M1",
    Interval.M5: "M5",
    Interval.M15: "M15",
    Interval.M30: "M30",
    Interval.H1: "H1",
    Interval.D1: "D1",
}

#: 米国の通常取引時間は 6.5 時間。必要本数の見積もりに使う。
_SESSION_MINUTES = 390


class WebullMarketDataProvider(MarketDataProvider):
    """Webull 市場データ API（米国株のみ）。"""

    name: ClassVar[str] = "webull"
    intervals: ClassVar[frozenset[Interval]] = frozenset(_TIMESPANS)

    #: 認証情報の名前空間。ブローカー（:class:`~wbcore.broker.webull.WebullBroker`）と同じ口座。
    credential_namespace: ClassVar[str] = "WBJP"

    def __init__(
        self,
        credentials: Credentials,
        env: Environment,
        endpoint: str,
        *,
        market: Market = Market.US,
    ) -> None:
        if market is not Market.US:
            raise MarketDataError(
                f"Webull の市場データ API は {market.value} 市場の足を返しません。"
                'data_provider = "yfinance" を使ってください'
            )
        self._credentials = credentials
        self._env = env
        self._endpoint = endpoint
        self._client: Any = None
        register_secret(credentials.app_key, credentials.app_secret, credentials.account_id)

    @classmethod
    def connect(cls, env: Environment, *, market: Market) -> Self:
        credentials = load_credentials(env, namespace=cls.credential_namespace)
        return cls(credentials, env, ENDPOINTS[env].market_data, market=market)

    @property
    def client(self) -> Any:
        if self._client is None:
            self._client = self._connect()
        return self._client

    def _connect(self) -> Any:
        from webull.core.client import ApiClient
        from webull.data.data_client import DataClient

        harden_third_party_logging()
        api_client = ApiClient(
            self._credentials.app_key, self._credentials.app_secret, self._env.value
        )
        api_client.add_endpoint(self._env.value, self._endpoint)
        # DataClient も構築時に自前のログ（webull_data_sdk.log）を仕込む。
        # 認証情報が平文でディスクに残るため、渡す前に必ず抑止する
        suppress_sdk_own_logging(api_client)
        client = DataClient(api_client)
        harden_third_party_logging()
        return client

    def fetch_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
        *,
        interval: Interval = Interval.D1,
    ) -> dict[str, pl.DataFrame]:
        self._require(interval)
        if not symbols:
            return {}
        if start > end:
            raise ValueError(f"start が end より後です: {start} > {end}")

        count = _estimate_count(start, end, interval)
        result: dict[str, pl.DataFrame] = {}
        for symbol in symbols:
            frame = self._fetch_one(symbol, count, interval)
            if frame is None or frame.height == 0:
                log.warning("銘柄の足が空でした", symbol=symbol, provider=self.name)
                continue
            result[symbol] = frame.filter((pl.col("date") >= start) & (pl.col("date") <= end))
        return result

    def _fetch_one(self, symbol: str, count: int, interval: Interval) -> pl.DataFrame | None:
        for category in _US_CATEGORIES:
            try:
                response = self.client.market_data.get_history_bar(
                    symbol, category, _TIMESPANS[interval], count=str(count)
                )
            except Exception as exc:
                raise MarketDataError(f"Webull からの足取得に失敗しました: {symbol}") from exc

            status = getattr(response, "status_code", 200)
            if status != 200:
                raise MarketDataError(
                    f"Webull からの足取得に失敗しました: {symbol} (HTTP {status})"
                )
            frame = _parse_bars(response.json(), interval)
            if frame.height > 0:
                return frame
        return None


def _estimate_count(start: dt.date, end: dt.date, interval: Interval) -> int:
    """期間から必要本数を見積もる（営業日ベースで週末ぶん 7/5 と祝日の余裕）。"""
    trading_days = int((end - start).days * 5 / 7) + 2
    if interval.is_intraday:
        per_day = _SESSION_MINUTES // int(interval.duration.total_seconds() // 60) + 1
        return min(MAX_BARS_PER_CALL, trading_days * per_day + 10)
    return min(MAX_BARS_PER_CALL, trading_days + 10)


def _parse_bars(payload: Any, interval: Interval = Interval.D1) -> pl.DataFrame:
    """API の応答を正規スキーマに変える。

    応答は ``[{"symbol": ..., "result": [{"open": "...", "time": "..."}]}]``
    または ``{"result": [...]}`` の形を想定する。``time`` は ISO 日時か
    エポック秒。
    """
    entries = payload
    if isinstance(entries, list) and entries and isinstance(entries[0], dict):
        entries = entries[0].get("result", entries[0].get("bars", entries))
    elif isinstance(entries, dict):
        entries = entries.get("result", entries.get("bars", []))
    if not isinstance(entries, list):
        return empty_bars(interval)

    rows = []
    for bar in entries:
        if not isinstance(bar, dict):
            continue
        moment = _parse_time(bar.get("time") or bar.get("timestamp") or bar.get("date"))
        if moment is None:
            continue
        row: dict[str, Any] = {
            "open": _float(bar.get("open")),
            "high": _float(bar.get("high")),
            "low": _float(bar.get("low")),
            "close": _float(bar.get("close")),
            "volume": _float(bar.get("volume")),
        }
        if interval.is_intraday:
            row["ts"] = moment
        else:
            # 米国の日足は米東部時間の取引日で解釈する
            row["date"] = moment.astimezone(_US_EASTERN).date()
        rows.append(row)
    if not rows:
        return empty_bars(interval)
    return normalize_bars(pl.DataFrame(rows), interval)


def _parse_time(value: Any) -> dt.datetime | None:
    """API の時刻表現を UTC の日時にする。日付だけの表記は 0 時 UTC とみなす。"""
    if value is None:
        return None
    if isinstance(value, int | float) or (isinstance(value, str) and value.isdigit()):
        seconds = float(value)
        if seconds > 1e11:  # ミリ秒
            seconds /= 1000
        return dt.datetime.fromtimestamp(seconds, tz=dt.UTC)
    try:
        parsed = dt.datetime.fromisoformat(str(value))
    except ValueError:
        try:
            parsed = dt.datetime.combine(dt.date.fromisoformat(str(value)[:10]), dt.time())
        except ValueError:
            return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.UTC)
    return parsed.astimezone(dt.UTC)


def _float(value: Any) -> float | None:
    try:
        return float(value) if value is not None else None
    except TypeError, ValueError:
        return None


_US_EASTERN = dt.timezone(dt.timedelta(hours=-5), "US/Eastern(approx)")
