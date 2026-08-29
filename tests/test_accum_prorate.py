"""開始月の基本予算は残り日数で日割りする。翌月からは全額。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import polars as pl

from accum.config import AccumConfig
from accum.execute import pending_contributions
from accum.ledger import Ledger

JST = dt.timezone(dt.timedelta(hours=9))


def _bars(n: int, start: dt.date) -> pl.DataFrame:
    dates: list[dt.date] = []
    day = start
    while len(dates) < n:
        if day.weekday() < 5:
            dates.append(day)
        day += dt.timedelta(days=1)
    return pl.DataFrame({"date": dates, "close": [100.0] * n})


CONFIG = AccumConfig.model_validate(
    {
        "monthly_budget": 25_000,
        "tactics": [{"id": "a", "tactic": "constant", "symbols": ["T"], "window": False}],
    }
)
BARS = _bars(70, dt.date(2026, 8, 1))  # 8 月〜11 月上旬


def _at(day: dt.date) -> dt.datetime:
    return dt.datetime.combine(day, dt.time(14, 5), tzinfo=JST)


def test_start_month_is_prorated_by_remaining_calendar_days() -> None:
    """9/16 開始（9 月は 30 日）→ 残り 15 日 → 25,000 × 15/30 = 12,500。"""
    (c,) = pending_contributions(
        CONFIG, {"T": BARS}, now=_at(dt.date(2026, 9, 16)), started=lambda _s: dt.date(2026, 9, 16)
    ).contributions
    assert c.amount == Decimal(12_500)
    assert "残り 15/30 日で日割り" in c.reason


def test_following_month_is_charged_in_full() -> None:
    (c,) = pending_contributions(
        CONFIG, {"T": BARS}, now=_at(dt.date(2026, 10, 5)), started=lambda _s: dt.date(2026, 9, 16)
    ).contributions
    assert c.amount == Decimal(25_000)
    assert "日割り" not in c.reason


def test_starting_on_the_first_is_not_prorated() -> None:
    (c,) = pending_contributions(
        CONFIG, {"T": BARS}, now=_at(dt.date(2026, 9, 3)), started=lambda _s: dt.date(2026, 9, 1)
    ).contributions
    assert c.amount == Decimal(25_000)


def test_without_a_start_date_the_full_budget_is_due() -> None:
    """開始日を管理しない呼び出し（テスト・検証用）は従来どおり全額。"""
    (c,) = pending_contributions(CONFIG, {"T": BARS}, now=_at(dt.date(2026, 9, 16))).contributions
    assert c.amount == Decimal(25_000)


def test_ledger_remembers_the_first_start_date_only(tmp_path: Path) -> None:
    with Ledger(tmp_path / "l.db") as ledger:
        assert ledger.started_on("452A") is None
        ledger.mark_started("452A", dt.date(2026, 9, 16))
        ledger.mark_started("452A", dt.date(2026, 10, 1))  # 2 回目は無視
        assert ledger.started_on("452A") == dt.date(2026, 9, 16)
