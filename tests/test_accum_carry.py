"""前月の残り（目標 − 発注済み）は当月に繰り越す。台帳の表記は設定と揃える。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import polars as pl

from accum.config import AccumConfig
from accum.execute import ledger_symbol, pending_contributions

JST = dt.timezone(dt.timedelta(hours=9))


def _bars(n: int, start: dt.date) -> pl.DataFrame:
    dates: list[dt.date] = []
    day = start
    while len(dates) < n:
        if day.weekday() < 5:
            dates.append(day)
        day += dt.timedelta(days=1)
    return pl.DataFrame({"date": dates, "close": [100.0] * n})


def _config(**fields) -> AccumConfig:  # type: ignore[no-untyped-def]
    return AccumConfig.model_validate(
        {
            "monthly_budget": 10_000,
            "tactics": [
                {
                    "id": "a",
                    "tactic": "constant",
                    "symbols": ["T"],
                    "window": False,
                    "monthly_budget": 10_000,
                    **fields,
                }
            ],
        }
    )


BARS = _bars(45, dt.date(2026, 7, 1))  # 7/1 〜 9/1
AUG, SEP = dt.date(2026, 8, 1), dt.date(2026, 9, 1)
NOW = dt.datetime(2026, 9, 2, 14, 5, tzinfo=JST)  # 9 月の入金日（9/1）の翌日


def test_previous_month_remainder_is_carried_into_this_month() -> None:
    """8 月に 10,000 のうち 8,000 しか買えていなければ、9 月の目標に 2,000 を足す。"""
    placed = {AUG: Decimal(8_000), SEP: Decimal(0)}
    (c,) = pending_contributions(
        _config(),
        {"T": BARS},
        now=NOW,
        placed=lambda _s, m: placed.get(m, Decimal(0)),
        active=lambda _s, m: m == AUG,
    ).contributions
    assert c.amount == Decimal(12_000)
    assert "繰り越し 2,000" in c.reason


def test_nothing_is_carried_without_real_orders_last_month() -> None:
    """前月が dry-run だけ（発注記録なし）なら、その分まで買わない。"""
    (c,) = pending_contributions(
        _config(),
        {"T": BARS},
        now=NOW,
        placed=lambda _s, _m: Decimal(0),
        active=lambda _s, _m: False,
    ).contributions
    assert c.amount == Decimal(10_000)


def test_overfilled_previous_month_does_not_reduce_this_month() -> None:
    placed = {AUG: Decimal(11_000)}
    (c,) = pending_contributions(
        _config(),
        {"T": BARS},
        now=NOW,
        placed=lambda _s, m: placed.get(m, Decimal(0)),
        active=lambda _s, _m: True,
    ).contributions
    assert c.amount == Decimal(10_000)


def test_ledger_symbol_matches_the_broker_notation() -> None:
    """設定の 1305.T は台帳では 1305（ブローカーの表記）。揃えないと二重買付になる。"""
    config = AccumConfig.model_validate(
        {
            "tactics": [
                {
                    "id": "jp",
                    "tactic": "constant",
                    "symbols": ["1305.T"],
                    "market": "JP",
                    "monthly_budget": 10_000,
                },
                {
                    "id": "us",
                    "tactic": "constant",
                    "symbols": ["VOO"],
                    "market": "US",
                    "monthly_budget": 10_000,
                },
            ]
        }
    )
    assert ledger_symbol(config, "1305.T") == "1305"
    assert ledger_symbol(config, "VOO") == "VOO"
    assert ledger_symbol(config, "unknown") == "unknown"
