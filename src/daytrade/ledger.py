"""デイトレの発注台帳。「今日もう買ったか」「何を手仕舞うべきか」を実行をまたいで覚える。

cron は ``open`` と ``close`` を別プロセスで呼ぶ。``close`` が売るべき数量は、
``open`` が送った注文とその約定状況にしか無い。ブローカーの建玉を無条件に売ると、
他の戦略（積立）の保有まで手放す。
"""

from __future__ import annotations

import datetime as dt
import sqlite3
from dataclasses import dataclass
from decimal import Decimal
from pathlib import Path

from wbcore.clock import now_utc
from wbcore.domain.models import OrderRequest, OrderStatus, Side

#: dry-run の記録に付ける状態。「発注済み」には数えない。
DRY_RUN_STATUS = "dry_run"


@dataclass(frozen=True, slots=True)
class LedgerOrder:
    client_order_id: str
    broker_order_id: str | None
    day: dt.date
    symbol: str
    side: Side
    quantity: Decimal
    filled_quantity: Decimal
    status: str
    price: Decimal | None
    avg_fill_price: Decimal | None
    placed_at: str
    updated_at: str | None
    reason: str

    @property
    def is_dry_run(self) -> bool:
        return self.status == DRY_RUN_STATUS

    @property
    def is_open(self) -> bool:
        """結果が確定していない（照会が要る）。"""
        return not self.is_dry_run and not OrderStatus(self.status).is_terminal

    @property
    def is_dead(self) -> bool:
        return not self.is_dry_run and self.status in {
            OrderStatus.CANCELLED.value,
            OrderStatus.REJECTED.value,
            OrderStatus.EXPIRED.value,
        }


class Ledger:
    """SQLite の台帳。1 環境 1 ファイル（``state/daytrade-<env>.db``）。"""

    def __init__(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self._connection = sqlite3.connect(path)
        self._connection.row_factory = sqlite3.Row
        self._connection.execute(
            "CREATE TABLE IF NOT EXISTS orders ("
            " client_order_id TEXT PRIMARY KEY,"
            " broker_order_id TEXT,"
            " day TEXT NOT NULL,"
            " symbol TEXT NOT NULL,"
            " side TEXT NOT NULL,"
            " quantity TEXT NOT NULL,"
            " filled_quantity TEXT NOT NULL DEFAULT '0',"
            " status TEXT NOT NULL,"
            " price TEXT,"
            " avg_fill_price TEXT,"
            " reason TEXT,"
            " placed_at TEXT NOT NULL,"
            " updated_at TEXT)"
        )
        self._connection.execute("CREATE INDEX IF NOT EXISTS orders_day ON orders(day, side)")
        self._connection.commit()

    def close(self) -> None:
        self._connection.close()

    def __enter__(self) -> Ledger:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def backup(self, destination: Path) -> Path:
        destination.parent.mkdir(parents=True, exist_ok=True)
        copy = sqlite3.connect(destination)
        try:
            self._connection.backup(copy)
        finally:
            copy.close()
        return destination

    @staticmethod
    def _row(row: sqlite3.Row) -> LedgerOrder:
        return LedgerOrder(
            client_order_id=row["client_order_id"],
            broker_order_id=row["broker_order_id"],
            day=dt.date.fromisoformat(row["day"]),
            symbol=row["symbol"],
            side=Side(row["side"]),
            quantity=Decimal(row["quantity"]),
            filled_quantity=Decimal(row["filled_quantity"] or "0"),
            status=row["status"],
            price=Decimal(row["price"]) if row["price"] else None,
            avg_fill_price=Decimal(row["avg_fill_price"]) if row["avg_fill_price"] else None,
            placed_at=row["placed_at"],
            updated_at=row["updated_at"],
            reason=row["reason"] or "",
        )

    def record(
        self,
        request: OrderRequest,
        day: dt.date,
        status: str,
        *,
        price: Decimal | None = None,
        broker_order_id: str | None = None,
    ) -> None:
        """発注の結果を残す。同じ ID なら上書き（dry-run → 本発注の順で来る）。"""
        self._connection.execute(
            "INSERT OR REPLACE INTO orders (client_order_id, broker_order_id, day, symbol, side,"
            " quantity, filled_quantity, status, price, avg_fill_price, reason, placed_at, updated_at)"
            " VALUES (?, ?, ?, ?, ?, ?, '0', ?, ?, NULL, ?, ?, NULL)",
            (
                request.client_order_id,
                broker_order_id,
                day.isoformat(),
                request.symbol,
                request.side.value,
                str(request.quantity),
                status,
                str(price) if price is not None else None,
                request.reason,
                now_utc().isoformat(timespec="seconds"),
            ),
        )
        self._connection.commit()

    def update_status(
        self,
        client_order_id: str,
        status: OrderStatus,
        *,
        filled_quantity: Decimal = Decimal(0),
        avg_fill_price: Decimal | None = None,
        broker_order_id: str | None = None,
    ) -> None:
        self._connection.execute(
            "UPDATE orders SET status = ?, filled_quantity = ?, avg_fill_price = ?,"
            " broker_order_id = COALESCE(?, broker_order_id), updated_at = ?"
            " WHERE client_order_id = ?",
            (
                status.value,
                str(filled_quantity),
                str(avg_fill_price) if avg_fill_price is not None else None,
                broker_order_id,
                now_utc().isoformat(timespec="seconds"),
                client_order_id,
            ),
        )
        self._connection.commit()

    def was_placed(self, client_order_id: str) -> bool:
        """本発注として送った記録があるか（dry-run は数えない）。"""
        row = self._connection.execute(
            "SELECT status FROM orders WHERE client_order_id = ?", (client_order_id,)
        ).fetchone()
        return row is not None and row["status"] != DRY_RUN_STATUS

    def orders_on(self, day: dt.date, side: Side | None = None) -> list[LedgerOrder]:
        """その日の注文（dry-run を含む）。"""
        if side is None:
            rows = self._connection.execute(
                "SELECT * FROM orders WHERE day = ? ORDER BY placed_at", (day.isoformat(),)
            ).fetchall()
        else:
            rows = self._connection.execute(
                "SELECT * FROM orders WHERE day = ? AND side = ? ORDER BY placed_at",
                (day.isoformat(), side.value),
            ).fetchall()
        return [self._row(r) for r in rows]

    def open_orders(self) -> list[LedgerOrder]:
        rows = self._connection.execute("SELECT * FROM orders ORDER BY placed_at").fetchall()
        return [o for o in (self._row(r) for r in rows) if o.is_open]

    def recent(self, limit: int = 20) -> list[LedgerOrder]:
        rows = self._connection.execute(
            "SELECT * FROM orders ORDER BY placed_at DESC LIMIT ?", (limit,)
        ).fetchall()
        return [self._row(r) for r in rows]
