"""ロス・キャメロン流（ross_cameron）戦略のテスト。"""

from __future__ import annotations

import datetime as dt
import math
from decimal import Decimal

import polars as pl
import pytest

from wbcore.domain.models import Position
from wbjp.strategy.base import StrategyContext
from wbjp.strategy.registry import available, create
from wbjp.strategy.samples.ross_cameron import RossCameronStrategy

D = Decimal
N = 120  # ウォームアップ（3×EMA20 + 5 + 2 = 67）を十分に超える本数


# --------------------------------------------------------------------------
# 足の生成
# --------------------------------------------------------------------------


def _dates(n: int, start: dt.date = dt.date(2025, 1, 1)) -> list[dt.date]:
    out: list[dt.date] = []
    cur = start
    while len(out) < n:
        if cur.weekday() < 5:
            out.append(cur)
        cur += dt.timedelta(days=1)
    return out


class Bars:
    """OHLCV を1本ずつ足していくビルダー。"""

    def __init__(self) -> None:
        self.rows: list[tuple[float, float, float, float, float]] = []

    @property
    def last_close(self) -> float:
        return self.rows[-1][3]

    def add(
        self, open_: float, high: float, low: float, close: float, volume: float = 1_000_000.0
    ) -> Bars:
        self.rows.append((open_, high, low, close, volume))
        return self

    def frame(self) -> pl.DataFrame:
        o, h, lo, c, v = zip(*self.rows, strict=True)
        return pl.DataFrame(
            {
                "date": _dates(len(self.rows)),
                "open": list(o),
                "high": list(h),
                "low": list(lo),
                "close": list(c),
                "volume": list(v),
            }
        )


def gentle_uptrend(n: int = N, *, volume: float = 1_000_000.0) -> Bars:
    """静かな上昇トレンド（EMA9 > EMA20 を満たし、ギャップも RVOL も無い）。"""
    bars = Bars()
    for i in range(n):
        close = 100.0 * (1.0015**i) * (1 + 0.004 * math.sin(i / 4))
        open_ = close * 0.998
        bars.add(open_, close * 1.012, open_ * 0.99, close, volume)
    return bars


def gap_and_go_day(bars: Bars, *, gap: float = 0.05, rvol: float = 3.0) -> Bars:
    """前日終値から gap 上に寄り付き、高値引け寸前まで伸びる材料日。"""
    prev = bars.last_close
    open_ = prev * (1 + gap)
    high = open_ * 1.03
    close = high * 0.995  # 引け強度 ≈ 0.9
    return bars.add(open_, high, open_ * 0.995, close, 1_000_000.0 * rvol)


def spy_frame(n: int = N, *, above_sma: bool = True) -> pl.DataFrame:
    bars = Bars()
    for i in range(n):
        close = 400.0 * (1.001**i) if above_sma else 400.0 * (0.998**i)
        bars.add(close, close * 1.005, close * 0.995, close, 50_000_000.0)
    return bars.frame()


def ctx_for(
    frames: dict[str, pl.DataFrame], positions: dict[str, Position] | None = None
) -> StrategyContext:
    as_of = max(f["date"].max() for f in frames.values())  # type: ignore[type-var]
    assert isinstance(as_of, dt.date)
    return StrategyContext(as_of=as_of, _bars=frames, _positions=positions or {})


def position(symbol: str, cost: float) -> Position:
    return Position(
        symbol=symbol,
        quantity=D(10),
        available_quantity=D(10),
        cost_price=D(str(cost)),
        last_price=D(str(cost)),
    )


# --------------------------------------------------------------------------
# Gap & Go
# --------------------------------------------------------------------------


def test_gap_and_go_emits_buy_with_setup_meta() -> None:
    frames = {"X": gap_and_go_day(gentle_uptrend()).frame(), "SPY": spy_frame()}
    signals = RossCameronStrategy().on_bars(ctx_for(frames))
    assert len(signals) == 1
    sig = signals[0]
    assert sig.symbol == "X" and sig.direction >= 0.3
    assert sig.meta["setup"] == "Gap&Go"
    assert sig.meta["gap_pct"] == pytest.approx(0.05, abs=1e-6)
    assert sig.meta["rvol_x"] == pytest.approx(3.0, rel=0.05)
    assert "Gap&Go" in sig.reason


def test_no_signal_without_gap() -> None:
    frames = {"X": gentle_uptrend().frame(), "SPY": spy_frame()}
    assert RossCameronStrategy().on_bars(ctx_for(frames)) == []


def test_gap_without_volume_is_rejected() -> None:
    bars = gap_and_go_day(gentle_uptrend(), rvol=1.2)
    checks = RossCameronStrategy().screen(
        bars.frame().with_columns(RossCameronStrategy().indicators())
    )
    assert not checks.passed
    assert any("RVOL" in f for f in checks.failed)


def test_gap_that_fades_is_rejected() -> None:
    """寄付で飛んだが、レンジの下の方で引けた（ギャップを守れなかった）。"""
    bars = gentle_uptrend()
    prev = bars.last_close
    open_ = prev * 1.05
    high = open_ * 1.03
    bars.add(open_, high, open_ * 0.99, open_ * 1.001, 3_000_000.0)  # 引け強度 ≈ 0.27
    frames = {"X": bars.frame(), "SPY": spy_frame()}
    assert RossCameronStrategy().on_bars(ctx_for(frames)) == []


def test_benchmark_below_sma_blocks_entry() -> None:
    frames = {"X": gap_and_go_day(gentle_uptrend()).frame(), "SPY": spy_frame(above_sma=False)}
    assert RossCameronStrategy().on_bars(ctx_for(frames)) == []
    assert RossCameronStrategy(benchmark=None).on_bars(ctx_for(frames)) != []


def test_bigger_gap_and_volume_rank_higher() -> None:
    strong = gap_and_go_day(gentle_uptrend(), gap=0.08, rvol=6.0).frame()
    weak = gap_and_go_day(gentle_uptrend(), gap=0.035, rvol=2.2).frame()
    signals = RossCameronStrategy().on_bars(ctx_for({"S": strong, "W": weak, "SPY": spy_frame()}))
    by_symbol = {s.symbol: s.direction for s in signals}
    assert by_symbol["S"] > by_symbol["W"]


# --------------------------------------------------------------------------
# マイクロプルバック
# --------------------------------------------------------------------------


def micro_pullback(*, pullback_days: int = 2, break_prev_high: bool = True) -> Bars:
    """材料日 → 浅い押し目（EMA9 の上）→ 前日高値を出来高伴って上抜け。"""
    bars = gap_and_go_day(gentle_uptrend())
    peak = bars.last_close
    for k in range(pullback_days):
        close = peak * (1 - 0.008 * (k + 1))
        bars.add(close * 1.003, close * 1.006, close * 0.996, close, 600_000.0)
    prev_high = bars.rows[-1][1]
    close = prev_high * (1.02 if break_prev_high else 0.995)
    bars.add(prev_high * 0.99, close * 1.003, prev_high * 0.985, close, 1_500_000.0)
    return bars


def test_micro_pullback_emits_buy() -> None:
    frames = {"X": micro_pullback().frame(), "SPY": spy_frame()}
    signals = RossCameronStrategy().on_bars(ctx_for(frames))
    assert len(signals) == 1
    assert signals[0].meta["setup"] == "マイクロプルバック"
    # 材料日のギャップ / RVOL がスコアに使われる
    assert signals[0].meta["gap_pct"] == pytest.approx(0.05, abs=1e-6)


def test_micro_pullback_requires_break_of_prev_high() -> None:
    frames = {"X": micro_pullback(break_prev_high=False).frame(), "SPY": spy_frame()}
    assert RossCameronStrategy().on_bars(ctx_for(frames)) == []


def test_micro_pullback_can_be_disabled() -> None:
    frames = {"X": micro_pullback().frame(), "SPY": spy_frame()}
    assert RossCameronStrategy(allow_pullback_entry=False).on_bars(ctx_for(frames)) == []


def test_pullback_too_long_is_rejected() -> None:
    frames = {"X": micro_pullback(pullback_days=4).frame(), "SPY": spy_frame()}
    assert RossCameronStrategy(max_pullback_days=3).on_bars(ctx_for(frames)) == []


# --------------------------------------------------------------------------
# 手仕舞い
# --------------------------------------------------------------------------


def test_holding_above_ema_keeps_position() -> None:
    frames = {"X": gap_and_go_day(gentle_uptrend()).frame(), "SPY": spy_frame()}
    signals = RossCameronStrategy().on_bars(ctx_for(frames, {"X": position("X", 100.0)}))
    assert len(signals) == 1 and signals[0].direction > 0
    assert "保有継続" in signals[0].reason


def test_close_below_ema9_exits() -> None:
    bars = gap_and_go_day(gentle_uptrend())
    peak = bars.last_close
    close = peak * 0.85  # EMA9 を大きく割る
    bars.add(peak * 0.9, peak * 0.9, close * 0.99, close, 2_000_000.0)
    frames = {"X": bars.frame(), "SPY": spy_frame()}
    signals = RossCameronStrategy().on_bars(ctx_for(frames, {"X": position("X", 100.0)}))
    assert len(signals) == 1 and signals[0].direction == -1.0
    assert "EMA9" in signals[0].reason


def test_exit_on_prev_low_is_optional() -> None:
    bars = gap_and_go_day(gentle_uptrend())
    prev_low = bars.rows[-1][2]
    peak = bars.last_close
    close = prev_low * 0.995  # 前日安値は割るが EMA9 は割らない
    bars.add(peak, peak, close * 0.99, close, 1_000_000.0)
    frames = {"X": bars.frame(), "SPY": spy_frame()}
    held = {"X": position("X", 100.0)}
    assert RossCameronStrategy().on_bars(ctx_for(frames, held))[0].direction > 0
    sig = RossCameronStrategy(exit_on_prev_low=True).on_bars(ctx_for(frames, held))[0]
    assert sig.direction == -1.0 and "前日安値" in sig.reason


# --------------------------------------------------------------------------
# 登録・パラメータ
# --------------------------------------------------------------------------


def test_registered_under_its_own_name() -> None:
    assert "ross_cameron" in available()
    assert "rsi_pullback" in available(), "既存戦略を上書きしていない"
    strategy = create("ross_cameron", {"gap_min": 0.05, "rvol_min": 3.0})
    assert isinstance(strategy, RossCameronStrategy)
    assert strategy.gap_min == 0.05


@pytest.mark.parametrize(
    "params",
    [
        {"gap_min": 0.0},
        {"rvol_min": -1.0},
        {"close_strength": 1.5},
        {"ema_fast": 20, "ema_slow": 9},
        {"min_atr_ratio": 0.05, "max_atr_ratio": 0.01},
        {"min_price": 20.0, "max_price": 2.0},
        {"allow_gap_entry": False, "allow_pullback_entry": False},
        {"catalyst_lookback": 3, "max_pullback_days": 3},
    ],
)
def test_invalid_parameters_are_rejected(params: dict[str, object]) -> None:
    with pytest.raises(ValueError):
        RossCameronStrategy(**params)  # type: ignore[arg-type]


def test_survives_short_history() -> None:
    frames = {"X": gentle_uptrend(5).frame(), "SPY": spy_frame(5)}
    assert RossCameronStrategy().on_bars(ctx_for(frames)) == []
