"""日中足の無い期間を日足から見立てて補う。

確かめること:
- 分足が無い日は日足 1 本が「引けに閉じる 1 本の日中足」になり、synthetic = True
- 分足のある日は本物が使われ、synthetic = False
- 見立てた足を日足に再合成すると元の日足に戻る（規則が壊れていない）
- 見立ては保存されない
- fill_from_daily=False で無効化できる
- 取得元が基準足に対応しなくても、日足は必ず揃う
"""

from __future__ import annotations

import datetime as dt
from pathlib import Path

import polars as pl

from wbcore.data.feed import SYNTHETIC, BarFeed
from wbcore.data.provider import Interval, MarketDataProvider
from wbcore.data.resample import resample
from wbcore.data.store import BarStore
from wbcore.domain.models import Market

JP_OPEN = dt.datetime(2026, 8, 3, 0, 0, tzinfo=dt.UTC)  # 09:00 JST


def _daily(dates: list[dt.date]) -> pl.DataFrame:
    n = len(dates)
    return pl.DataFrame(
        {
            "date": dates,
            "open": [100.0 + i for i in range(n)],
            "high": [105.0 + i for i in range(n)],
            "low": [95.0 + i for i in range(n)],
            "close": [102.0 + i for i in range(n)],
            "volume": [1000.0 * (i + 1) for i in range(n)],
        }
    )


def _minutes(start: dt.datetime, n: int) -> pl.DataFrame:
    stamps = [start + dt.timedelta(minutes=i) for i in range(n)]
    return pl.DataFrame(
        {
            "ts": stamps,
            "open": [1.0] * n,
            "high": [2.0] * n,
            "low": [0.5] * n,
            "close": [1.5] * n,
            "volume": [1.0] * n,
        }
    ).with_columns(pl.col("ts").dt.date().alias("date"))


def test_days_without_intraday_bars_are_filled_from_daily(tmp_path: Path) -> None:
    dates = [dt.date(2026, 7, 30), dt.date(2026, 7, 31), dt.date(2026, 8, 3)]
    BarStore(tmp_path).write("X", _daily(dates))
    BarStore(tmp_path, Interval.M1).write("X", _minutes(JP_OPEN, 3))  # 8/3 だけ本物

    out = BarFeed(tmp_path, Market.JP, base=Interval.M1).read(["X"], Interval.M5)["X"]
    assert SYNTHETIC in out.columns
    by_date = {d: f for d, f in out.group_by("date")}
    synthetic = out.filter(pl.col(SYNTHETIC))
    real = out.filter(~pl.col(SYNTHETIC))
    assert synthetic["date"].to_list() == [dt.date(2026, 7, 30), dt.date(2026, 7, 31)]
    assert real["date"].unique().to_list() == [dt.date(2026, 8, 3)]
    # 見立てた足は引け（15:30 JST = 06:30 UTC）に閉じる 5 分足 → 06:25 UTC
    assert synthetic["ts"][0] == dt.datetime(2026, 7, 30, 6, 25, tzinfo=dt.UTC)
    assert synthetic.row(0, named=True)["high"] == 105.0
    assert out["ts"].is_sorted()
    assert by_date  # group_by が動くこと（列が壊れていない）


def test_synthetic_bars_resample_back_to_the_original_daily(tmp_path: Path) -> None:
    dates = [dt.date(2026, 7, 30), dt.date(2026, 7, 31)]
    BarStore(tmp_path).write("X", _daily(dates))
    out = BarFeed(tmp_path, Market.JP, base=Interval.M1).read(["X"], Interval.M5)["X"]
    back = resample(out.drop(SYNTHETIC), Interval.M5, Interval.D1, market=Market.JP)
    assert back.select("date", "open", "high", "low", "close", "volume").equals(_daily(dates))


def test_fill_can_be_disabled_and_is_never_persisted(tmp_path: Path) -> None:
    BarStore(tmp_path).write("X", _daily([dt.date(2026, 7, 30)]))
    feed = BarFeed(tmp_path, Market.JP, base=Interval.M1)
    assert feed.read(["X"], Interval.M5, fill_from_daily=False) == {}
    assert feed.read(["X"], Interval.M5)["X"].height == 1
    assert feed.store(Interval.M5).symbols() == []
    assert feed.store(Interval.M1).symbols() == []


def test_daily_requests_are_untouched_by_the_fill(tmp_path: Path) -> None:
    BarStore(tmp_path).write("X", _daily([dt.date(2026, 7, 30)]))
    out = BarFeed(tmp_path, Market.JP, base=Interval.M1).read(["X"], Interval.D1)["X"]
    assert SYNTHETIC not in out.columns


def test_sync_always_fetches_daily_even_when_source_has_no_intraday(tmp_path: Path) -> None:
    class DailyOnly(MarketDataProvider):
        name = "daily_only"
        intervals = frozenset({Interval.D1})

        def fetch_bars(self, symbols, start, end, *, interval=Interval.D1):  # type: ignore[no-untyped-def]
            self._require(interval)
            return {s: _daily([dt.date(2026, 7, 30), dt.date(2026, 7, 31)]) for s in symbols}

    feed = BarFeed(tmp_path, Market.JP, base=Interval.M1)
    counts = feed.sync(DailyOnly(), ["X"], dt.date(2026, 7, 1), dt.date(2026, 8, 1), Interval.M5)
    assert counts == {"X": 0}  # 本物の 5 分足は無い
    assert feed.store(Interval.D1).symbols() == ["X"]
    filled = feed.read(["X"], Interval.M5)["X"]
    assert filled.height == 2 and filled[SYNTHETIC].all()
