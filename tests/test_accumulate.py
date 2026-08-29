"""積立モジュールの検証。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import numpy as np
import polars as pl
import pytest

from accum import (
    AccumulationSettings,
    BearStack,
    Constant,
    bear_stack,
    build_plan,
    simulate,
    stack_score,
)


def _bars(closes: list[float], *, start: dt.date = dt.date(2020, 1, 1)) -> pl.DataFrame:
    return pl.DataFrame(
        {
            "date": [start + dt.timedelta(days=i) for i in range(len(closes))],
            "close": closes,
        }
    )


def _long(kind: str, n: int = 500) -> pl.DataFrame:
    if kind == "down":
        return _bars(list(np.linspace(200.0, 100.0, n)))
    if kind == "up":
        return _bars(list(np.linspace(100.0, 200.0, n)))
    return _bars([100.0] * n)


# --- 配列判定 -----------------------------------------------------------


def test_bear_stack_true_on_sustained_decline() -> None:
    frame = _long("down").with_columns(bear_stack())
    # 200日線が確定してからは、単調下降なので常に完全下降配列
    settled = frame.tail(200)
    assert settled["bear_stack"].all()


def test_bear_stack_false_on_sustained_rise() -> None:
    frame = _long("up").with_columns(bear_stack())
    assert not frame["bear_stack"].fill_null(False).any()


def test_bear_stack_is_null_before_warmup() -> None:
    frame = _long("down").with_columns(bear_stack())
    assert frame["bear_stack"][:199].null_count() == 199


def test_stack_score_agrees_with_bear_stack() -> None:
    frame = _long("down").with_columns(bear_stack(), stack_score())
    settled = frame.drop_nulls("bear_stack")
    assert ((settled["stack_score"] == 6) == settled["bear_stack"]).all()


def test_stack_score_is_zero_in_perfect_uptrend() -> None:
    frame = _long("up").with_columns(stack_score())
    assert frame.tail(200)["stack_score"].max() == 0


def test_invalid_periods_are_rejected() -> None:
    with pytest.raises(ValueError, match="fast < mid < slow"):
        bear_stack(50, 20, 200)


# --- 計画 ---------------------------------------------------------------


def test_base_is_paid_once_per_month_in_full() -> None:
    bars = _long("flat", 400)
    plan = build_plan(bars)
    months = bars["date"].dt.truncate("1mo").n_unique()
    assert (plan["base"] > 0).sum() == months
    assert plan["base"].sum() == 25_000 * months


def test_constant_tactic_never_adds_extra() -> None:
    plan = build_plan(_long("down"), AccumulationSettings(tactic=Constant()))
    assert plan["extra"].sum() == 0
    assert (plan["amount"] == plan["base"]).all()


def test_extra_fires_only_when_multiplier_exceeds_one() -> None:
    plan = build_plan(_long("down"))
    boosted = plan.filter(pl.col("extra") > 0)
    assert boosted.height > 0
    assert (boosted["multiplier"] > 1.0).all()
    assert plan.filter(pl.col("multiplier") <= 1.0)["extra"].sum() == 0


def test_extra_per_month_does_not_exceed_budget_share() -> None:
    """月をまたいで下降配列が続いても、増額は月あたり基本の3倍を超えない。"""
    plan = build_plan(_long("down", 900))
    per_month = plan.group_by(pl.col("date").dt.truncate("1mo")).agg(pl.col("extra").sum())
    assert per_month["extra"].max() <= 75_000


def test_reason_is_always_filled() -> None:
    plan = build_plan(_long("down"))
    assert plan["reason"].null_count() == 0
    assert plan.filter(pl.col("amount") > 0)["reason"].str.contains("円").all()


def test_tactic_is_swappable() -> None:
    """戦術を差し替えると必要資金と効果が変わる。"""
    bars = _long("down", 900)
    strong = simulate(
        bars,
        build_plan(bars, AccumulationSettings(tactic=BearStack(4))),
        monthly_budget=Decimal(25_000),
    )
    weak = simulate(
        bars,
        build_plan(bars, AccumulationSettings(tactic=BearStack(2))),
        monthly_budget=Decimal(25_000),
    )
    assert strong.capital_multiple > weak.capital_multiple > 1.0
    assert strong.cost_edge < weak.cost_edge < 0


def test_missing_column_is_rejected() -> None:
    with pytest.raises(ValueError, match="必要な列がありません"):
        build_plan(pl.DataFrame({"date": [dt.date(2020, 1, 1)]}))


def test_unsorted_bars_are_rejected() -> None:
    bars = _long("flat", 10).reverse()
    with pytest.raises(ValueError, match="date 昇順"):
        build_plan(bars)


# --- 検証 ---------------------------------------------------------------


def test_flat_market_gives_price_as_average_cost() -> None:
    bars = _long("flat", 400)
    settings = AccumulationSettings()
    result = simulate(bars, build_plan(bars, settings), monthly_budget=settings.monthly_budget)
    assert result.average_cost == pytest.approx(100.0)
    assert result.cost_edge == pytest.approx(0.0, abs=1e-9)
    assert result.capital_multiple == pytest.approx(1.0)


def test_declining_market_beats_the_control() -> None:
    """下降相場で増額すれば、同額を均等に入れるより安く買えるはず。"""
    bars = _long("down", 900)
    settings = AccumulationSettings()
    result = simulate(bars, build_plan(bars, settings), monthly_budget=settings.monthly_budget)
    assert result.cost_edge < 0
    assert result.capital_multiple > 1.0
    assert result.boosted_days > 0


def test_fill_uses_next_session_not_today() -> None:
    """当日終値で約定させていないこと（先読みの遮断）。"""
    closes = [100.0] * 300 + [50.0] + [100.0] * 100
    bars = _bars(closes).with_columns(pl.col("close").alias("open"))
    plan = build_plan(bars, AccumulationSettings(tactic=Constant()))
    # 300 番目の足を「翌日安くなる直前の日」にして、投下日をそこに寄せる
    single = plan.with_columns(
        pl.when(pl.col("date") == bars["date"][299]).then(25_000).otherwise(0).alias("base"),
        pl.lit(0, dtype=pl.Int64).alias("extra"),
    ).with_columns((pl.col("base") + pl.col("extra")).alias("amount"))
    result = simulate(bars, single, monthly_budget=Decimal(25_000))
    # 判断日の終値は 100 だが、約定は翌営業日の寄付 50 になる
    assert result.average_cost == pytest.approx(50.0)


def test_capital_multiple_matches_contributions() -> None:
    bars = _long("down", 900)
    settings = AccumulationSettings()
    plan = build_plan(bars, settings)
    result = simulate(bars, plan, monthly_budget=settings.monthly_budget)
    months = (plan["base"] > 0).sum()
    assert result.contributed == Decimal(int(plan["amount"].sum()))
    assert result.capital_multiple == pytest.approx(float(result.contributed) / (25_000 * months))


def test_empty_plan_is_rejected() -> None:
    with pytest.raises(ValueError, match="計画表が空です"):
        simulate(
            _long("flat", 10), build_plan(_long("flat", 10)).head(0), monthly_budget=Decimal(1)
        )
