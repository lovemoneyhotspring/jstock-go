"""積立の発注台帳。「この注文はもうブローカーへ送ったか」を実行をまたいで覚える。

**なぜ要るのか**

積立の注文IDは「取引日 × 銘柄 × 株数」から決定論的に作る。同じ日に cron が
二重に走っても同じIDになるが、それを**弾く側**が無ければ意味がない。
ブローカーが重複IDを拒否するかは実機で未確認で、しかも次のような日は
足が増えないので前回と同じ判断が再生される:

- 祝日（東証は休み、cron は動く）
- yfinance に当日足がまだ無い時刻

入金日の翌日が祝日なら、台帳が無いと入金日の注文がもう一度出る。
スイング売買側は :class:`wbjp.db.repo.Journal` が同じ役目を担っている。

dry-run は数えない。確認のために dry-run した日の本発注が抑止されてしまう。
"""

from __future__ import annotations

import datetime as dt
import sqlite3
from pathlib import Path

from wbcore.domain.models import OrderRequest

#: dry-run の記録に付ける状態。``was_placed`` はこれを除外する。
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
            " placed_at TEXT NOT NULL)"
        )
        self._connection.commit()

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

    def record(
        self, request: OrderRequest, status: str, broker_order_id: str | None = None
    ) -> None:
        """発注の結果を残す。同じIDなら上書き（dry-run → 本発注 の順で来る）。"""
        self._connection.execute(
            "INSERT OR REPLACE INTO orders "
            "(client_order_id, broker_order_id, symbol, quantity, status, reason, placed_at) "
            "VALUES (?, ?, ?, ?, ?, ?, ?)",
            (
                request.client_order_id,
                broker_order_id,
                request.symbol,
                str(request.quantity),
                status,
                request.reason,
                dt.datetime.now(dt.UTC).isoformat(timespec="seconds"),
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
