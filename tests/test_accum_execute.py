"""積立計画 → 注文 の変換（``accum.execute``）。

ここが壊れると「予算どおりの株数で、正しい銘柄コードに、二重発注せずに」
買えなくなる。ブローカーは使わず、注文リクエストの中身だけを見る。
"""

from __future__ import annotations

import dataclasses
import datetime as dt
from decimal import Decimal

import polars as pl
import pytest

from accum.config import AccumConfig
from accum.execute import (
    Contribution,
    broker_symbol,
    build_orders,
    to_order,
    todays_contributions,
)
from accum.tactics import Constant
from wbcore.domain.models import Market, OrderType, Side, TaxAccountType


def _contribution(
    symbol: str = "1305.T",
    market: Market = Market.JP,
    *,
    close: str = "3000",
    amount: str = "25000",
    window: object = False,
) -> Contribution:
    return Contribution(
        symbol=symbol,
        market=market,
        date=dt.date(2026, 8, 3),
        close=Decimal(close),
        amount=Decimal(amount),
        multiplier=1.0,
        reason="入金日",
        tactic=Constant(window=window),
    )


# --- 銘柄コード --------------------------------------------------------


def test_jp_symbol_drops_yahoo_suffix_for_the_broker() -> None:
    assert broker_symbol("1305.T", Market.JP) == "1305"


def test_us_symbol_is_passed_through() -> None:
    assert broker_symbol("VOO", Market.US) == "VOO"


def test_index_cannot_be_ordered() -> None:
    with pytest.raises(ValueError, match="指数は発注できません"):
        broker_symbol("^N225", Market.JP)


# --- 注文 --------------------------------------------------------------


def test_order_is_a_market_buy_rounded_down_to_the_lot() -> None:
    # 25,000 / 3,000 = 8.33 株 → 10株単位では 0 だが、1株単位なら 8 株
    order = to_order(_contribution(), tax_type=TaxAccountType.NISA, lot_size=1)
    assert order.symbol == "1305"
    assert order.side is Side.BUY
    assert order.order_type is OrderType.MARKET
    assert order.quantity == Decimal(8)
    assert order.tax_type is TaxAccountType.NISA
    assert order.limit_price is None


def test_below_one_lot_is_rejected_with_advice() -> None:
    # 既定の 100 株単位だと 300,000 必要
    with pytest.raises(ValueError, match=r"1単元.*に届きません"):
        to_order(_contribution(), tax_type=TaxAccountType.SPECIFIC)


def test_order_id_is_deterministic_for_the_same_day() -> None:
    """cron が二重に走っても同じ ID になり、ブローカー側で弾ける。"""
    first = to_order(_contribution(), tax_type=TaxAccountType.SPECIFIC, lot_size=1)
    second = to_order(_contribution(), tax_type=TaxAccountType.SPECIFIC, lot_size=1)
    assert first.client_order_id == second.client_order_id
    assert len(first.client_order_id) <= 32


def test_order_id_changes_with_the_date() -> None:
    a = to_order(_contribution(), tax_type=TaxAccountType.SPECIFIC, lot_size=1)
    later = dataclasses.replace(_contribution(), date=dt.date(2026, 9, 1))
    b = to_order(later, tax_type=TaxAccountType.SPECIFIC, lot_size=1)
    assert a.client_order_id != b.client_order_id


# --- 一括変換 ----------------------------------------------------------


def test_build_orders_keeps_failures_with_a_note() -> None:
    """作れない注文も落とさない。何が見送られたかを表に出すため。"""
    planned = build_orders(
        [_contribution(), _contribution("^N225"), _contribution("1591.T")],
        tax_type=TaxAccountType.SPECIFIC,
        lot_sizes={"1305.T": 1},  # 1591.T は既定の 100 株単位のまま → 届かない
    )
    ok, index, too_small = planned
    assert ok.request is not None and ok.request.quantity == Decimal(8)
    assert index.request is None and "指数" in index.note
    assert too_small.request is None and "1単元" in too_small.note


def test_build_orders_respects_the_trading_window() -> None:
    c = _contribution(window={"start": "14:00", "end": "15:00"})
    morning = dt.datetime(2026, 8, 3, 10, 0, tzinfo=dt.UTC)
    (blocked,) = build_orders(
        [c], tax_type=TaxAccountType.SPECIFIC, lot_sizes={"1305.T": 1}, moment=morning
    )
    assert blocked.request is None
    assert "発注時間帯の外" in blocked.note

    (forced,) = build_orders(
        [c],
        tax_type=TaxAccountType.SPECIFIC,
        lot_sizes={"1305.T": 1},
        moment=morning,
        ignore_window=True,
    )
    assert forced.request is not None


# --- 計画からの取り出し ------------------------------------------------


def _bars(n: int, *, start: dt.date = dt.date(2026, 7, 1)) -> pl.DataFrame:
    dates: list[dt.date] = []
    day = start
    while len(dates) < n:
        if day.weekday() < 5:
            dates.append(day)
        day += dt.timedelta(days=1)
    return pl.DataFrame(
        {
            "date": dates,
            "open": [100.0] * n,
            "high": [101.0] * n,
            "low": [99.0] * n,
            "close": [100.0] * n,
            "volume": [1000] * n,
        }
    )


def test_todays_contributions_only_returns_symbols_with_an_amount() -> None:
    config = AccumConfig.model_validate(
        {
            "monthly_budget": 10_000,
            "tactics": [
                {
                    "id": "a",
                    "tactic": "constant",
                    "symbols": ["AAA"],
                    "window": False,
                    "monthly_budget": 10_000,
                },
                {
                    "id": "b",
                    "tactic": "constant",
                    "symbols": ["BBB"],
                    "window": False,
                    "monthly_budget": 10_000,
                },
            ],
        }
    )
    # AAA は月初（入金日）で終わる。BBB は月の途中で終わり投下額 0。
    aaa = _bars(24)  # 7/1 〜 8/3 まで
    bbb = _bars(12)  # 7/1 〜 7/16
    out = todays_contributions(config, {"AAA": aaa, "BBB": bbb})
    assert [c.symbol for c in out] == ["AAA"]
    assert out[0].amount == Decimal(10_000)
    assert out[0].date == aaa["date"][-1]
