"""yfinance から日足を取得する（日本株・米国株）。

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
"""

from __future__ import annotations

import datetime as dt
from typing import Any

import polars as pl
from tenacity import retry, retry_if_exception_type, stop_after_attempt, wait_exponential

from wbcore.data.provider import MarketDataError, MarketDataProvider, normalize_bars
from wbcore.domain.models import Market
from wbcore.logging import get_logger

log = get_logger(__name__)

#: 東証銘柄の接尾辞。
TSE_SUFFIX = ".T"


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

    name = "yfinance"

    def __init__(
        self, *, market: Market = Market.JP, max_attempts: int = 3, timeout: int = 30
    ) -> None:
        self.market = market
        self.max_attempts = max_attempts
        self.timeout = timeout

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

        tickers = [to_yahoo_ticker(s, self.market) for s in symbols]
        log.info(
            "日足を取得します",
            provider=self.name,
            symbols=len(tickers),
            start=str(start),
            end=str(end),
        )

        raw = self._download(tickers, start, end)
        if raw is None or raw.empty:
            log.warning("日足が取得できませんでした", symbols=tickers)
            return {}

        result: dict[str, pl.DataFrame] = {}
        for symbol, ticker in zip(symbols, tickers, strict=True):
            frame = self._extract(raw, ticker)
            if frame is not None and frame.height > 0:
                result[symbol] = frame
            else:
                log.warning("銘柄の足が空でした", symbol=symbol, ticker=ticker)

        return result

    # -- 内部 ---------------------------------------------------------------

    def _download(self, tickers: list[str], start: dt.date, end: dt.date) -> Any:
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
                interval="1d",
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

    def _extract(self, raw: Any, ticker: str) -> pl.DataFrame | None:
        """yfinance の pandas DataFrame から1銘柄ぶんを取り出す。

        yfinance 1.6 は銘柄が1つでも ``(Price, Ticker)`` の MultiIndex を
        返す。銘柄数で列の形が変わると誤って別銘柄を掴む危険があるため、
        常に ``Ticker`` の階層を名前で指定して取り出す。
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

        date_column = "Date" if "Date" in renamed.columns else renamed.columns[0]
        renamed = renamed.rename(columns={date_column: "date"})

        keep = [c for c in ("date", "open", "high", "low", "close", "volume") if c in renamed]
        pdf = renamed[keep]

        polars_frame = pl.from_pandas(pdf)
        if polars_frame["date"].dtype != pl.Date:
            polars_frame = polars_frame.with_columns(pl.col("date").dt.date())

        return normalize_bars(polars_frame)
