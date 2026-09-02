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
from wbcore.domain.models import OrderRequest, OrderStatus, Side, TradeType

#: dry-run の記録に付ける状態。「発注済み」には数えない。
DRY_RUN_STATUS = "dry_run"

#: 未約定のまま終わった状態。同じ判断を送り直してよい。
_DEAD = {OrderStatus.CANCELLED.value, OrderStatus.REJECTED.value, OrderStatus.EXPIRED.value}


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
    #: 現物 / 信用新規 / 信用返済。古い台帳（列が無い）は現物。
    trade: TradeType = TradeType.CASH

    @property
    def is_dry_run(self) -> bool:
        return self.status == DRY_RUN_STATUS

    @property
    def is_open(self) -> bool:
        """結果が確定していない（照会が要る）。"""
        return not self.is_dry_run and not OrderStatus(self.status).is_terminal

    @property
    def is_dead(self) -> bool:
        return not self.is_dry_run and self.status in _DEAD

    @property
    def is_entry(self) -> bool:
        """建てる側の注文か（現物の買い、信用の新規建て）。手仕舞う側なら偽。"""
        if self.trade is TradeType.MARGIN_OPEN:
            return True
        if self.trade is TradeType.MARGIN_CLOSE:
            return False
        return self.side is Side.BUY

    @property
    def is_exit(self) -> bool:
        return not self.is_entry

    @property
    def leg(self) -> str:
        """``"long"``（買って売る）か ``"short"``（売建てて買い戻す）か。"""
        opens_with_buy = (self.is_entry and self.side is Side.BUY) or (
            self.is_exit and self.side is Side.SELL
        )
        return "long" if opens_with_buy else "short"


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
            " updated_at TEXT,"
            " trade TEXT NOT NULL DEFAULT 'CASH')"
        )
        # 既存の台帳（trade 列が無い）を壊さずに列を足す。既定 CASH = 従来の現物
        columns = {row["name"] for row in self._connection.execute("PRAGMA table_info(orders)")}
        if "trade" not in columns:
            self._connection.execute(
                "ALTER TABLE orders ADD COLUMN trade TEXT NOT NULL DEFAULT 'CASH'"
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
            trade=TradeType(row["trade"] or "CASH"),
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
            " quantity, filled_quantity, status, price, avg_fill_price, reason, placed_at,"
            " updated_at, trade)"
            " VALUES (?, ?, ?, ?, ?, ?, '0', ?, ?, NULL, ?, ?, NULL, ?)",
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
                request.trade.value,
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

    def clear_dry_run(self, day: dt.date) -> int:
        """その日の dry-run の記録を消す（確認のたびに増えて台帳が読みにくくなるため）。"""
        cursor = self._connection.execute(
            "DELETE FROM orders WHERE day = ? AND status = ?", (day.isoformat(), DRY_RUN_STATUS)
        )
        self._connection.commit()
        return int(cursor.rowcount or 0)

    def was_placed(self, client_order_id: str) -> bool:
        """本発注として送り、まだ生きているか約定した記録があるか。

        dry-run は数えない。拒否・取消・失効で終わった注文も数えない——同じ判断を
        もう一度送ってよい（再送は呼び出し側が ID の種を変える）。
        """
        row = self._connection.execute(
            "SELECT status FROM orders WHERE client_order_id = ?", (client_order_id,)
        ).fetchone()
        if row is None or row["status"] == DRY_RUN_STATUS:
            return False
        return row["status"] not in _DEAD

    def dead_count(self, day: dt.date, symbol: str, side: Side) -> int:
        """その日・その銘柄・その売買で、拒否・取消・失効に終わった注文の数（再送の ID の種に使う）。"""
        return sum(1 for o in self.orders_on(day, side) if o.symbol == symbol and o.is_dead)

    def entries_on(self, day: dt.date) -> list[LedgerOrder]:
        """その日の建てる側の注文（dry-run を含む）。現物の買い・信用の新規建て。"""
        return [o for o in self.orders_on(day) if o.is_entry]

    def exits_on(self, day: dt.date) -> list[LedgerOrder]:
        """その日の手仕舞う側の注文（dry-run を含む）。現物の売り・信用の返済。"""
        return [o for o in self.orders_on(day) if o.is_exit]

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


def realized_pnl(
    ledger: Ledger, days: list[dt.date], *, leg: str | None = None
) -> dict[dt.date, float | None]:
    """日ごとの実現損益（円）。建てた注文と手仕舞った注文の単価差 × 数量（手数料は含まない）。

    ロング（買って売る）もショート（売建てて買い戻す）も「売り単価 − 買い単価」で同じ式になる。
    ``leg`` を ``"long"`` / ``"short"`` にするとその脚だけ（資産曲線の合図は**ロング側**で見る）。

    dry-run は数えない。本発注で建てた注文が無い日は 0。建てて約定したのに手仕舞いの
    約定単価が無い（照会前・未約定・記録なし）日は **None**——0 と混ぜると「負けた」と誤読する。
    """
    result: dict[dt.date, float | None] = {}
    for day in days:
        orders = [
            o
            for o in ledger.orders_on(day)
            if not o.is_dry_run and (leg is None or o.leg == leg)
        ]
        entries = {(o.symbol, o.leg): o for o in orders if o.is_entry and not o.is_dead}
        if not entries:
            result[day] = 0.0
            continue
        exits = {(o.symbol, o.leg): o for o in orders if o.is_exit and not o.is_dead}
        total = 0.0
        complete = True
        for key, entry in entries.items():
            if entry.filled_quantity <= 0:
                continue  # 約定していないなら手仕舞う物が無い
            exit_ = exits.get(key)
            if exit_ is None or exit_.avg_fill_price is None or entry.avg_fill_price is None:
                complete = False
                continue
            if entry.side is Side.BUY:
                buy_price, sell_price = entry.avg_fill_price, exit_.avg_fill_price
            else:
                buy_price, sell_price = exit_.avg_fill_price, entry.avg_fill_price
            total += float((sell_price - buy_price) * exit_.filled_quantity)
        result[day] = total if complete else None
    return result
