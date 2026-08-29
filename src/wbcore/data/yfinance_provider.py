"""yfinance から足を取得する（日本株・米国株、日足と日中足）。

注意点:
    - **非公式**。Yahoo Finance をスクレイピングしているため、予告なく
      壊れることがある。壊れたときに戦略側へ影響が出ないよう、
      :class:`~wbcore.data.provider.MarketDataProvider` の裏に隠してある。
    - **短時間に連投すると IP 単位で弾かれる。** 取得済みの足は必ず
      :class:`~wbcore.data.store.BarStore` にキャッシュし、増分だけ取りに行く。
    - 東証銘柄は ``7203`` ではなく ``7203.T`` で問い合わせる。この変換を
      忘れると米国の別銘柄が返ってくることがあり、実害が大きい。
      米国株（``market=US``）は接尾辞を付けずにそのまま渡す。
    - 株式分割を反映した価格を使う（``auto_adjust=True``）。未調整のまま
      バックテストすると、分割日に巨大な偽の下落が現れる。
    - **日中足は遡れる期間が短い。** 1分足は直近 7 日、5〜30分足は 60 日、
      1時間足は 730 日。それより前を要求されたら黙って切らずに警告し、
      取れる範囲に丸める。
"""

from __future__ import annotations

import datetime as dt
from typing import Any, ClassVar, Self

import polars as pl
from tenacity import retry, retry_if_exception_type, stop_after_attempt, wait_exponential

from wbcore.clock import today_utc
from wbcore.credentials import Environment
from wbcore.data.provider import Interval, MarketDataError, MarketDataProvider, normalize_bars
from wbcore.domain.models import Market
from wbcore.logging import get_logger

log = get_logger(__name__)

#: 東証銘柄の接尾辞。
TSE_SUFFIX = ".T"

#: 日中足を遡れる日数（yfinance の制限）。日足は制限なし。
MAX_LOOKBACK_DAYS: dict[Interval, int] = {
    Interval.M1: 7,
    Interval.M5: 60,
    Interval.M15: 60,
    Interval.M30: 60,
    Interval.H1: 730,
}


def to_yahoo_ticker(symbol: str, market: Market = Market.JP) -> str:
    """銘柄コードを Yahoo Finance の表記に変換する。

    日本株はすでに接尾辞が付いていればそのまま返すので、``7203`` と
    ``7203.T`` のどちらを渡しても動く。``^`` で始まる指数のティッカーは
    東証銘柄ではないので、接尾辞を付けずにそのまま通す。
    米国株は Yahoo もティッカーそのままなので変換しない（``BRK.B`` は
    Yahoo では ``BRK-B``。この置換だけ行う）。

    >>> to_yahoo_ticker("7203")
    '7203.T'
    >>> to_yahoo_ticker("7203.T")
    '7203.T'
    >>> to_yahoo_ticker("^N225")
    '^N225'
    >>> to_yahoo_ticker("AAPL", Market.US)
    'AAPL'
    >>> to_yahoo_ticker("BRK.B", Market.US)
    'BRK-B'
    """
    symbol = symbol.strip()
    if not symbol:
        raise ValueError("銘柄コードが空です")
    if symbol.startswith("^"):
        return symbol
    if market is Market.US:
        return symbol.replace(".", "-")
    return symbol if "." in symbol else f"{symbol}{TSE_SUFFIX}"


def from_yahoo_ticker(ticker: str) -> str:
    """Yahoo の表記から銘柄コードに戻す。"""
    return ticker.removesuffix(TSE_SUFFIX)


class YFinanceProvider(MarketDataProvider):
    """yfinance 実装。"""

    name: ClassVar[str] = "yfinance"
    intervals: ClassVar[frozenset[Interval]] = frozenset(Interval)

    def __init__(
        self, *, market: Market = Market.JP, max_attempts: int = 3, timeout: int = 30
    ) -> None:
        self.market = market
        self.max_attempts = max_attempts
        self.timeout = timeout

    @classmethod
    def connect(cls, env: Environment, *, market: Market) -> Self:
        """認証情報は要らない。環境（uat / prod）にも依存しない。"""
        return cls(market=market)

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
        start = self._clamp_start(start, end, interval)

        tickers = [to_yahoo_ticker(s, self.market) for s in symbols]
        log.info(
            "足を取得します",
            provider=self.name,
            interval=interval.value,
            symbols=len(tickers),
            start=str(start),
            end=str(end),
        )

        raw = self._download(tickers, start, end, interval)
        if raw is None or raw.empty:
            log.warning("足が取得できませんでした", symbols=tickers, interval=interval.value)
            return {}

        result: dict[str, pl.DataFrame] = {}
        for symbol, ticker in zip(symbols, tickers, strict=True):
            frame = self._extract(raw, ticker, interval)
            if frame is not None and frame.height > 0:
                result[symbol] = frame
            else:
                log.warning("銘柄の足が空でした", symbol=symbol, ticker=ticker)

        return result

    # -- 内部 ---------------------------------------------------------------

    @staticmethod
    def _clamp_start(start: dt.date, end: dt.date, interval: Interval) -> dt.date:
        """日中足の遡れる限界に丸める。切られたことは必ずログに出す。"""
        limit = MAX_LOOKBACK_DAYS.get(interval)
        if limit is None:
            return start
        earliest = today_utc() - dt.timedelta(days=limit - 1)
        if start >= earliest:
            return start
        log.warning(
            "yfinance の日中足はこれより前を取れません。開始日を丸めます",
            interval=interval.value,
            requested=str(start),
            earliest=str(earliest),
        )
        return min(earliest, end)

    def _download(
        self, tickers: list[str], start: dt.date, end: dt.date, interval: Interval
    ) -> Any:
        """yfinance を叩く。一時的な失敗はリトライする。"""
        import yfinance as yf

        @retry(
            stop=stop_after_attempt(self.max_attempts),
            wait=wait_exponential(multiplier=2, min=2, max=30),
            retry=retry_if_exception_type((OSError, ConnectionError, TimeoutError)),
            reraise=True,
        )
        def _call() -> Any:
            return yf.download(
                tickers,
                # yfinance の end は「その日を含まない」ため1日足す
                start=start.isoformat(),
                end=(end + dt.timedelta(days=1)).isoformat(),
                interval=interval.value,
                auto_adjust=True,
                actions=False,
                progress=False,
                threads=False,
                timeout=self.timeout,
            )

        try:
            return _call()
        except Exception as exc:
            raise MarketDataError(f"yfinance からの取得に失敗しました: {exc}") from exc

    def _extract(self, raw: Any, ticker: str, interval: Interval) -> pl.DataFrame | None:
        """yfinance の pandas DataFrame から1銘柄ぶんを取り出す。

        yfinance 1.6 は銘柄が1つでも ``(Price, Ticker)`` の MultiIndex を
        返す。銘柄数で列の形が変わると誤って別銘柄を掴む危険があるため、
        常に ``Ticker`` の階層を名前で指定して取り出す。

        日中足の索引は取引所のタイムゾーン付き ``Datetime``。
        :func:`normalize_bars` が UTC に揃える。
        """
        import pandas as pd

        try:
            if isinstance(raw.columns, pd.MultiIndex):
                level = "Ticker" if "Ticker" in (raw.columns.names or []) else 1
                frame = raw.xs(ticker, axis=1, level=level)
            else:
                frame = raw
        except KeyError:
            return None

        frame = frame.dropna(how="all")
        if frame.empty:
            return None

        renamed = frame.rename(
            columns={
                "Open": "open",
                "High": "high",
                "Low": "low",
                "Close": "close",
                "Volume": "volume",
            }
        ).reset_index()

        key = interval.time_column
        for candidate in ("Datetime", "Date"):
            if candidate in renamed.columns:
                renamed = renamed.rename(columns={candidate: key})
                break
        else:
            renamed = renamed.rename(columns={renamed.columns[0]: key})

        keep = [c for c in (key, "open", "high", "low", "close", "volume") if c in renamed]
        polars_frame = pl.from_pandas(renamed[keep])
        if key == "date" and polars_frame["date"].dtype != pl.Date:
            polars_frame = polars_frame.with_columns(pl.col("date").dt.date())

        return normalize_bars(polars_frame, interval)
