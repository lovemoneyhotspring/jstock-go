"""モメンタム順位戦略のテスト。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import polars as pl
import pytest

from wbjp.config import load_file_config
from wbjp.domain.models import Position
from wbjp.strategy.base import StrategyContext
from wbjp.strategy.registry import create
from wbjp.strategy.samples.momentum_rank import MomentumRankStrategy

D = Decimal
N = 300


def _dates(n: int, start: dt.date = dt.date(2025, 1, 1)) -> list[dt.date]:
    out, cur = [], start
    while len(out) < n:
        if cur.weekday() < 5:
            out.append(cur)
        cur += dt.timedelta(days=1)
    return out


def series(daily: float, *, n: int = N, noise: float = 0.0, start: float = 100.0) -> list[float]:
    import math

    return [round(start * (1 + daily) ** i * (1 + noise * math.sin(i / 3)), 4) for i in range(n)]


def frame_from(
    closes: list[float], *, volume: float = 3_000_000.0, dates: list[dt.date] | None = None
) -> pl.DataFrame:
    n = len(closes)
    opens = [closes[0], *closes[:-1]]
    return pl.DataFrame(
        {
            "date": dates or _dates(n),
            "open": opens,
            "high": [max(o, c) * 1.01 for o, c in zip(opens, closes, strict=True)],
            "low": [min(o, c) * 0.99 for o, c in zip(opens, closes, strict=True)],
            "close": closes,
            "volume": [volume] * n,
        }
    )


def month_start_dates(n: int = N) -> list[dt.date]:
    """最終日が月初の営業日になる日付列。"""
    dates = _dates(n + 40)
    for i in range(len(dates) - 1, 0, -1):
        if dates[i].month != dates[i - 1].month:
            return dates[i - n + 1 : i + 1]
    raise AssertionError


def bars(**kw) -> dict[str, pl.DataFrame]:  # type: ignore[no-untyped-def]
    """既定: SPY は上昇、STRONG > MILD > SPY、WEAK は下落。最終日は月初。"""
    dates = kw.pop("dates", month_start_dates())
    base = {
        "SPY": series(0.0008),
        "STRONG": series(0.003),
        "MILD": series(0.0015),
        "WEAK": series(-0.001),
    }
    base.update(kw)
    return {s: frame_from(c, dates=dates) for s, c in base.items()}


def context(
    frames: dict[str, pl.DataFrame], positions: dict[str, Position] | None = None
) -> StrategyContext:
    as_of = max(f["date"].max() for f in frames.values())  # type: ignore[type-var]
    return StrategyContext(as_of=as_of, _bars=frames, _positions=positions or {})  # type: ignore[arg-type]


def held(*symbols: str) -> dict[str, Position]:
    return {s: Position(s, D(10), D(10), D(100), D(100), currency="USD") for s in symbols}


def strategy(**kw) -> MomentumRankStrategy:  # type: ignore[no-untyped-def]
    return MomentumRankStrategy(**{"min_dollar_volume": 1_000_000.0, **kw})


# --------------------------------------------------------------------------


def test_registered() -> None:
    assert isinstance(create("momentum_rank"), MomentumRankStrategy)


def test_ranks_by_risk_adjusted_momentum_on_rebalance_day() -> None:
    signals = {s.symbol: s for s in strategy().on_bars(context(bars()))}
    assert set(signals) == {"STRONG", "MILD"}  # WEAK は落選、SPY は対象外
    assert signals["STRONG"].direction == 1.0
    assert signals["STRONG"].meta["rank"] == 1
    assert signals["MILD"].meta["rank"] == 2
    assert 0.3 <= signals["MILD"].direction < 1.0


def test_no_entries_outside_rebalance_day() -> None:
    dates = month_start_dates()
    # 最終日を月初の翌営業日にずらす
    frames = bars(dates=[*dates[1:], dates[-1] + dt.timedelta(days=1)])
    assert strategy().on_bars(context(frames)) == []
    assert strategy(rebalance="daily").on_bars(context(frames))


def test_weekly_rebalance_triggers_on_week_change() -> None:
    dates = _dates(N)
    # 最終日が月曜になるよう調整
    while dates[-1].weekday() != 0:
        dates = [*dates[1:], dates[-1] + dt.timedelta(days=1)]
        while dates[-1].weekday() >= 5:
            dates[-1] += dt.timedelta(days=1)
    frames = bars(dates=dates)
    assert strategy(rebalance="weekly").on_bars(context(frames))


def test_market_filter_blocks_entries() -> None:
    frames = bars(SPY=series(-0.002))
    assert strategy().on_bars(context(frames)) == []


def test_market_off_exits_every_holding() -> None:
    frames = bars(SPY=series(-0.002))
    signals = {s.symbol: s for s in strategy().on_bars(context(frames, held("STRONG")))}
    assert signals["STRONG"].direction == -1.0
    assert "地合い" in signals["STRONG"].reason


def test_must_beat_benchmark() -> None:
    frames = bars(MILD=series(0.0005))  # SPY(0.0008) より弱い
    signals = {s.symbol for s in strategy().on_bars(context(frames))}
    assert signals == {"STRONG"}


def test_skip_month_excludes_recent_spike() -> None:
    """直近1ヶ月だけ急騰した銘柄は 6M(直近除く) がゼロ近辺で順位に乗らない。"""
    flat = [100.0] * (N - 21) + series(0.02, n=21, start=100.0)
    frames = bars(SPIKE=flat)
    signals = {s.symbol for s in strategy().on_bars(context(frames))}
    assert "SPIKE" not in signals


def test_holds_between_rebalances_and_exits_on_trend_break() -> None:
    frames = bars()
    signals = {s.symbol: s for s in strategy().on_bars(context(frames, held("MILD")))}
    assert signals["MILD"].direction == 0.5

    broken = series(0.003, n=N - 30) + series(-0.01, n=30, start=series(0.003, n=N - 30)[-1])
    frames = bars(STRONG=broken)
    signals = {s.symbol: s for s in strategy().on_bars(context(frames, held("STRONG")))}
    assert signals["STRONG"].direction == -1.0
    assert "トレンド崩れ" in signals["STRONG"].reason


def test_rank_dropout_exits_only_on_rebalance_day() -> None:
    # 上位 1×1 位だけ保持。MILD は 2 位なので脱落
    s = strategy(top_n=1, keep_multiple=1)
    frames = bars()
    signals = {x.symbol: x for x in s.on_bars(context(frames, held("MILD")))}
    assert signals["MILD"].direction == -1.0
    assert "順位脱落" in signals["MILD"].reason

    dates = month_start_dates()
    frames = bars(dates=[*dates[1:], dates[-1] + dt.timedelta(days=1)])
    signals = {x.symbol: x for x in s.on_bars(context(frames, held("MILD")))}
    assert signals["MILD"].direction == 0.5


def test_illiquid_and_volatile_are_screened_out() -> None:
    frames = bars()
    frames["STRONG"] = frame_from(series(0.003), volume=1_000.0, dates=month_start_dates())
    frames["MILD"] = frame_from(series(0.0015, noise=0.08), dates=month_start_dates())
    assert strategy().on_bars(context(frames)) == []


def test_missing_benchmark_blocks_rather_than_guessing() -> None:
    frames = bars()
    del frames["SPY"]
    assert strategy().on_bars(context(frames)) == []
    assert strategy(benchmark=None).on_bars(context(frames))


def test_parameter_validation() -> None:
    with pytest.raises(ValueError):
        MomentumRankStrategy(lookback=126, skip=21, long_lookback=100)
    with pytest.raises(ValueError):
        MomentumRankStrategy(rebalance="yearly")


def test_shipped_momentum_config_loads() -> None:
    config = load_file_config(Path("config/us-momentum"))
    assert config.strategies.enabled[0].name == "momentum_rank"
    assert config.stops.trailing is True
    assert config.stops.max_hold_days is None
    assert config.sizing.atr_stop_multiple == D("3.0")
    assert "SPY" in config.universe.symbols


# --------------------------------------------------------------------------
# 受け皿（core_symbol）
# --------------------------------------------------------------------------


def test_core_fills_empty_slots_at_the_lowest_rank() -> None:
    signals = {s.symbol: s for s in strategy(top_n=3, core_symbol="SPY").on_bars(context(bars()))}
    # 候補は STRONG / MILD の2つ → 3枠目を SPY が埋める
    assert set(signals) == {"STRONG", "MILD", "SPY"}
    assert signals["SPY"].direction == 0.3
    assert signals["SPY"].meta["core"] is True
    assert signals["SPY"].direction < signals["MILD"].direction


def test_core_is_not_added_when_candidates_fill_the_slots() -> None:
    signals = {s.symbol for s in strategy(top_n=2, core_symbol="SPY").on_bars(context(bars()))}
    assert signals == {"STRONG", "MILD"}


def test_core_is_held_until_real_candidates_can_replace_it() -> None:
    s = strategy(top_n=3, core_symbol="SPY")
    held_core = held("SPY")
    signals = {x.symbol: x for x in s.on_bars(context(bars(), held_core))}
    assert signals["SPY"].direction == 0.5  # 候補2つでは枠が埋まらない → 保持

    s2 = strategy(top_n=2, core_symbol="SPY")
    signals = {x.symbol: x for x in s2.on_bars(context(bars(), held_core))}
    assert signals["SPY"].direction == -1.0
    assert "受け皿を解除" in signals["SPY"].reason


def test_core_exits_when_market_turns_off() -> None:
    frames = bars(SPY=series(-0.002))
    signals = {
        x.symbol: x for x in strategy(core_symbol="SPY").on_bars(context(frames, held("SPY")))
    }
    assert signals["SPY"].direction == -1.0


def test_shipped_etf_rotation_config_loads() -> None:
    config = load_file_config(Path("config/us-etf-rotation"))
    assert config.strategies.enabled[0].params["core_symbol"] == "SPY"
    assert config.sizing.method == "equal_weight"
