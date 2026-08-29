"""ライブの積立の規則: 「今月の目標（今日まで）− 今月の発注済み」。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import polars as pl
import pytest

from accum.config import AccumConfig
from accum.execute import build_orders, pending_contributions, to_order
from accum.ledger import DRY_RUN_STATUS, Ledger
from wbcore.domain.models import Fill, Order, OrderStatus, OrderType, Side, TaxAccountType

JST = dt.timezone(dt.timedelta(hours=9))


def _bars(closes: list[float], *, start: dt.date) -> pl.DataFrame:
    dates: list[dt.date] = []
    day = start
    while len(dates) < len(closes):
        if day.weekday() < 5:
            dates.append(day)
        day += dt.timedelta(days=1)
    return pl.DataFrame(
        {
            "date": dates,
            "open": closes,
            "high": [c * 1.01 for c in closes],
            "low": [c * 0.99 for c in closes],
            "close": closes,
            "volume": [1.0] * len(closes),
        }
    )


def _config(budget: int = 10_000, tactic: str = "constant", **fields) -> AccumConfig:  # type: ignore[no-untyped-def]
    return AccumConfig.model_validate(
        {
            "monthly_budget": budget,
            "tactics": [{"id": "a", "tactic": tactic, "symbols": ["T"], "window": False, **fields}],
        }
    )


def _at(day: dt.date, hour: int = 14) -> dt.datetime:
    return dt.datetime.combine(day, dt.time(hour, 5), tzinfo=JST)


# 8 月の入金日は 8/3（月）。足は 7/1 から。
PAYDAY = dt.date(2026, 8, 3)
BARS = _bars([100.0] * 45, start=dt.date(2026, 7, 1))  # 7/1 〜 9/1


def _placed(**by_symbol_month: Decimal):  # type: ignore[no-untyped-def]
    def lookup(symbol: str, month: dt.date) -> Decimal:
        return by_symbol_month.get(f"{symbol}_{month:%Y%m}", Decimal(0))

    return lookup


def test_starting_mid_month_invests_the_whole_budget_on_the_first_run() -> None:
    """月の途中から始めても、入金日を過ぎていれば最初に注文を出せる日に全額。"""
    (c,) = pending_contributions(
        _config(), {"T": BARS}, now=_at(dt.date(2026, 8, 20))
    ).contributions
    assert c.amount == Decimal(10_000)
    assert c.month == dt.date(2026, 8, 1)
    assert c.date == dt.date(2026, 8, 20)  # 判断日＝今日。買う価格も今日
    assert "基本 10,000" in c.reason


def test_nothing_is_due_once_the_month_is_fully_placed() -> None:
    pending = pending_contributions(
        _config(),
        {"T": BARS},
        now=_at(dt.date(2026, 8, 20)),
        placed=_placed(T_202608=Decimal(10_000)),
    )
    assert pending.contributions == []


def test_budget_increase_mid_month_adds_only_the_difference() -> None:
    (c,) = pending_contributions(
        _config(budget=15_000),
        {"T": BARS},
        now=_at(dt.date(2026, 8, 20)),
        placed=_placed(T_202608=Decimal(10_000)),
    ).contributions
    assert c.amount == Decimal(5_000)
    assert "発注済み 10,000" in c.reason


def test_payday_itself_is_not_due_until_its_bar_closes() -> None:
    """入金日当日は、その日の足がまだ途中経過なので判定しない。翌日に出る。"""
    assert pending_contributions(_config(), {"T": BARS}, now=_at(PAYDAY)).contributions == []
    (c,) = pending_contributions(_config(), {"T": BARS}, now=_at(dt.date(2026, 8, 4))).contributions
    assert c.amount == Decimal(10_000)


def test_month_boundary_resets_the_target() -> None:
    """8 月ぶんを発注済みでも、9 月の入金日（9/1）が確定すれば 9 月の目標が立つ。"""
    placed = _placed(T_202608=Decimal(10_000))
    assert (
        pending_contributions(
            _config(), {"T": BARS}, now=_at(dt.date(2026, 9, 1)), placed=placed
        ).contributions
        == []
    )
    (c,) = pending_contributions(
        _config(), {"T": BARS}, now=_at(dt.date(2026, 9, 2)), placed=placed
    ).contributions
    assert c.month == dt.date(2026, 9, 1) and c.amount == Decimal(10_000)


def test_extras_accumulate_into_the_target() -> None:
    """増額分は日ごとに目標へ積み上がる。発注済みとの差だけが出る。"""
    n = 260
    falling = _bars([1000.0 - i for i in range(n)], start=dt.date(2025, 8, 1))  # 完全下降配列
    today = falling["date"][-1] + dt.timedelta(days=1)
    config = _config(budget=10_000, tactic="bear_stack", multiplier=4)
    (c,) = pending_contributions(config, {"T": falling}, now=_at(today)).contributions
    assert c.amount > Decimal(10_000)  # 基本＋増額
    assert c.multiplier == 4.0
    # 半分だけ発注済みなら残りだけ
    half = c.amount / 2
    (rest,) = pending_contributions(
        config, {"T": falling}, now=_at(today), placed=lambda s, m: half
    ).contributions
    assert rest.amount == c.amount - half


def test_stale_bars_are_not_judged_and_are_reported() -> None:
    pending = pending_contributions(
        _config(),
        {"T": BARS.filter(pl.col("date") <= PAYDAY)},
        now=_at(dt.date(2026, 8, 12)),
        max_stale_days=4,
    )
    assert pending.contributions == [] and pending.stale == {"T": PAYDAY}


def test_ledger_sums_placed_amount_per_symbol_and_month(tmp_path: Path) -> None:
    """台帳の月次集計。dry-run は数えず、古い台帳ファイルにも列を足せる。"""
    path = tmp_path / "ledger.db"
    (c,) = pending_contributions(
        _config(), {"T": BARS}, now=_at(dt.date(2026, 8, 20))
    ).contributions
    order = to_order(c, tax_type=TaxAccountType.SPECIFIC, lot_size=1)
    with Ledger(path) as ledger:
        ledger.record(order, DRY_RUN_STATUS, plan_month=c.month, amount=c.amount)
        assert ledger.placed_amount("T", dt.date(2026, 8, 1)) == 0
        ledger.record(order, "SUBMITTED", "B1", plan_month=c.month, amount=c.amount)
        assert ledger.placed_amount("T", dt.date(2026, 8, 15)) == Decimal(10_000)
        assert ledger.placed_amount("T", dt.date(2026, 9, 1)) == 0
    # 台帳を使えば、同じ日の 2 回目は差が 0
    with Ledger(path) as ledger:
        again = pending_contributions(
            _config(), {"T": BARS}, now=_at(dt.date(2026, 8, 20)), placed=ledger.placed_amount
        )
    assert again.contributions == []


def test_ledger_migrates_an_old_schema(tmp_path: Path) -> None:
    import sqlite3

    path = tmp_path / "old.db"
    con = sqlite3.connect(path)
    con.execute(
        "CREATE TABLE orders (client_order_id TEXT PRIMARY KEY, broker_order_id TEXT, symbol TEXT NOT NULL,"
        " quantity TEXT NOT NULL, status TEXT NOT NULL, reason TEXT, placed_at TEXT NOT NULL)"
    )
    con.commit()
    con.close()
    with Ledger(path) as ledger:
        assert ledger.placed_amount("T", dt.date(2026, 8, 1)) == 0


def test_orders_are_built_from_the_due_amount() -> None:
    (c,) = pending_contributions(
        _config(), {"T": BARS}, now=_at(dt.date(2026, 8, 20))
    ).contributions
    (planned,) = build_orders([c], tax_type=TaxAccountType.SPECIFIC, lot_sizes={"T": 1})
    assert planned.request is not None and planned.request.quantity == Decimal(100)  # 10,000 / 100


# --- 時刻の規約: モデルは tz 無しの時刻を受け付けない ---


def test_domain_models_reject_naive_timestamps() -> None:
    with pytest.raises(ValueError, match="時間帯が必要"):
        Order(
            client_order_id="x",
            broker_order_id=None,
            symbol="T",
            side=Side.BUY,
            order_type=OrderType.MARKET,
            quantity=Decimal(1),
            filled_quantity=Decimal(0),
            status=OrderStatus.SUBMITTED,
            created_at=dt.datetime(2026, 8, 3, 5, 5),
        )
    with pytest.raises(ValueError, match="時間帯が必要"):
        Fill("x", "T", Side.BUY, Decimal(1), Decimal(100), filled_at=dt.datetime(2026, 8, 3))
    assert Fill("x", "T", Side.BUY, Decimal(1), Decimal(100), filled_at=_at(PAYDAY)).filled_at
