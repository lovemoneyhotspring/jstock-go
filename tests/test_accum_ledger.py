"""積立の発注台帳。実行をまたいで同じ注文を二度送らないための記録。"""

from __future__ import annotations

from decimal import Decimal
from pathlib import Path

from accum.ledger import DRY_RUN_STATUS, Ledger
from wbcore.domain.models import OrderRequest, OrderType, Side


def _request(client_order_id: str = "a" * 32) -> OrderRequest:
    return OrderRequest(
        client_order_id=client_order_id,
        symbol="1305",
        side=Side.BUY,
        order_type=OrderType.MARKET,
        quantity=Decimal(10),
        reason="積立",
    )


def test_placed_order_is_remembered_across_instances(tmp_path: Path) -> None:
    """cron の次回実行は別プロセス。ファイルに残っていなければ意味がない。"""
    path = tmp_path / "ledger.db"
    with Ledger(path) as ledger:
        assert not ledger.was_placed("a" * 32)
        ledger.record(_request(), "SUBMITTED", broker_order_id="B1")
    with Ledger(path) as ledger:
        assert ledger.was_placed("a" * 32)
        assert ledger.recent()[0].broker_order_id == "B1"


def test_dry_run_does_not_block_the_real_order(tmp_path: Path) -> None:
    """確認のために dry-run した日の本発注を潰さない。"""
    with Ledger(tmp_path / "ledger.db") as ledger:
        ledger.record(_request(), DRY_RUN_STATUS)
        assert not ledger.was_placed("a" * 32)
        ledger.record(_request(), "FILLED")
        assert ledger.was_placed("a" * 32)


def test_ledger_creates_parent_directory(tmp_path: Path) -> None:
    with Ledger(tmp_path / "nested" / "dir" / "ledger.db") as ledger:
        assert ledger.recent() == []


def test_rejected_order_can_be_resent_the_same_day(tmp_path: Path) -> None:
    """一時的な拒否で当日の投下を丸ごと翌日に持ち越さない。同じ ID を出し直せる。"""
    from wbcore.domain.models import OrderStatus

    with Ledger(tmp_path / "ledger.db") as ledger:
        ledger.record(_request(), OrderStatus.PENDING.value)
        assert ledger.was_placed("a" * 32)
        ledger.update_status("a" * 32, OrderStatus.REJECTED)
        assert not ledger.was_placed("a" * 32)
