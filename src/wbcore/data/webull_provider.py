"""Webull の市場データ API から米国株の日足を取得する。

Webull JP の市場データ API は ``US_STOCK`` / ``US_ETF`` にしか対応して
いない（日本株の足は返さない）。米国株ではこちらが一次情報源になる。
yfinance と違って規約上の心配が無く、ブローカーと同じ値を見られる。

制約:
    - 1回の呼び出しで最大 1200 本。日足なら約5年ぶん。
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
from typing import Any

import polars as pl

from wbcore.credentials import Credentials, Environment
from wbcore.data.provider import MarketDataError, MarketDataProvider, normalize_bars
from wbcore.domain.models import Market
from wbcore.logging import get_logger, harden_third_party_logging, register_secret

log = get_logger(__name__)

#: 1回の呼び出しで取れる最大本数（SDK のドキュメントより）。
MAX_BARS_PER_CALL = 1200

#: 米国株は株式と ETF で category が分かれる。設定で ETF を明示できない
#: ため、株式で空なら ETF として取り直す。
_US_CATEGORIES = ("US_STOCK", "US_ETF")


class WebullMarketDataProvider(MarketDataProvider):
    """Webull 市場データ API（米国株のみ）。"""

    name = "webull"

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
                'provider = "yfinance" を使ってください'
            )
        self._credentials = credentials
        self._env = env
        self._endpoint = endpoint
        self._client: Any = None
        register_secret(credentials.app_key, credentials.app_secret, credentials.account_id)

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
        client = DataClient(api_client)
        harden_third_party_logging()
        return client

    def fetch_daily_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
    ) -> dict[str, pl.DataFrame]:
        if not symbols:
            return {}
        if start > end:
            raise ValueError(f"start が end より後です: {start} > {end}")

        # 営業日ベースで必要本数を見積もる（週末ぶん 7/5 と祝日の余裕）
        count = min(MAX_BARS_PER_CALL, int((end - start).days * 5 / 7) + 10)
        result: dict[str, pl.DataFrame] = {}
        for symbol in symbols:
            frame = self._fetch_one(symbol, count)
            if frame is None or frame.height == 0:
                log.warning("銘柄の足が空でした", symbol=symbol, provider=self.name)
                continue
            result[symbol] = frame.filter((pl.col("date") >= start) & (pl.col("date") <= end))
        return result

    def _fetch_one(self, symbol: str, count: int) -> pl.DataFrame | None:
        for category in _US_CATEGORIES:
            try:
                response = self.client.market_data.get_history_bar(
                    symbol, category, "D1", count=str(count)
                )
            except Exception as exc:
                raise MarketDataError(f"Webull からの足取得に失敗しました: {symbol}") from exc

            status = getattr(response, "status_code", 200)
            if status != 200:
                raise MarketDataError(
                    f"Webull からの足取得に失敗しました: {symbol} (HTTP {status})"
                )
            frame = _parse_bars(response.json())
            if frame.height > 0:
                return frame
        return None


def _parse_bars(payload: Any) -> pl.DataFrame:
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
        return normalize_bars(
            pl.DataFrame(schema={"date": pl.Date()}).with_columns(
                [
                    pl.lit(None, dtype=pl.Float64).alias(c)
                    for c in ("open", "high", "low", "close", "volume")
                ]
            )
        )

    rows = []
    for bar in entries:
        if not isinstance(bar, dict):
            continue
        date = _parse_time(bar.get("time") or bar.get("timestamp") or bar.get("date"))
        if date is None:
            continue
        rows.append(
            {
                "date": date,
                "open": _float(bar.get("open")),
                "high": _float(bar.get("high")),
                "low": _float(bar.get("low")),
                "close": _float(bar.get("close")),
                "volume": _float(bar.get("volume")),
            }
        )
    if not rows:
        return normalize_bars(
            pl.DataFrame(
                {c: [] for c in ("date", "open", "high", "low", "close", "volume")},
                schema={
                    "date": pl.Date(),
                    "open": pl.Float64(),
                    "high": pl.Float64(),
                    "low": pl.Float64(),
                    "close": pl.Float64(),
                    "volume": pl.Float64(),
                },
            )
        )
    return normalize_bars(pl.DataFrame(rows))


def _parse_time(value: Any) -> dt.date | None:
    if value is None:
        return None
    if isinstance(value, int | float) or (isinstance(value, str) and value.isdigit()):
        seconds = float(value)
        if seconds > 1e11:  # ミリ秒
            seconds /= 1000
        # 米国の日足は米東部時間の取引日で解釈する
        return dt.datetime.fromtimestamp(seconds, tz=dt.UTC).astimezone(_US_EASTERN).date()
    try:
        return dt.datetime.fromisoformat(str(value)).date()
    except ValueError:
        try:
            return dt.date.fromisoformat(str(value)[:10])
        except ValueError:
            return None


def _float(value: Any) -> float | None:
    try:
        return float(value) if value is not None else None
    except TypeError, ValueError:
        return None


_US_EASTERN = dt.timezone(dt.timedelta(hours=-5), "US/Eastern(approx)")
