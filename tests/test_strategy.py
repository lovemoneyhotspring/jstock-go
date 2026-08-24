"""戦略層のテスト。

重点:
    - 戦略が未来の足を見られないこと（先読みバイアス）
    - 「意見を言わない」と「中立だと主張する」が区別されること
    - ウォームアップ不足で落ちないこと
"""

from __future__ import annotations

import datetime as dt
import math

import polars as pl
import pytest

from wbjp.domain.models import Signal
from wbjp.strategy.base import (
    IndicatorStrategy,
    InsufficientBarsError,
    Strategy,
    StrategyContext,
)
from wbjp.strategy.combiner import (
    MajorityVoteCombiner,
    PriorityCombiner,
    VetoCombiner,
    WeightedVoteCombiner,
    build_combiner,
)
from wbjp.strategy.registry import available, build_all, create, get, register
from wbjp.strategy.samples.atr_breakout import AtrBreakoutStrategy
from wbjp.strategy.samples.rsi_reversion import RsiReversionStrategy
from wbjp.strategy.samples.sma_cross import SmaCrossStrategy


def make_bars(closes: list[float], start: dt.date = dt.date(2025, 1, 1)) -> pl.DataFrame:
    return pl.DataFrame(
        {
            "date": [start + dt.timedelta(days=i) for i in range(len(closes))],
            "open": closes,
            "high": [round(c * 1.01, 4) for c in closes],
            "low": [round(c * 0.99, 4) for c in closes],
            "close": closes,
            "volume": [1_000_000.0] * len(closes),
        }
    )


def wavy(n: int, *, period: float = 17.0) -> list[float]:
    """再現性のある擬似的な値動き。"""
    out, price = [], 1000.0
    for i in range(n):
        price *= 1 + 0.012 * math.sin(i / period) + 0.006 * math.cos(i / 3.1)
        out.append(round(price, 2))
    return out


def ctx_for(bars: dict[str, pl.DataFrame], **kwargs: object) -> StrategyContext:
    as_of = max(frame["date"][-1] for frame in bars.values())
    return StrategyContext(as_of=as_of, _bars=bars, **kwargs)  # type: ignore[arg-type]


# --------------------------------------------------------------------------
# StrategyContext
# --------------------------------------------------------------------------


def test_context_exposes_only_provided_symbols() -> None:
    ctx = ctx_for({"7203": make_bars(wavy(30))})
    assert ctx.symbols == ["7203"]


def test_context_missing_symbol_error_lists_alternatives() -> None:
    ctx = ctx_for({"7203": make_bars(wavy(30))})
    with pytest.raises(KeyError, match="7203"):
        ctx.bars("9999")


def test_context_latest_returns_final_bar() -> None:
    closes = wavy(30)
    ctx = ctx_for({"7203": make_bars(closes)})
    assert ctx.latest("7203")["close"] == closes[-1]


def test_context_latest_raises_on_empty_bars() -> None:
    empty = make_bars([]).clear()
    ctx = StrategyContext(as_of=dt.date(2025, 1, 1), _bars={"7203": empty})
    with pytest.raises(InsufficientBarsError):
        ctx.latest("7203")


def test_context_has_bars_respects_minimum() -> None:
    ctx = ctx_for({"7203": make_bars(wavy(10))})
    assert ctx.has_bars("7203", 10) is True
    assert ctx.has_bars("7203", 11) is False
    assert ctx.has_bars("9999", 1) is False


def test_context_cannot_see_beyond_as_of() -> None:
    """コンテキストが持つ足は as_of までに切り詰められている。

    戦略が未来を覗く手段が存在しないことを、この不変条件で担保する。
    """
    closes = wavy(100)
    full = make_bars(closes)
    as_of = full["date"][49]
    truncated = full.filter(pl.col("date") <= as_of)

    ctx = StrategyContext(as_of=as_of, _bars={"7203": truncated})

    assert ctx.bars("7203").height == 50
    assert ctx.bars("7203")["date"].max() == as_of
    assert ctx.latest("7203")["close"] == closes[49]


# --------------------------------------------------------------------------
# Strategy 基底
# --------------------------------------------------------------------------


def test_strategy_subclass_without_name_is_rejected() -> None:
    """設定ファイルから引けない戦略を、定義した時点で弾く。"""
    with pytest.raises(TypeError, match="name"):

        class Nameless(Strategy):
            def on_bars(self, ctx: StrategyContext) -> list[Signal]:
                return []


def test_abstract_intermediate_class_may_omit_name() -> None:
    class Middle(Strategy, abstract=True):
        pass

    assert Middle.name == ""


def test_indicator_strategy_skips_symbols_without_enough_bars() -> None:
    """ウォームアップ不足の銘柄は例外ではなくスキップ。"""

    class NeedsFifty(IndicatorStrategy):
        name = "needs_fifty"
        warmup_bars = 50

        def evaluate(self, symbol, frame, ctx):  # type: ignore[no-untyped-def]
            return Signal(self.name, symbol, direction=1.0, reason="常に買い")

    ctx = ctx_for({"short": make_bars(wavy(10)), "long": make_bars(wavy(60))})
    signals = NeedsFifty().on_bars(ctx)

    assert [s.symbol for s in signals] == ["long"]


def test_indicator_strategy_applies_indicator_columns() -> None:
    seen: list[str] = []

    class Peek(IndicatorStrategy):
        name = "peek"
        warmup_bars = 5

        def indicators(self) -> list[pl.Expr]:
            from wbjp.indicators.ohlcv import sma

            return [sma(5)]

        def evaluate(self, symbol, frame, ctx):  # type: ignore[no-untyped-def]
            seen.extend(frame.columns)
            return None

    Peek().on_bars(ctx_for({"7203": make_bars(wavy(30))}))
    assert "sma_5" in seen


# --------------------------------------------------------------------------
# サンプル戦略
# --------------------------------------------------------------------------


def test_sma_cross_detects_golden_cross() -> None:
    """下落から上昇に転じた局面で、上抜けシグナルが出る。"""
    closes = [100.0 - i for i in range(60)] + [40.0 + i * 3 for i in range(40)]
    strategy = SmaCrossStrategy(fast=5, slow=20)

    directions = []
    bars = make_bars(closes)
    for end in range(strategy.warmup_bars, len(closes)):
        ctx = StrategyContext(as_of=bars["date"][end - 1], _bars={"X": bars.head(end)})
        signals = strategy.on_bars(ctx)
        if signals and "上抜け" in signals[0].reason:
            directions.append(signals[0].direction)

    assert directions and all(d == 1.0 for d in directions)


def test_sma_cross_rejects_invalid_periods() -> None:
    with pytest.raises(ValueError, match="fast は slow より小さく"):
        SmaCrossStrategy(fast=75, slow=25)


def test_sma_cross_holds_an_opinion_between_crosses() -> None:
    """クロスした日以外も向きを表明する。

    これが無いと、クロス当日しか保有を維持できず翌日に手仕舞ってしまう。
    """
    closes = [100.0 + i for i in range(80)]
    strategy = SmaCrossStrategy(fast=5, slow=20)
    signals = strategy.on_bars(ctx_for({"X": make_bars(closes)}))

    assert len(signals) == 1
    assert signals[0].direction == 1.0
    assert "推移" in signals[0].reason


def test_rsi_reversion_buys_when_oversold() -> None:
    closes = [200.0 - i * 2 for i in range(80)]  # 一貫した下落
    strategy = RsiReversionStrategy(period=14, adx_period=5, max_adx=100.0)
    signals = strategy.on_bars(ctx_for({"X": make_bars(closes)}))

    assert len(signals) == 1
    assert signals[0].direction == 1.0
    assert "売られすぎ" in signals[0].reason


def test_rsi_reversion_stays_silent_in_strong_trend() -> None:
    """強トレンド時は黙る。逆張りが最も損をする局面を避ける設計。"""
    closes = [200.0 - i * 2 for i in range(80)]
    ctx = ctx_for({"X": make_bars(closes)})

    permissive = RsiReversionStrategy(period=14, adx_period=5, max_adx=100.0)
    strict = RsiReversionStrategy(period=14, adx_period=5, max_adx=10.0)

    assert permissive.on_bars(ctx) != []
    assert strict.on_bars(ctx) == []


def test_rsi_reversion_rejects_invalid_thresholds() -> None:
    with pytest.raises(ValueError, match="oversold"):
        RsiReversionStrategy(oversold=80.0, overbought=20.0)


def test_atr_breakout_fires_on_new_high() -> None:
    closes = [100.0] * 40 + [130.0]
    strategy = AtrBreakoutStrategy(channel=20, atr_period=14, min_atr_ratio=0.0)
    signals = strategy.on_bars(ctx_for({"X": make_bars(closes)}))

    assert len(signals) == 1
    assert signals[0].direction == 1.0


def test_atr_breakout_ignores_low_volatility() -> None:
    """値動きが乏しい局面では、だましを避けて見送る。"""
    closes = [100.0] * 40 + [130.0]
    strategy = AtrBreakoutStrategy(channel=20, atr_period=14, min_atr_ratio=0.5)
    assert strategy.on_bars(ctx_for({"X": make_bars(closes)})) == []


def test_atr_breakout_does_not_use_the_current_bar_as_its_own_threshold() -> None:
    """当日高値を閾値に含めると必ずブレイクが成立してしまう。

    その状態なら「静かな横ばい」でもシグナルが出るはず。出なければ
    当日を正しく除外できている。
    """
    closes = [100.0 + (i % 2) * 0.05 for i in range(60)]
    strategy = AtrBreakoutStrategy(channel=20, atr_period=14, min_atr_ratio=0.0)
    assert strategy.on_bars(ctx_for({"X": make_bars(closes)})) == []


@pytest.mark.parametrize("name", ["sma_cross", "rsi_reversion", "atr_breakout"])
def test_sample_strategies_survive_short_history(name: str) -> None:
    """足が数本しか無くても例外を出さない。"""
    strategy = create(name)
    assert strategy.on_bars(ctx_for({"X": make_bars(wavy(3))})) == []


@pytest.mark.parametrize("name", ["sma_cross", "rsi_reversion", "atr_breakout"])
def test_sample_strategies_produce_valid_signals(name: str) -> None:
    strategy = create(name)
    for signal in strategy.on_bars(ctx_for({"X": make_bars(wavy(300))})):
        assert -1.0 <= signal.direction <= 1.0
        assert 0.0 <= signal.confidence <= 1.0
        assert signal.reason, "reason は必ず埋める（後から判断を追えるように）"
        assert signal.strategy == name


# --------------------------------------------------------------------------
# 登録簿
# --------------------------------------------------------------------------


def test_builtin_strategies_are_registered() -> None:
    assert set(available()) >= {"sma_cross", "rsi_reversion", "atr_breakout"}


def test_get_unknown_strategy_lists_alternatives() -> None:
    with pytest.raises(ValueError, match="sma_cross"):
        get("does_not_exist")


def test_create_passes_parameters() -> None:
    strategy = create("sma_cross", {"fast": 3, "slow": 8})
    assert isinstance(strategy, SmaCrossStrategy)
    assert (strategy.fast, strategy.slow) == (3, 8)


def test_create_rejects_unknown_parameter() -> None:
    with pytest.raises(ValueError, match="パラメータが不正"):
        create("sma_cross", {"nonexistent": 1})


def test_register_rejects_duplicate_name() -> None:
    class Duplicate(Strategy):
        name = "sma_cross"

        def on_bars(self, ctx: StrategyContext) -> list[Signal]:
            return []

    with pytest.raises(ValueError, match="既に"):
        register(Duplicate)


def test_build_all_from_config_entries() -> None:
    from wbjp.config import StrategyEntry

    strategies = build_all(
        [
            StrategyEntry(name="sma_cross", fast=5, slow=20),
            StrategyEntry(name="atr_breakout", channel=10),
        ]
    )
    assert [s.name for s in strategies] == ["sma_cross", "atr_breakout"]
    assert strategies[0].fast == 5  # type: ignore[attr-defined]


# --------------------------------------------------------------------------
# 合成器
# --------------------------------------------------------------------------


def sig(strategy: str, direction: float, confidence: float = 1.0, symbol: str = "X") -> Signal:
    return Signal(strategy, symbol, direction=direction, confidence=confidence, reason="test")


def test_weighted_vote_averages_scores() -> None:
    result = WeightedVoteCombiner().combine([sig("a", 1.0), sig("b", -1.0), sig("c", 1.0)])
    assert result["X"].direction == pytest.approx(1 / 3)


def test_weighted_vote_respects_weights() -> None:
    combiner = WeightedVoteCombiner({"a": 3.0, "b": 1.0})
    result = combiner.combine([sig("a", 1.0), sig("b", -1.0)])
    assert result["X"].direction == pytest.approx((3.0 - 1.0) / 4.0)


def test_weighted_vote_uses_confidence() -> None:
    result = WeightedVoteCombiner().combine([sig("a", 1.0, confidence=0.5)])
    assert result["X"].direction == pytest.approx(0.5)


def test_silent_strategy_does_not_count_as_neutral() -> None:
    """意見を言わなかった戦略が、中立票として結論を薄めてはいけない。

    「黙る」と「中立だと主張する」は別物。ここを取り違えると、
    フィルタで黙った戦略が他の戦略の判断を打ち消してしまう。
    """
    combiner = WeightedVoteCombiner()

    only_one = combiner.combine([sig("a", 1.0)])["X"].direction
    with_neutral = combiner.combine([sig("a", 1.0), sig("b", 0.0)])["X"].direction

    assert only_one == pytest.approx(1.0)
    assert with_neutral == pytest.approx(0.5)
    assert only_one != with_neutral


def test_combined_signal_records_every_contribution() -> None:
    """なぜその結論になったかを必ず残す。"""
    result = WeightedVoteCombiner().combine([sig("a", 1.0), sig("b", -0.5)])
    assert result["X"].contributions == {"a": 1.0, "b": -0.5}


def test_combiner_groups_by_symbol() -> None:
    result = WeightedVoteCombiner().combine(
        [sig("a", 1.0, symbol="7203"), sig("a", -1.0, symbol="6758")]
    )
    assert result["7203"].direction == pytest.approx(1.0)
    assert result["6758"].direction == pytest.approx(-1.0)


def test_majority_follows_the_larger_camp() -> None:
    result = MajorityVoteCombiner().combine([sig("a", 1.0), sig("b", 1.0), sig("c", -1.0)])
    assert result["X"].direction > 0
    assert "強気多数" in result["X"].reason


def test_majority_is_neutral_on_a_tie() -> None:
    """賛否同数なら動かない。"""
    result = MajorityVoteCombiner().combine([sig("a", 1.0), sig("b", -1.0)])
    assert result["X"].direction == 0.0
    assert "同数" in result["X"].reason


def test_majority_ignores_the_minority_magnitude() -> None:
    """少数派がどれだけ強く主張しても、多数決の向きは変わらない。"""
    result = MajorityVoteCombiner().combine([sig("a", 0.2), sig("b", 0.2), sig("c", -1.0)])
    assert result["X"].direction > 0


def test_veto_requires_unanimity() -> None:
    agree = VetoCombiner().combine([sig("a", 1.0), sig("b", 0.8)])
    assert agree["X"].direction > 0

    disagree = VetoCombiner().combine([sig("a", 1.0), sig("b", -0.8)])
    assert disagree["X"].direction == 0.0
    assert "割れた" in disagree["X"].reason


def test_veto_ignores_near_neutral_opinions() -> None:
    """ほぼ中立の意見は反対票として扱わない。"""
    result = VetoCombiner().combine([sig("a", 1.0), sig("b", -0.01)])
    assert result["X"].direction > 0


def test_priority_picks_the_highest_weight() -> None:
    combiner = PriorityCombiner({"risk_off": 10.0, "trend": 1.0})
    result = combiner.combine([sig("trend", 1.0), sig("risk_off", -1.0)])
    assert result["X"].direction == pytest.approx(-1.0)
    assert "risk_off" in result["X"].reason


def test_priority_tie_break_is_deterministic() -> None:
    """重みが同じでも、実行のたびに結果が変わらない。"""
    combiner = PriorityCombiner()
    signals = [sig("b", 1.0), sig("a", -1.0)]
    first = combiner.combine(signals)["X"].direction
    assert all(combiner.combine(signals)["X"].direction == first for _ in range(5))


def test_all_combiners_stay_within_bounds() -> None:
    signals = [sig("a", 1.0), sig("b", 1.0), sig("c", 1.0)]
    for name in ("weighted_vote", "majority", "veto", "priority"):
        direction = build_combiner(name).combine(signals)["X"].direction
        assert -1.0 <= direction <= 1.0, name


def test_build_combiner_rejects_unknown_name() -> None:
    with pytest.raises(ValueError, match="weighted_vote"):
        build_combiner("coin_flip")


def test_combiners_handle_empty_input() -> None:
    for name in ("weighted_vote", "majority", "veto", "priority"):
        assert build_combiner(name).combine([]) == {}
