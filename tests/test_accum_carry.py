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


def test_carry_over_uses_the_prorated_target_of_the_start_month() -> None:
    """8/16 開始（日割り 5,000）で 5,000 を買えていれば、9 月に繰り越しは無い。

    満額 10,000 で計算すると 5,000 が「買い残し」に見え、9 月に二重に買う。
    """
    placed = {AUG: Decimal(5_000), SEP: Decimal(0)}
    (c,) = pending_contributions(
        _config(),
        {"T": BARS},
        now=NOW,
        placed=lambda _s, m: placed.get(m, Decimal(0)),
        active=lambda _s, m: m == AUG,
        started=lambda _s: dt.date(2026, 8, 16),  # 残り 16/31 日 → 5,161
    ).contributions
    assert c.amount == Decimal(10_000) + (Decimal(5_161) - Decimal(5_000))
    assert "繰り越し 161" in c.reason


def test_stale_signal_bars_are_reported_but_do_not_stop_the_contribution() -> None:
    """判定用の足が古くても投下は止めない（倍率が古いだけ）。ただし警告する。"""
    signal = BARS.filter(pl.col("date") < dt.date(2026, 8, 20))
    pending = pending_contributions(
        _config(signal_symbol="S", monthly_budget=10_000),
        {"T": BARS, "S": signal},
        now=NOW,
        placed=lambda _s, _m: Decimal(0),
    )
    assert pending.stale_signals == {"S": dt.date(2026, 8, 19)}
    assert [c.amount for c in pending.contributions] == [Decimal(10_000)]
