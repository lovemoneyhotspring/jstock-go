"""日本株ルールのテスト。

呼値・値幅制限を間違えると注文が取引所に弾かれるため、
テーブルの境界値を重点的に固める。
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import pytest

from wbcore.domain.jp_rules import (
    JST,
    PriceRounding,
    is_trading_hours,
    is_within_price_limit,
    price_limit_range,
    price_limit_width,
    round_to_lot,
    snap_to_tick,
    tick_size,
    violates_same_day_settlement,
)
from wbcore.domain.models import Side

D = Decimal


# --------------------------------------------------------------------------
# 呼値
# --------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("price", "expected"),
    [
        # 境界値ちょうどは「以下」側の区分に入る
        ("1", "1"),
        ("1000", "1"),
        ("1000.5", "1"),
        ("3000", "1"),
        ("3001", "5"),
        ("5000", "5"),
        ("5001", "10"),
        ("30000", "10"),
        ("30001", "50"),
        ("50000", "50"),
        ("50001", "100"),
        ("300000", "100"),
        ("300001", "500"),
        ("500000", "500"),
        ("500001", "1000"),
        ("3000000", "1000"),
        ("3000001", "5000"),
        ("50000000", "50000"),
        ("50000001", "100000"),
    ],
)
def test_tick_size_standard(price: str, expected: str) -> None:
    assert tick_size(D(price)) == D(expected)


@pytest.mark.parametrize(
    ("price", "expected"),
    [
        ("1", "0.1"),
        ("1000", "0.1"),
        ("1000.1", "0.5"),
        ("3000", "0.5"),
        ("3001", "1"),
        ("10000", "1"),
        ("10001", "5"),
        ("30000", "5"),
        ("30001", "10"),
        ("100000", "10"),
        ("100001", "50"),
        ("50000000", "10000"),
        ("50000001", "10000"),
    ],
)
def test_tick_size_topix500(price: str, expected: str) -> None:
    assert tick_size(D(price), topix500=True) == D(expected)


def test_tick_size_rejects_non_positive() -> None:
    with pytest.raises(ValueError, match="正の数"):
        tick_size(D(0))


def test_snap_to_tick_conservative_never_disadvantages() -> None:
    """保守的丸めでは、買いは高くならず、売りは安くならない。"""
    price = D("3007")  # 通常銘柄なら呼値5円
    assert snap_to_tick(price, Side.BUY) == D("3005")
    assert snap_to_tick(price, Side.SELL) == D("3010")


def test_snap_to_tick_aggressive_favors_execution() -> None:
    price = D("3007")
    assert snap_to_tick(price, Side.BUY, rounding=PriceRounding.AGGRESSIVE) == D("3010")
    assert snap_to_tick(price, Side.SELL, rounding=PriceRounding.AGGRESSIVE) == D("3005")


def test_snap_to_tick_nearest() -> None:
    assert snap_to_tick(D("3007"), Side.BUY, rounding=PriceRounding.NEAREST) == D("3005")
    assert snap_to_tick(D("3008"), Side.BUY, rounding=PriceRounding.NEAREST) == D("3010")


def test_snap_to_tick_topix500_sub_yen() -> None:
    """TOPIX500 は 1,000円以下で 0.1円刻み。"""
    assert snap_to_tick(D("987.63"), Side.BUY, topix500=True) == D("987.6")
    assert snap_to_tick(D("987.63"), Side.SELL, topix500=True) == D("987.7")


def test_snap_to_tick_crossing_tier_boundary() -> None:
    """丸めで価格帯をまたいでも、結果は必ずその帯の呼値に乗る。"""
    # 1000.4 は 0.5円刻みの帯。切り下げると 1000.0 で 0.1円刻みの帯に入る。
    result = snap_to_tick(D("1000.4"), Side.BUY, topix500=True)
    assert result == D("1000")
    assert result % tick_size(result, topix500=True) == 0


def test_snap_to_tick_result_is_always_on_tick() -> None:
    """広い価格帯で、結果が必ず呼値の倍数になることを確認する。"""
    for raw in ["1.07", "999.99", "1000.4", "4999.9", "12345.6", "987654.3", "12345678"]:
        for topix500 in (False, True):
            for side in (Side.BUY, Side.SELL):
                snapped = snap_to_tick(D(raw), side, topix500=topix500)
                tick = tick_size(snapped, topix500=topix500)
                assert snapped > 0
                assert snapped % tick == 0, f"{raw} {side} topix500={topix500} -> {snapped}"


def test_snap_to_tick_never_returns_scientific_notation() -> None:
    """指数表記のまま API に渡すと注文が弾かれるため、絶対に出さない。"""
    for raw in ["1000", "10000", "1000000", "50000000", "99999999"]:
        for side in (Side.BUY, Side.SELL):
            snapped = snap_to_tick(D(raw), side)
            assert "E" not in str(snapped), f"{raw} -> {snapped}"


def test_snap_to_tick_below_minimum_tick_lifts_to_one_tick() -> None:
    """切り下げで 0 になる価格は 1ティックに引き上げる。"""
    assert snap_to_tick(D("0.05"), Side.BUY, topix500=True) == D("0.1")


# --------------------------------------------------------------------------
# 制限値幅
# --------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("base", "expected"),
    [
        ("99", "30"),
        ("100", "50"),  # 100円は「100円以上200円未満」の区分
        ("199", "50"),
        ("200", "80"),
        ("999", "150"),
        ("1000", "300"),
        ("30000", "7000"),
        ("49999", "7000"),
        ("50000", "10000"),
        ("50000000", "10000000"),
        ("99999999", "10000000"),
    ],
)
def test_price_limit_width(base: str, expected: str) -> None:
    assert price_limit_width(D(base)) == D(expected)


def test_price_limit_range() -> None:
    low, high = price_limit_range(D("2500"))
    assert (low, high) == (D("2000"), D("3000"))


def test_price_limit_range_floor_is_one_yen() -> None:
    """値幅を引くと 0 以下になる低位株でも、下限は 1円で止める。"""
    low, _ = price_limit_range(D("20"))
    assert low == D(1)


def test_is_within_price_limit() -> None:
    base = D("2500")
    assert is_within_price_limit(D("3000"), base)
    assert is_within_price_limit(D("2000"), base)
    assert not is_within_price_limit(D("3001"), base)
    assert not is_within_price_limit(D("1999"), base)


# --------------------------------------------------------------------------
# 単元株
# --------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("qty", "expected"),
    [("0", "0"), ("99", "0"), ("100", "100"), ("199", "100"), ("250", "200"), ("1000", "1000")],
)
def test_round_to_lot_truncates(qty: str, expected: str) -> None:
    assert round_to_lot(D(qty)) == D(expected)


def test_round_to_lot_never_rounds_up() -> None:
    """切り上げると資金を超過しうるため、絶対に切り上げない。"""
    for qty in range(0, 1000):
        assert round_to_lot(D(qty)) <= D(qty)


def test_round_to_lot_negative_reduces_magnitude() -> None:
    assert round_to_lot(D("-250")) == D("-200")


def test_round_to_lot_custom_size() -> None:
    assert round_to_lot(D("57"), D(1)) == D("57")
    assert round_to_lot(D("57"), D(10)) == D("50")


def test_round_to_lot_rejects_bad_lot_size() -> None:
    with pytest.raises(ValueError, match="正の数"):
        round_to_lot(D("100"), D(0))


# --------------------------------------------------------------------------
# 取引時間
# --------------------------------------------------------------------------


def _jst(y: int, m: int, d: int, hh: int, mm: int) -> dt.datetime:
    return dt.datetime(y, m, d, hh, mm, tzinfo=JST)


@pytest.mark.parametrize(
    ("when", "expected"),
    [
        (_jst(2026, 8, 24, 8, 59), False),  # 寄り前
        (_jst(2026, 8, 24, 9, 0), True),  # 前場寄り
        (_jst(2026, 8, 24, 11, 30), True),  # 前場引け
        (_jst(2026, 8, 24, 12, 0), False),  # 昼休み
        (_jst(2026, 8, 24, 12, 30), True),  # 後場寄り
        (_jst(2026, 8, 24, 15, 25), True),  # クロージング・オークション
        (_jst(2026, 8, 24, 15, 30), True),  # 大引け
        (_jst(2026, 8, 24, 15, 31), False),  # 引け後
        (_jst(2026, 8, 22, 10, 0), False),  # 土曜
        (_jst(2026, 8, 23, 10, 0), False),  # 日曜
    ],
)
def test_is_trading_hours(when: dt.datetime, expected: bool) -> None:
    assert is_trading_hours(when) is expected


def test_is_trading_hours_converts_timezone() -> None:
    """UTC で渡しても JST に変換して判定する。"""
    utc_noon_jst = dt.datetime(2026, 8, 24, 1, 0, tzinfo=dt.UTC)  # JST 10:00
    assert is_trading_hours(utc_noon_jst) is True


# --------------------------------------------------------------------------
# 差金決済
# --------------------------------------------------------------------------


def test_same_day_settlement_blocks_selling_what_was_bought_today() -> None:
    assert violates_same_day_settlement(Side.SELL, "7203", {"7203"}) is True


def test_same_day_settlement_allows_other_cases() -> None:
    assert violates_same_day_settlement(Side.SELL, "6758", {"7203"}) is False
    assert violates_same_day_settlement(Side.BUY, "7203", {"7203"}) is False
    assert violates_same_day_settlement(Side.SELL, "7203", set()) is False
