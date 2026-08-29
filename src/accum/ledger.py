"""積立の発注台帳。「今月いくら発注済みか」と「この注文はもう送ったか」を実行をまたいで覚える。

**なぜ要るのか**

積立の判断は「今月の目標（今日まで） − 今月の発注済み」で決める
（:func:`accum.execute.pending_contributions`）。目標は足から毎回計算できるが、
発注済みは台帳にしか無い。台帳が無ければ、月の途中で予算を増やしたときに
差額ではなく全額をもう一度買い、cron が二重に走れば二重に買う。

祝日や yfinance に当日足がまだ無い時刻には前回と同じ判断が再生されるが、
発注済みの額が目標に達していれば差が 0 になり、何も出ない。

dry-run は数えない。確認のために dry-run した日の本発注が抑止されてしまう。
"""

from __future__ import annotations

import datetime as dt
import sqlite3
from decimal import Decimal
from pathlib import Path

from wbcore.clock import now_utc
from wbcore.domain.models import OrderRequest

#: dry-run の記録に付ける状態。``was_placed`` / ``placed_amount`` はこれを除外する。
DRY_RUN_STATUS = "dry_run"


class Ledger:
    """SQLite の発注台帳。1環境1ファイル（``data/accum-<env>.db``）。"""

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
            " placed_at TEXT NOT NULL,"
            " plan_month TEXT,"
            " amount TEXT)"
        )
        self._migrate()
        self._connection.commit()

    def _migrate(self) -> None:
        """``CREATE TABLE IF NOT EXISTS`` は既存テーブルの列を増やさないので、足りない列を足す。"""
        existing = {
            row["name"] for row in self._connection.execute("PRAGMA table_info(orders)").fetchall()
        }
        for column in ("plan_month", "amount"):
            if column not in existing:
                self._connection.execute(f"ALTER TABLE orders ADD COLUMN {column} TEXT")

    def close(self) -> None:
        self._connection.close()

    def __enter__(self) -> Ledger:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def was_placed(self, client_order_id: str) -> bool:
        """すでに**ブローカーへ送った**注文か。dry-run の記録は数えない。"""
        row = self._connection.execute(
            "SELECT 1 FROM orders WHERE client_order_id = ? AND status != ?",
            (client_order_id, DRY_RUN_STATUS),
        ).fetchone()
        return row is not None

    def placed_amount(self, symbol: str, month: dt.date) -> Decimal:
        """その銘柄に、その月ぶんとして**送った**投下額の合計。dry-run は数えない。

        ``month`` は月初の日付（``date.replace(day=1)``）。
        """
        rows = self._connection.execute(
            "SELECT amount FROM orders WHERE symbol = ? AND plan_month = ? AND status != ?",
            (symbol, month.replace(day=1).isoformat(), DRY_RUN_STATUS),
        ).fetchall()
        return sum((Decimal(r["amount"]) for r in rows if r["amount"]), start=Decimal(0))

    def record(
        self,
        request: OrderRequest,
        status: str,
        broker_order_id: str | None = None,
        *,
        plan_month: dt.date | None = None,
        amount: Decimal | None = None,
    ) -> None:
        """発注の結果を残す。同じIDなら上書き（dry-run → 本発注 の順で来る）。

        Args:
            plan_month: この注文がどの月の積立か（月初の日付）。
            amount: 投下額（口座通貨）。株数×価格ではなく、判断した金額そのもの。
                月の発注済みの集計に使うので、予算の単位で持つ。
        """
        self._connection.execute(
            "INSERT OR REPLACE INTO orders "
            "(client_order_id, broker_order_id, symbol, quantity, status, reason, placed_at,"
            " plan_month, amount) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
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
            ),
        )
        self._connection.commit()

    def recent(self, limit: int = 20) -> list[sqlite3.Row]:
        """直近の記録。新しい順。"""
        return list(
            self._connection.execute(
                "SELECT * FROM orders ORDER BY placed_at DESC LIMIT ?", (limit,)
            ).fetchall()
        )
