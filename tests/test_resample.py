"""細かい足からの合成と、二層構造の供給。

確かめること:
- OHLCV の合成規則（始値＝最初、高値＝最大、安値＝最小、終値＝最後、出来高＝合計）
- 区切りが取引所の寄付に揃う（NYSE 09:30 開始の 1 時間足は 09:30, 10:30, …）
- 日足は暦日で切れる
- 粗い→細かい、割り切れない組は弾く
- 形成中の最後の足を落とせる
- 供給層が「基準足から合成」と「直接取得」をつなぎ、重なりは直接取得を採る
- 戦略が判断中にマルチタイムフレームで粗い足を見られる
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import polars as pl
import pytest

from wbcore.data.feed import BarFeed
from wbcore.data.provider import Interval, MarketDataProvider
from wbcore.data.resample import can_resample, drop_forming, resample
from wbcore.data.store import BarStore
from wbcore.domain.models import Market
from wbjp.config import UniverseConfig
from wbjp.strategy.base import StrategyContext


def minute_bars(
    start: dt.datetime, n: int, *, step: dt.timedelta = dt.timedelta(minutes=1)
) -> pl.DataFrame:
    """1 本ごとに 1 ずつ上がる足。open=i, high=i+0.5, low=i-0.5, close=i+0.25, volume=1。"""
    stamps = [start + step * i for i in range(n)]
    return pl.DataFrame(
        {
            "ts": stamps,
            "open": [float(i) for i in range(n)],
            "high": [i + 0.5 for i in range(n)],
            "low": [i - 0.5 for i in range(n)],
            "close": [i + 0.25 for i in range(n)],
            "volume": [1.0] * n,
        }
    ).with_columns(pl.col("ts").dt.date().alias("date"))


JP_OPEN = dt.datetime(2026, 8, 3, 0, 0, tzinfo=dt.UTC)  # 09:00 JST
US_OPEN = dt.datetime(2026, 8, 3, 13, 30, tzinfo=dt.UTC)  # 09:30 ET（夏時間）


# --- 合成規則 --------------------------------------------------------------


def test_five_minute_bars_aggregate_ohlcv() -> None:
    out = resample(minute_bars(JP_OPEN, 10), Interval.M1, Interval.M5, market=Market.JP)
    assert out.height == 2
    first = out.row(0, named=True)
    assert first["ts"] == JP_OPEN
    assert first["open"] == 0.0
    assert first["high"] == 4.5
    assert first["low"] == -0.5
    assert first["close"] == 4.25
    assert first["volume"] == 5.0
    assert first["date"] == dt.date(2026, 8, 3)


def test_hourly_buckets_align_to_the_exchange_open() -> None:
    """NYSE は 09:30 開始。毎正時で切ると先頭に半端な足ができる。"""
    out = resample(minute_bars(US_OPEN, 150), Interval.M1, Interval.H1, market=Market.US)
    assert out["ts"].to_list()[:3] == [
        US_OPEN,
        US_OPEN + dt.timedelta(hours=1),
        US_OPEN + dt.timedelta(hours=2),
    ]
    assert out["volume"].to_list() == [60.0, 60.0, 30.0]


def test_daily_bars_are_cut_by_calendar_day() -> None:
    day1 = minute_bars(JP_OPEN, 3)
    day2 = minute_bars(JP_OPEN + dt.timedelta(days=1), 3)
    out = resample(pl.concat([day1, day2]), Interval.M1, Interval.D1, market=Market.JP)
    assert list(out.columns) == ["date", "open", "high", "low", "close", "volume"]
    assert out["date"].to_list() == [dt.date(2026, 8, 3), dt.date(2026, 8, 4)]
    assert out["volume"].to_list() == [3.0, 3.0]


def test_resampling_is_causal_and_order_independent() -> None:
    bars = minute_bars(JP_OPEN, 10)
    shuffled = bars.sample(fraction=1.0, shuffle=True, seed=1)
    assert resample(shuffled, Interval.M1, Interval.M5, market=Market.JP).equals(
        resample(bars, Interval.M1, Interval.M5, market=Market.JP)
    )


def test_unsupported_pairs_are_rejected() -> None:
    assert can_resample(Interval.M5, Interval.H1)
    assert can_resample(Interval.M1, Interval.D1)
    assert not can_resample(Interval.H1, Interval.M5)
    assert not can_resample(Interval.D1, Interval.H1)
    assert can_resample(Interval.M15, Interval.H1) is not False  # 15 → 60 は割り切れる
    with pytest.raises(ValueError, match="合成できません"):
        resample(minute_bars(JP_OPEN, 2), Interval.H1, Interval.M5, market=Market.JP)


def test_forming_bar_is_dropped_until_it_closes() -> None:
    out = resample(minute_bars(JP_OPEN, 7), Interval.M1, Interval.M5, market=Market.JP)
    assert out.height == 2
    at_0006 = JP_OPEN + dt.timedelta(minutes=6)
    assert drop_forming(out, Interval.M5, at_0006, market=Market.JP).height == 1
    at_0010 = JP_OPEN + dt.timedelta(minutes=10)
    assert drop_forming(out, Interval.M5, at_0010, market=Market.JP).height == 2
    daily = resample(minute_bars(JP_OPEN, 7), Interval.M1, Interval.D1, market=Market.JP)
    assert drop_forming(daily, Interval.D1, at_0010, market=Market.JP).height == 0


# --- 供給層 ----------------------------------------------------------------


def test_feed_merges_derived_recent_bars_with_native_history(tmp_path: Path) -> None:
    feed = BarFeed(tmp_path, Market.JP, base=Interval.M1)
    # 直接取得の日足: 8/1（金曜ではないが検証には十分）と 8/3
    BarStore(tmp_path).write(
        "X",
        pl.DataFrame(
            {
                "date": [dt.date(2026, 7, 31), dt.date(2026, 8, 3)],
                "open": [1.0, 999.0],
                "high": [1.0, 999.0],
                "low": [1.0, 999.0],
                "close": [1.0, 999.0],
                "volume": [1.0, 1.0],
            }
        ),
    )
    # 基準足: 8/3 と 8/4
    BarStore(tmp_path, Interval.M1).write(
        "X", pl.concat([minute_bars(JP_OPEN, 3), minute_bars(JP_OPEN + dt.timedelta(days=1), 3)])
    )
    out = feed.read(["X"], Interval.D1)["X"]
    assert out["date"].to_list() == [dt.date(2026, 7, 31), dt.date(2026, 8, 3), dt.date(2026, 8, 4)]
    # 重なる 8/3 は直接取得（分割調整済み）を採る
    assert out.filter(pl.col("date") == dt.date(2026, 8, 3))["close"][0] == 999.0
    # 8/4 は基準足から合成
    assert out.filter(pl.col("date") == dt.date(2026, 8, 4))["close"][0] == 2.25


def test_feed_without_base_reads_the_native_store_only(tmp_path: Path) -> None:
    BarStore(tmp_path, Interval.M1).write("X", minute_bars(JP_OPEN, 3))
    assert BarFeed(tmp_path, Market.JP).read(["X"], Interval.M5) == {}
    assert BarFeed(tmp_path, Market.JP, base=Interval.M1).read(["X"], Interval.M5)["X"].height == 1


def test_feed_sync_fetches_base_and_native(tmp_path: Path) -> None:
    class Both(MarketDataProvider):
        name = "both"
        intervals = frozenset({Interval.M1, Interval.M5})

        def fetch_bars(self, symbols, start, end, *, interval=Interval.D1):  # type: ignore[no-untyped-def]
            self._require(interval)
            frame = minute_bars(JP_OPEN, 10, step=interval.duration)
            return {s: frame for s in symbols}

    feed = BarFeed(tmp_path, Market.JP, base=Interval.M1)
    counts = feed.sync(Both(), ["X"], dt.date(2026, 8, 1), dt.date(2026, 8, 3), Interval.M5)
    assert counts == {"X": 10}  # 直接取得の 5 分足 10 本（合成分 2 本は重なりで置き換わる）
    assert feed.store(Interval.M1).symbols() == ["X"]
    assert feed.store(Interval.M5).symbols() == ["X"]


# --- 設定と戦略 --------------------------------------------------------------


def test_config_requires_base_finer_than_interval() -> None:
    assert (
        UniverseConfig(symbols=["X"], interval="5m", base_interval="1m").base_bar_interval
        is Interval.M1
    )
    with pytest.raises(ValueError, match="合成できません"):
        UniverseConfig(symbols=["X"], interval="5m", base_interval="1h")
    with pytest.raises(ValueError, match="合成できません"):
        UniverseConfig(symbols=["X"], interval="5m", base_interval="5m")


def test_strategy_context_resamples_visible_bars_causally() -> None:
    bars = minute_bars(JP_OPEN, 12, step=dt.timedelta(minutes=5))  # 5 分足 × 12 = 1 時間
    at = bars["ts"][-1]
    ctx = StrategyContext(
        as_of=at.date(),
        _bars={"X": bars},
        equity=Decimal(0),
        interval=Interval.M5,
        at=at,
        market=Market.JP,
    )
    hourly = ctx.resample("X", Interval.H1, completed_only=False)
    assert hourly.height == 1 and hourly["volume"][0] == 12.0
    # 最後の 1 時間足は 09:55 の足まででまだ閉じていない（閉じるのは 10:00）
    assert ctx.resample("X", Interval.H1).height == 0
    assert ctx.resample("X", Interval.M5).equals(bars)
    with pytest.raises(ValueError, match="合成できません"):
        ctx.resample("X", Interval.M1)
