"""送った注文の約定状況を確かめ、失効・拒否の未約定分を「発注済み」から外す。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

from accum.execute import StatusChange, sync_order_status
from accum.ledger import DRY_RUN_STATUS, Ledger
from wbcore.broker.paper import PaperBroker
from wbcore.clock import now_utc
from wbcore.domain.models import Market, Order, OrderRequest, OrderStatus, OrderType, Side

MONTH = dt.date(2026, 8, 1)


def _request(cid: str, qty: int = 30) -> OrderRequest:
    return OrderRequest(cid, "452A", Side.BUY, OrderType.MARKET, Decimal(qty), reason="積立")


class FakeBroker(PaperBroker):
    """照会だけ差し替える。``answers`` に無い注文は None（見つからない）。"""

    name = "fake"

    def __init__(self, answers: dict[str, Order]) -> None:
        super().__init__()
        self.answers = answers
        self.asked: list[str] = []

    def get_order(self, client_order_id: str) -> Order | None:
        self.asked.append(client_order_id)
        return self.answers.get(client_order_id)


def _order(cid: str, status: OrderStatus, filled: int, qty: int = 30, price: str = "827") -> Order:
    return Order(
        client_order_id=cid,
        broker_order_id="B" + cid,
        symbol="452A",
        side=Side.BUY,
        order_type=OrderType.MARKET,
        quantity=Decimal(qty),
        filled_quantity=Decimal(filled),
        status=status,
        avg_fill_price=Decimal(price) if filled else None,
        created_at=now_utc(),
    )


def test_placed_amount_counts_open_and_filled_but_not_dead_orders(tmp_path: Path) -> None:
    with Ledger(tmp_path / "l.db") as ledger:
        ledger.record(
            _request("a" * 32),
            "SUBMITTED",
            plan_month=MONTH,
            amount=Decimal(25_000),
            market=Market.JP,
        )
        ledger.record(
            _request("b" * 32),
            "SUBMITTED",
            plan_month=MONTH,
            amount=Decimal(10_000),
            market=Market.JP,
        )
        ledger.record(
            _request("c" * 32),
            DRY_RUN_STATUS,
            plan_month=MONTH,
            amount=Decimal(99_999),
            market=Market.JP,
        )
        assert ledger.placed_amount("452A", MONTH) == Decimal(35_000)

        ledger.update_status("a" * 32, OrderStatus.FILLED, filled_quantity=Decimal(30))
        ledger.update_status("b" * 32, OrderStatus.EXPIRED, filled_quantity=Decimal(0))
        assert ledger.placed_amount("452A", MONTH) == Decimal(25_000)  # 失効分は消える

        ledger.update_status("b" * 32, OrderStatus.EXPIRED, filled_quantity=Decimal(15))
        assert ledger.placed_amount("452A", MONTH) == Decimal(30_000)  # 部分約定は按分
        assert [o.client_order_id for o in ledger.open_orders()] == []


def test_sync_updates_open_orders_and_reports_changes(tmp_path: Path) -> None:
    broker = FakeBroker(
        {
            "a" * 32: _order("a" * 32, OrderStatus.FILLED, 30),
            "b" * 32: _order("b" * 32, OrderStatus.EXPIRED, 10),
        }
    )
    with Ledger(tmp_path / "l.db") as ledger:
        for cid in ("a" * 32, "b" * 32, "c" * 32):
            ledger.record(
                _request(cid),
                "SUBMITTED",
                plan_month=MONTH,
                amount=Decimal(25_000),
                market=Market.JP,
            )
        ledger.record(
            _request("d" * 32), "FILLED", plan_month=MONTH, amount=Decimal(1), market=Market.JP
        )

        changes = sync_order_status(ledger, lambda market: broker)

        assert sorted(broker.asked) == ["a" * 32, "b" * 32, "c" * 32]  # 確定済みの d は照会しない
        by_id = {c.client_order_id: c for c in changes}
        assert (
            by_id["a" * 32].after is OrderStatus.FILLED and by_id["a" * 32].lost_amount_ratio == 0
        )
        assert by_id["b" * 32].after is OrderStatus.EXPIRED
        assert by_id["b" * 32].lost_amount_ratio == Decimal(20) / Decimal(30)
        assert "次回に持ち越し" in by_id["b" * 32].describe()
        assert "c" * 32 not in by_id  # 見つからない注文は変えない（勝手に失効にしない）
        assert [o.client_order_id for o in ledger.open_orders()] == ["c" * 32]

        # 発注済み: a は全額、b は 10/30、c は生きているので全額
        expected = Decimal(25_000) + Decimal(25_000) * Decimal(10) / Decimal(30) + Decimal(25_000)
        assert ledger.placed_amount("452A", MONTH) + Decimal(0) == expected + Decimal(1)


def test_sync_is_a_noop_without_open_orders(tmp_path: Path) -> None:
    broker = FakeBroker({})
    with Ledger(tmp_path / "l.db") as ledger:
        assert sync_order_status(ledger, lambda market: broker) == []
    assert broker.asked == []


def test_status_change_describe_for_a_full_fill() -> None:
    change = StatusChange("x", "452A", "SUBMITTED", OrderStatus.FILLED, Decimal(30), Decimal(30))
    assert change.describe() == "452A: FILLED（30/30 約定）"
