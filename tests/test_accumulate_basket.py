"""バスケット（複数銘柄への配分）と 13F パーサの検証。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import numpy as np
import polars as pl
import pytest

from accum import (
    BasketSettings,
    BearStack,
    WeightSchedule,
    build_basket_plan,
    simulate_basket,
    xirr,
)
from accum.config import AccumConfig
from wbcore.data.edgar_13f import parse_information_table, weight_schedule


def _bars(closes: list[float], *, start: dt.date = dt.date(2020, 1, 1)) -> pl.DataFrame:
    # 営業日っぽく平日だけ並べる
    dates: list[dt.date] = []
    day = start
    while len(dates) < len(closes):
        if day.weekday() < 5:
            dates.append(day)
        day += dt.timedelta(days=1)
    return pl.DataFrame({"date": dates, "close": closes})


# --- 配分表 -------------------------------------------------------------


def test_schedule_applies_from_the_day_after_effective() -> None:
    schedule = WeightSchedule.from_pairs(
        [
            (dt.date(2020, 1, 10), {"A": 1.0}),
            (dt.date(2020, 4, 10), {"B": 1.0}),
        ]
    )
    assert schedule.at(dt.date(2020, 1, 9)) == {}
    assert schedule.at(dt.date(2020, 1, 10)) == {}  # 提出日当日はまだ前の表
    assert schedule.at(dt.date(2020, 1, 11)) == {"A": 1.0}
    assert schedule.at(dt.date(2020, 4, 11)) == {"B": 1.0}
    assert schedule.symbols == ["A", "B"]


def test_blend_keeps_core_share_fixed() -> None:
    satellite = WeightSchedule.from_pairs([(dt.date(2020, 1, 1), {"A": 3.0, "B": 1.0})])
    blended = satellite.blend({"VOO": 1.0}, 0.3)
    weights = blended.at(dt.date(2020, 6, 1))
    assert weights["VOO"] == pytest.approx(0.7)
    assert weights["A"] == pytest.approx(0.225)
    assert weights["B"] == pytest.approx(0.075)


def test_negative_weight_is_rejected() -> None:
    with pytest.raises(ValueError, match="負にできません"):
        WeightSchedule.from_pairs([(dt.date(2020, 1, 1), {"A": -1.0})])


# --- 計画 ---------------------------------------------------------------


def test_budget_is_split_by_weight_on_payday() -> None:
    bars = {"A": _bars([100.0] * 30), "B": _bars([50.0] * 30)}
    settings = BasketSettings(Decimal(100_000), WeightSchedule.static({"A": 3, "B": 1}))
    plans = build_basket_plan(bars, settings)
    payday = plans["A"].filter(pl.col("base") > 0)
    assert payday["base"].to_list() == [75_000, 75_000]
    assert plans["B"].filter(pl.col("base") > 0)["base"].to_list() == [25_000, 25_000]


def test_weight_is_renormalized_when_a_symbol_has_no_bars_yet() -> None:
    a = _bars([100.0] * 40)
    b = _bars([50.0] * 20, start=dt.date(2020, 1, 29))  # 途中から上場
    settings = BasketSettings(Decimal(100_000), WeightSchedule.static({"A": 1, "B": 1}))
    plans = build_basket_plan({"A": a, "B": b}, settings)
    first, second = plans["A"].filter(pl.col("base") > 0)["base"].to_list()
    assert first == 100_000  # 1月は B が無いので A に全額
    assert second == 50_000  # 2月は折半
    assert plans["B"].filter(pl.col("base") > 0)["base"].to_list() == [50_000]


def test_tactic_multiplier_scales_with_weight() -> None:
    down = list(np.linspace(200.0, 100.0, 500))
    bars = {"A": _bars(down), "B": _bars(down)}
    settings = BasketSettings(
        Decimal(100_000), WeightSchedule.static({"A": 1, "B": 1}), BearStack(multiplier=3)
    )
    plans = build_basket_plan(bars, settings)
    assert (plans["A"]["extra"] > 0).any()
    # 同じ足なので A と B の増額は等しく、合計は予算の (3-1) 倍/月を超えない
    assert plans["A"]["extra"].sum() == plans["B"]["extra"].sum()


def test_symbols_with_zero_weight_are_dropped() -> None:
    bars = {"A": _bars([100.0] * 30), "B": _bars([50.0] * 30)}
    settings = BasketSettings(Decimal(100_000), WeightSchedule.static({"A": 1}))
    assert set(build_basket_plan(bars, settings)) == {"A"}


# --- 検証 ---------------------------------------------------------------


def test_xirr_matches_a_known_growth_rate() -> None:
    dates = [dt.date(2021, 1, 1), dt.date(2022, 1, 1)]
    rate = xirr(dates, np.array([-100.0, 0.0]), 110.0)
    assert rate == pytest.approx(0.10, abs=1e-4)


def test_flat_market_returns_contributed_amount() -> None:
    bars = {"A": _bars([100.0] * 60), "B": _bars([50.0] * 60)}
    settings = BasketSettings(Decimal(100_000), WeightSchedule.static({"A": 1, "B": 1}))
    result = simulate_basket(bars, build_basket_plan(bars, settings), benchmark=bars["A"])
    assert result.basket.terminal_value == pytest.approx(float(result.basket.contributed))
    assert result.basket.max_drawdown == pytest.approx(0.0)
    assert result.benchmark is not None
    assert result.benchmark.terminal_value == pytest.approx(float(result.basket.contributed))
    assert result.symbols == ["A", "B"]


def test_drawdown_reflects_price_not_contributions() -> None:
    closes = [100.0] * 30 + [50.0] * 30 + [100.0] * 30
    bars = {"A": _bars(closes)}
    settings = BasketSettings(Decimal(100_000), WeightSchedule.static({"A": 1}))
    result = simulate_basket(bars, build_basket_plan(bars, settings))
    assert result.basket.max_drawdown == pytest.approx(0.5, abs=0.02)


# --- 13F --------------------------------------------------------------

_SAMPLE = """
<infoTable><nameOfIssuer>APPLE INC</nameOfIssuer><titleOfClass>COM</titleOfClass>
<cusip>037833100</cusip><value>600</value>
<shrsOrPrnAmt><sshPrnamt>1</sshPrnamt><sshPrnamtType>SH</sshPrnamtType></shrsOrPrnAmt></infoTable>
<infoTable><nameOfIssuer>APPLE INC</nameOfIssuer><titleOfClass>COM</titleOfClass>
<cusip>037833100</cusip><value>400</value>
<shrsOrPrnAmt><sshPrnamt>1</sshPrnamt><sshPrnamtType>SH</sshPrnamtType></shrsOrPrnAmt></infoTable>
<infoTable><nameOfIssuer>COCA COLA CO</nameOfIssuer><titleOfClass>COM</titleOfClass>
<cusip>191216100</cusip><value>500</value>
<shrsOrPrnAmt><sshPrnamt>1</sshPrnamt><sshPrnamtType>SH</sshPrnamtType></shrsOrPrnAmt></infoTable>
<infoTable><nameOfIssuer>SOME BOND</nameOfIssuer><titleOfClass>NOTE</titleOfClass>
<cusip>999999999</cusip><value>900</value>
<shrsOrPrnAmt><sshPrnamt>1</sshPrnamt><sshPrnamtType>PRN</sshPrnamtType></shrsOrPrnAmt></infoTable>
<infoTable><nameOfIssuer>GONE CO</nameOfIssuer><titleOfClass>COM</titleOfClass>
<cusip>111111111</cusip><value>500</value>
<shrsOrPrnAmt><sshPrnamt>1</sshPrnamt><sshPrnamtType>SH</sshPrnamtType></shrsOrPrnAmt></infoTable>
"""


def test_parse_aggregates_same_cusip_and_skips_non_shares() -> None:
    rows = {c: v for c, _, v in parse_information_table(_SAMPLE)}
    assert rows == {"037833100": 1000.0, "191216100": 500.0, "111111111": 500.0}


def test_weight_schedule_drops_unmapped_and_delisted_then_normalizes() -> None:
    filed = dt.date(2024, 5, 15)
    holdings = pl.DataFrame(
        {
            "filed": [filed] * 3,
            "period": [dt.date(2024, 3, 31)] * 3,
            "cusip": ["037833100", "191216100", "111111111"],
            "name": ["APPLE", "KO", "GONE"],
            "value": [1000.0, 500.0, 500.0],
        }
    )
    pairs = weight_schedule(holdings, {"037833100": "AAPL", "191216100": "KO", "111111111": ""})
    assert pairs == [(filed, {"AAPL": pytest.approx(2 / 3), "KO": pytest.approx(1 / 3)})]


def test_top_n_is_applied_before_mapping() -> None:
    filed = dt.date(2024, 5, 15)
    holdings = pl.DataFrame(
        {
            "filed": [filed] * 2,
            "period": [dt.date(2024, 3, 31)] * 2,
            "cusip": ["037833100", "191216100"],
            "name": ["APPLE", "KO"],
            "value": [1000.0, 500.0],
        }
    )
    pairs = weight_schedule(holdings, {"037833100": "AAPL", "191216100": "KO"}, top=1)
    assert pairs == [(filed, {"AAPL": 1.0})]


# --- 設定 ---------------------------------------------------------------


def test_basket_entry_builds_static_and_blended_schedules() -> None:
    config = AccumConfig.model_validate(
        {
            "baskets": [
                {"id": "s", "weights": {"VOO": 1.0}},
                {
                    "id": "b",
                    "source": "13f",
                    "weights": {"VOO": 1.0},
                    "satellite_share": 0.3,
                    "tactic": "bear_stack",
                    "multiplier": 2,
                },
            ]
        }
    )
    static, blended = config.active_baskets
    assert static.build_schedule().at(dt.date.today()) == {"VOO": 1.0}
    schedule = blended.build_schedule([(dt.date(2020, 1, 1), {"AAPL": 1.0})])
    assert schedule.at(dt.date(2021, 1, 1)) == {
        "VOO": pytest.approx(0.7),
        "AAPL": pytest.approx(0.3),
    }
    assert isinstance(blended.build_tactic(), BearStack)
    with pytest.raises(ValueError, match="sync-13f"):
        blended.build_schedule(None)


def test_basket_ids_share_the_namespace_with_tactics() -> None:
    config = AccumConfig.model_validate(
        {
            "tactics": [{"id": "x", "tactic": "constant", "symbols": ["A"]}],
            "baskets": [{"id": "x", "weights": {"A": 1.0}}],
        }
    )
    with pytest.raises(ValueError, match="id が重複"):
        config.validate_assignment()


# --- 割安傾斜 -----------------------------------------------------------


def test_tilt_shifts_budget_toward_the_fallen_symbol() -> None:
    from accum import DrawdownTilt

    flat = _bars([100.0] * 60)
    fallen = _bars([100.0] * 30 + [50.0] * 30)  # 2か月目の途中で半値（60営業日は3か月）
    schedule = WeightSchedule.static({"A": 1, "B": 1})
    plain = build_basket_plan({"A": flat, "B": fallen}, BasketSettings(Decimal(100_000), schedule))
    tilted = build_basket_plan(
        {"A": flat, "B": fallen},
        BasketSettings(Decimal(100_000), schedule, tilt=DrawdownTilt(strength=2, lookback=60)),
    )
    plain_b = plain["B"].filter(pl.col("base") > 0)["base"].to_list()
    tilted_b = tilted["B"].filter(pl.col("base") > 0)["base"].to_list()
    assert plain_b == [50_000, 50_000, 50_000]
    assert tilted_b[:2] == [50_000, 50_000]  # 下落前の入金日は均等
    # 3か月目: 下落率 0.5 → 係数 2.0。B:A = 2:1 なので B に 66,666
    assert tilted_b[2] == 66_666
    tilted_a = tilted["A"].filter(pl.col("base") > 0)["base"].to_list()
    assert tilted_a[2] == 33_333


def test_tilt_parameters_are_validated() -> None:
    from accum import DrawdownTilt

    with pytest.raises(ValueError, match="strength"):
        DrawdownTilt(strength=0)
    with pytest.raises(ValueError, match="lookback"):
        DrawdownTilt(lookback=1)
