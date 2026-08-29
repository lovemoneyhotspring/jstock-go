"""発注は台帳に**先に**記録する。送信→記録の隙間で落ちても、次回に再送しない。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import pytest

from accum.cli import UnconfirmedOrderError, _place_recorded
from accum.execute import Contribution
from accum.ledger import Ledger
from accum.tactics import Constant
from wbcore.broker.base import BrokerError, OrderRejectedError
from wbcore.broker.paper import PaperBroker
from wbcore.domain.models import (
    Market,
    OrderAck,
    OrderRequest,
    OrderStatus,
    OrderType,
    Side,
)

MONTH = dt.date(2026, 8, 1)
CID = "c" * 32


def _contribution() -> Contribution:
    return Contribution(
        symbol="452A.T",
        market=Market.JP,
        date=dt.date(2026, 8, 20),
        close=Decimal("827"),
        amount=Decimal(25_000),
        multiplier=1.0,
        reason="",
        tactic=Constant(),
        month=MONTH,
    )


def _request() -> OrderRequest:
    return OrderRequest(CID, "452A", Side.BUY, OrderType.MARKET, Decimal(30), reason="積立")


class FailingBroker(PaperBroker):
    name = "failing"

    def __init__(self, error: Exception | None = None) -> None:
        super().__init__()
        self.error = error

    def place(self, request: OrderRequest) -> OrderAck:
        if self.error is not None:
            raise self.error
        return OrderAck(request.client_order_id, "B1", OrderStatus.SUBMITTED)


def test_accepted_order_is_recorded_with_broker_status(tmp_path: Path) -> None:
    with Ledger(tmp_path / "l.db") as ledger:
        ack = _place_recorded(FailingBroker(), ledger, _request(), _contribution())
        assert ack.status is OrderStatus.SUBMITTED
        (row,) = ledger.recent()
        assert row.status == "SUBMITTED"
        assert row.broker_order_id == "B1"
        assert ledger.placed_amount("452A", MONTH) == Decimal(30 * 827)  # 株数×価格。端数は繰り越す


def test_rejected_order_does_not_count_as_placed(tmp_path: Path) -> None:
    with Ledger(tmp_path / "l.db") as ledger:
        with pytest.raises(OrderRejectedError):
            _place_recorded(
                FailingBroker(OrderRejectedError("弾かれた")), ledger, _request(), _contribution()
            )
        (row,) = ledger.recent()
        assert row.status == "REJECTED"
        # 次回の差額計算で埋め直される
        assert ledger.placed_amount("452A", MONTH) == Decimal(0)


def test_unconfirmed_order_stays_pending_and_blocks_resend(tmp_path: Path) -> None:
    """応答が返らなかった注文は「送信中」のまま残り、再送されない（二重買付より買い漏れ）。"""
    with Ledger(tmp_path / "l.db") as ledger:
        with pytest.raises(UnconfirmedOrderError) as info:
            _place_recorded(
                FailingBroker(BrokerError("timeout")), ledger, _request(), _contribution()
            )
        assert info.value.client_order_id == CID
        (row,) = ledger.recent()
        assert row.status == "PENDING"
        assert row.is_open  # 次回の run が照会しに行く
        assert ledger.was_placed(CID)
        assert ledger.placed_amount("452A", MONTH) == Decimal(30 * 827)  # 株数×価格。端数は繰り越す
