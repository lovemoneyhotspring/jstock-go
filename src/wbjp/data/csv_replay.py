"""CSV から足を供給するプロバイダ。

用途:
    - **決定論的なデバッグ**。ネットワークにも外部サービスにも依存せず、
      同じ入力から必ず同じ結果が出る。「あの日おかしな注文が出た」を
      再現するときに要る。
    - テストで特定の値動き（ストップ高、連続ギャップ、薄商い）を
      意図的に作り込む。

CSV の形式:
    ``date,open,high,low,close,volume`` のヘッダ付き。
    ファイル名が銘柄コードになる（``7203.csv`` → ``7203``）。
"""

from __future__ import annotations

import datetime as dt
from pathlib import Path

import polars as pl

from wbjp.data.provider import MarketDataProvider, normalize_bars


class CsvReplayProvider(MarketDataProvider):
    """ディレクトリ内の CSV を読む。"""

    name = "csv_replay"

    def __init__(self, directory: Path) -> None:
        self.directory = Path(directory)

    def available_symbols(self) -> list[str]:
        if not self.directory.exists():
            return []
        return sorted(p.stem for p in self.directory.glob("*.csv"))

    def fetch_daily_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
    ) -> dict[str, pl.DataFrame]:
        result: dict[str, pl.DataFrame] = {}

        for symbol in symbols:
            path = self.directory / f"{symbol}.csv"
            if not path.exists():
                continue

            frame = normalize_bars(
                pl.read_csv(path, try_parse_dates=True).with_columns(
                    pl.col("date").cast(pl.Date, strict=False)
                )
            ).filter((pl.col("date") >= start) & (pl.col("date") <= end))

            if frame.height > 0:
                result[symbol] = frame

        return result


class InMemoryProvider(MarketDataProvider):
    """あらかじめ用意した DataFrame を返す。テスト専用。"""

    name = "in_memory"

    def __init__(self, bars: dict[str, pl.DataFrame]) -> None:
        self._bars = {symbol: normalize_bars(frame) for symbol, frame in bars.items()}

    def fetch_daily_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
    ) -> dict[str, pl.DataFrame]:
        result = {}
        for symbol in symbols:
            frame = self._bars.get(symbol)
            if frame is None:
                continue
            windowed = frame.filter((pl.col("date") >= start) & (pl.col("date") <= end))
            if windowed.height > 0:
                result[symbol] = windowed
        return result
