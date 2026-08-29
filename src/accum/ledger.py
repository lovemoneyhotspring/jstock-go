"""積立の発注台帳。「今月いくら発注済みか」と「送った注文がどうなったか」を実行をまたいで覚える。

**なぜ要るのか**

積立の判断は「今月の目標（今日まで） − 今月の発注済み」で決める
（:func:`accum.execute.pending_contributions`）。目標は足から毎回計算できるが、
発注済みは台帳にしか無い。台帳が無ければ、月の途中で予算を増やしたときに
差額ではなく全額をもう一度買い、cron が二重に走れば二重に買う。

**「送った」と「約定した」は違う**

送った注文が拒否・失効すれば、その月の投下は足りていない。台帳は送った
記録に加えて約定状況（:meth:`update_status`）を持ち、集計
（:meth:`placed_amount`）では生きている注文と約定した分だけを数える。
失効した注文の未約定分は数えないので、次の実行で差額が自動的に出る。

dry-run は数えない。確認のために dry-run した日の本発注が抑止されてしまう。
"""

from __future__ import annotations

import datetime as dt
import sqlite3
from dataclasses import dataclass
from decimal import Decimal
from pathlib import Path

from wbcore.clock import now_utc
from wbcore.domain.models import Market, OrderRequest, OrderStatus

#: dry-run の記録に付ける状態。集計と照会から除外する。
DRY_RUN_STATUS = "dry_run"

#: 未約定のまま終わった状態。未約定分は「発注済み」に数えない。
_DEAD = {OrderStatus.CANCELLED.value, OrderStatus.REJECTED.value, OrderStatus.EXPIRED.value}


@dataclass(frozen=True, slots=True)
class LedgerOrder:
    """台帳の 1 行。"""

    client_order_id: str
    broker_order_id: str | None
    symbol: str
    market: Market | None
    quantity: Decimal
    filled_quantity: Decimal
    status: str
    amount: Decimal | None
    plan_month: dt.date | None
    placed_at: str
    updated_at: str | None
    avg_fill_price: Decimal | None

    @property
    def is_open(self) -> bool:
        """まだ結果が確定していない（照会が要る）。"""
        return self.status != DRY_RUN_STATUS and not OrderStatus(self.status).is_terminal

    @property
    def effective_amount(self) -> Decimal:
        """「発注済み」として数える額。

        生きている注文と約定した注文は全額。未約定のまま終わった注文は、
        部分約定ぶんだけ（``amount × 約定数量 ÷ 数量``）。
        """
        if self.amount is None or self.status == DRY_RUN_STATUS:
            return Decimal(0)
        if self.status in _DEAD:
            if self.quantity <= 0:
                return Decimal(0)
            return self.amount * self.filled_quantity / self.quantity
        return self.amount


class Ledger:
    """SQLite の発注台帳。1環境1ファイル（``data/accum-<env>.db``）。"""

    _COLUMNS = (
        ("plan_month", "TEXT"),
        ("amount", "TEXT"),
        ("market", "TEXT"),
        ("filled_quantity", "TEXT"),
        ("avg_fill_price", "TEXT"),
        ("updated_at", "TEXT"),
    )

    def __init__(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self._connection = sqlite3.connect(path)
        self._connection.row_factory = sqlite3.Row
        self._connection.execute(
            "CREATE TABLE IF NOT EXISTS orders ("
            " client_order_id TEXT PRIMARY KEY,"
            " broker_order_id TEXT,"
            " symbol TEXT NOT NULL,"
            " quantity TEXT NOT NULL,"
            " status TEXT NOT NULL,"
            " reason TEXT,"
            " placed_at TEXT NOT NULL)"
        )
        self._connection.execute(
            "CREATE TABLE IF NOT EXISTS accumulation ("
            " symbol TEXT PRIMARY KEY,"
            " started_on TEXT NOT NULL)"
        )
        self._migrate()
        self._connection.commit()

    def _migrate(self) -> None:
        """``CREATE TABLE IF NOT EXISTS`` は既存テーブルの列を増やさないので、足りない列を足す。"""
        existing = {
            row["name"] for row in self._connection.execute("PRAGMA table_info(orders)").fetchall()
        }
        for column, kind in self._COLUMNS:
            if column not in existing:
                self._connection.execute(f"ALTER TABLE orders ADD COLUMN {column} {kind}")

    def close(self) -> None:
        self._connection.close()

    def __enter__(self) -> Ledger:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    # -- 読み出し -----------------------------------------------------------

    @staticmethod
    def _row(row: sqlite3.Row) -> LedgerOrder:
        return LedgerOrder(
            client_order_id=row["client_order_id"],
            broker_order_id=row["broker_order_id"],
            symbol=row["symbol"],
            market=Market(row["market"]) if row["market"] else None,
            quantity=Decimal(row["quantity"]),
            filled_quantity=Decimal(row["filled_quantity"] or 0),
            status=row["status"],
            amount=Decimal(row["amount"]) if row["amount"] else None,
            plan_month=dt.date.fromisoformat(row["plan_month"]) if row["plan_month"] else None,
            placed_at=row["placed_at"],
            updated_at=row["updated_at"],
            avg_fill_price=Decimal(row["avg_fill_price"]) if row["avg_fill_price"] else None,
        )

    def was_placed(self, client_order_id: str) -> bool:
        """すでに**ブローカーへ送った**注文か。dry-run の記録は数えない。"""
        row = self._connection.execute(
            "SELECT 1 FROM orders WHERE client_order_id = ? AND status != ?",
            (client_order_id, DRY_RUN_STATUS),
        ).fetchone()
        return row is not None

    def placed_amount(self, symbol: str, month: dt.date) -> Decimal:
        """その銘柄に、その月ぶんとして**有効に**送った投下額の合計。

        生きている注文と約定した注文は全額、失効・拒否は約定ぶんだけ、
        dry-run は 0（:attr:`LedgerOrder.effective_amount`）。
        ``month`` は月初の日付。
        """
        rows = self._connection.execute(
            "SELECT * FROM orders WHERE symbol = ? AND plan_month = ?",
            (symbol, month.replace(day=1).isoformat()),
        ).fetchall()
        return sum((self._row(r).effective_amount for r in rows), start=Decimal(0))

    def has_orders(self, symbol: str, month: dt.date) -> bool:
        """その銘柄・その月に**ブローカーへ送った**記録があるか（dry-run は数えない）。

        前月の残り（目標 − 発注済み）を当月に繰り越すかの判断に使う。
        dry-run しかしていない月や、動いていなかった月の分まで繰り越すと、
        本稼働の初月に前月ぶんを二重に買うことになる。
        """
        row = self._connection.execute(
            "SELECT 1 FROM orders WHERE symbol = ? AND plan_month = ? AND status != ? LIMIT 1",
            (symbol, month.replace(day=1).isoformat(), DRY_RUN_STATUS),
        ).fetchone()
        return row is not None

    def open_orders(self) -> list[LedgerOrder]:
        """結果が確定していない注文（ブローカーへの照会が要る）。"""
        rows = self._connection.execute(
            "SELECT * FROM orders WHERE status != ? ORDER BY placed_at", (DRY_RUN_STATUS,)
        ).fetchall()
        return [o for o in (self._row(r) for r in rows) if o.is_open]

    def recent(self, limit: int = 20) -> list[LedgerOrder]:
        """直近の記録。新しい順。"""
        rows = self._connection.execute(
            "SELECT * FROM orders ORDER BY placed_at DESC LIMIT ?", (limit,)
        ).fetchall()
        return [self._row(r) for r in rows]

    # -- 開始日 -------------------------------------------------------------

    def started_on(self, symbol: str) -> dt.date | None:
        """その銘柄の積立を始めた日。まだ本発注に至っていなければ None。

        開始月の基本予算を残り日数で日割りするために使う。
        """
        row = self._connection.execute(
            "SELECT started_on FROM accumulation WHERE symbol = ?", (symbol,)
        ).fetchone()
        return dt.date.fromisoformat(row["started_on"]) if row else None

    def mark_started(self, symbol: str, day: dt.date) -> None:
        """開始日を記録する。すでにあれば変えない（最初の日が開始日）。"""
        self._connection.execute(
            "INSERT OR IGNORE INTO accumulation (symbol, started_on) VALUES (?, ?)",
            (symbol, day.isoformat()),
        )
        self._connection.commit()

    # -- 書き込み -----------------------------------------------------------

    def record(
        self,
        request: OrderRequest,
        status: str,
        broker_order_id: str | None = None,
        *,
        plan_month: dt.date | None = None,
        amount: Decimal | None = None,
        market: Market | None = None,
    ) -> None:
        """発注の結果を残す。同じIDなら上書き（dry-run → 本発注 の順で来る）。

        Args:
            plan_month: この注文がどの月の積立か（月初の日付）。
            amount: 投下額（口座通貨）。株数×価格ではなく、判断した金額そのもの。
                月の発注済みの集計に使うので、予算の単位で持つ。
            market: 照会するときにどの市場のブローカーに繋ぐか。
        """
        self._connection.execute(
            "INSERT OR REPLACE INTO orders "
            "(client_order_id, broker_order_id, symbol, quantity, status, reason, placed_at,"
            " plan_month, amount, market, filled_quantity, avg_fill_price, updated_at)"
            " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                request.client_order_id,
                broker_order_id,
                request.symbol,
                str(request.quantity),
                status,
                request.reason,
                now_utc().isoformat(timespec="seconds"),
                plan_month.replace(day=1).isoformat() if plan_month else None,
                str(amount) if amount is not None else None,
                market.value if market else None,
                "0",
                None,
                None,
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
        """ブローカーに照会した結果で約定状況を更新する。"""
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
