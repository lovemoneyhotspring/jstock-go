"""判断と注文の永続化。

**計画からの変更**: 当初 SQLAlchemy + Alembic を想定していたが、
標準ライブラリの ``sqlite3`` を直接使う形にした。理由:

    - 単一プロセス・単一ライターで、ORM が解決する問題が存在しない
    - 障害時に ``sqlite3 data/wbjp-prod.db`` で素の SQL を叩いて調べられる
      ことが、この用途では何より重要
    - 依存が1つ減る

将来 PostgreSQL に移すことになったら、この :class:`Journal` の
インターフェースを保ったまま実装だけ差し替えればよい。

金額と数量は TEXT で保存する。SQLite の REAL は倍精度浮動小数なので、
株数や約定代金を通すと丸め誤差が入る。読み出し時に Decimal へ戻す。
"""

from __future__ import annotations

import datetime as dt
import json
import sqlite3
from collections.abc import Iterable, Iterator
from contextlib import contextmanager
from decimal import Decimal
from pathlib import Path
from typing import Any

from wbcore.domain.models import (
    CombinedSignal,
    Fill,
    Order,
    OrderRequest,
    OrderStatus,
    Position,
    Signal,
    TargetPosition,
)
from wbcore.logging import get_logger
from wbjp.risk.stops import Stop

log = get_logger(__name__)

SCHEMA_PATH = Path(__file__).with_name("schema.sql")


def _now() -> str:
    return dt.datetime.now(dt.UTC).isoformat()


def _text(value: Decimal | None) -> str | None:
    return None if value is None else str(value)


def _dec(value: Any) -> Decimal | None:
    return None if value in (None, "") else Decimal(str(value))


class Journal:
    """実行の記録を残す。

    1回の実行（run）を起点に、シグナル・合成結果・目標・注文・約定・
    拒否理由をすべて紐づける。障害調査はここを ``run_id`` で辿る。
    """

    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._connection = sqlite3.connect(self.path, isolation_level=None)
        self._connection.row_factory = sqlite3.Row
        self._create_schema()

    def _create_schema(self) -> None:
        self._connection.executescript(SCHEMA_PATH.read_text(encoding="utf-8"))
        self._migrate()

    def _migrate(self) -> None:
        """後から足した列を既存 DB に追加する。

        ``CREATE TABLE IF NOT EXISTS`` は既存テーブルの列を増やさない。
        列が無いまま INSERT すると毎サイクル落ちるので、起動時に埋める。
        """
        columns = {
            row["name"] for row in self._connection.execute("PRAGMA table_info(stops)").fetchall()
        }
        if "initial_stop_price" not in columns:
            self._connection.execute("ALTER TABLE stops ADD COLUMN initial_stop_price TEXT")
        if "initial_quantity" not in columns:
            self._connection.execute("ALTER TABLE stops ADD COLUMN initial_quantity TEXT")
        if "scaled_out" not in columns:
            self._connection.execute(
                "ALTER TABLE stops ADD COLUMN scaled_out INTEGER NOT NULL DEFAULT 0"
            )

    def close(self) -> None:
        self._connection.close()

    def __enter__(self) -> Journal:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    @contextmanager
    def transaction(self) -> Iterator[sqlite3.Connection]:
        """まとめて書き、途中で失敗したら丸ごと戻す。"""
        self._connection.execute("BEGIN")
        try:
            yield self._connection
        except Exception:
            self._connection.execute("ROLLBACK")
            raise
        else:
            self._connection.execute("COMMIT")

    # -- 実行 ---------------------------------------------------------------

    def start_run(
        self,
        run_id: str,
        as_of: dt.date,
        env: str,
        mode: str,
        equity: Decimal | None = None,
        cash: Decimal | None = None,
    ) -> None:
        self._connection.execute(
            "INSERT OR REPLACE INTO runs "
            "(run_id, started_at, as_of, env, mode, equity, cash, status) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, 'running')",
            (run_id, _now(), as_of.isoformat(), env, mode, _text(equity), _text(cash)),
        )

    def finish_run(self, run_id: str, status: str = "ok", error: str | None = None) -> None:
        self._connection.execute(
            "UPDATE runs SET finished_at = ?, status = ?, error = ? WHERE run_id = ?",
            (_now(), status, error, run_id),
        )

    def get_run(self, run_id: str) -> sqlite3.Row | None:
        cursor = self._connection.execute("SELECT * FROM runs WHERE run_id = ?", (run_id,))
        row: sqlite3.Row | None = cursor.fetchone()
        return row

    def recent_runs(self, limit: int = 20) -> list[sqlite3.Row]:
        cursor = self._connection.execute(
            "SELECT * FROM runs ORDER BY started_at DESC LIMIT ?", (limit,)
        )
        return cursor.fetchall()

    # -- 判断の記録 ---------------------------------------------------------

    def record_signals(self, run_id: str, signals: Iterable[Signal]) -> None:
        self._connection.executemany(
            "INSERT INTO signals (run_id, strategy, symbol, direction, confidence, reason, meta_json) "
            "VALUES (?, ?, ?, ?, ?, ?, ?)",
            [
                (
                    run_id,
                    s.strategy,
                    s.symbol,
                    s.direction,
                    s.confidence,
                    s.reason,
                    json.dumps(s.meta, ensure_ascii=False, default=str),
                )
                for s in signals
            ],
        )

    def record_combined(self, run_id: str, combined: dict[str, CombinedSignal]) -> None:
        self._connection.executemany(
            "INSERT INTO combined_signals (run_id, symbol, direction, contributions_json, reason) "
            "VALUES (?, ?, ?, ?, ?)",
            [
                (
                    run_id,
                    c.symbol,
                    c.direction,
                    json.dumps(c.contributions, ensure_ascii=False),
                    c.reason,
                )
                for c in combined.values()
            ],
        )

    def record_targets(self, run_id: str, targets: Iterable[TargetPosition]) -> None:
        self._connection.executemany(
            "INSERT INTO targets (run_id, symbol, quantity, reason) VALUES (?, ?, ?, ?)",
            [(run_id, t.symbol, str(t.quantity), t.reason) for t in targets],
        )

    def record_risk_events(self, run_id: str, rejected: dict[str, str]) -> None:
        self._connection.executemany(
            "INSERT INTO risk_events (run_id, symbol, reason, created_at) VALUES (?, ?, ?, ?)",
            [(run_id, symbol, reason, _now()) for symbol, reason in rejected.items()],
        )

    def record_snapshot(self, run_id: str, as_of: dt.date, positions: Iterable[Position]) -> None:
        self._connection.executemany(
            "INSERT INTO position_snapshots "
            "(run_id, as_of, symbol, quantity, cost_price, last_price) VALUES (?, ?, ?, ?, ?, ?)",
            [
                (
                    run_id,
                    as_of.isoformat(),
                    p.symbol,
                    str(p.quantity),
                    str(p.cost_price),
                    str(p.last_price),
                )
                for p in positions
            ],
        )

    # -- 注文 ---------------------------------------------------------------

    #: dry-run で組み立てただけの注文に付く状態。ブローカーには送っていない。
    DRY_RUN_STATUS = "DRY_RUN"

    def was_placed(self, client_order_id: str) -> bool:
        """すでに**ブローカーへ送った**注文か。

        ``client_order_id`` は取引日から決定論的に作られるので、これが
        True なら同じ判断からの再発注。冪等性の最後の砦になる。

        **dry-run の記録は数えない。** dry-run は1件もブローカーに送って
        いない。これを「発注済み」と数えると、確認のために dry-run した日は
        本発注が丸ごと抑止される（README が勧める手順そのものが、その日の
        取引を潰すことになる）。
        """
        cursor = self._connection.execute(
            "SELECT 1 FROM orders WHERE client_order_id = ? AND status != ?",
            (client_order_id, self.DRY_RUN_STATUS),
        )
        return cursor.fetchone() is not None

    def record_order(
        self, run_id: str, request: OrderRequest, status: str, broker_order_id: str | None = None
    ) -> None:
        self._connection.execute(
            "INSERT OR REPLACE INTO orders "
            "(client_order_id, run_id, broker_order_id, symbol, side, order_type, quantity, "
            " limit_price, status, reason, placed_at) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                request.client_order_id,
                run_id,
                broker_order_id,
                request.symbol,
                request.side.value,
                request.order_type.value,
                str(request.quantity),
                _text(request.limit_price),
                status,
                request.reason,
                _now(),
            ),
        )

    def unresolved_orders(self) -> list[str]:
        """結果がまだ確定していない注文の ``client_order_id``。

        ブローカーへ送った（dry-run でない）が、まだ終端状態
        （FILLED / CANCELLED / REJECTED / EXPIRED）に達していないもの。
        PENDING のまま残っている注文（送信後にタイムアウト等で応答を
        取りこぼしたもの）もここに含まれる——次回サイクルの冒頭で
        ブローカーに照会し、実際の状態を確定させるため。
        """
        terminal = (
            OrderStatus.FILLED.value,
            OrderStatus.CANCELLED.value,
            OrderStatus.REJECTED.value,
            OrderStatus.EXPIRED.value,
        )
        cursor = self._connection.execute(
            "SELECT client_order_id FROM orders WHERE status != ? AND status NOT IN (?, ?, ?, ?)",
            (self.DRY_RUN_STATUS, *terminal),
        )
        return [row["client_order_id"] for row in cursor.fetchall()]

    def update_order(self, order: Order) -> None:
        self._connection.execute(
            "UPDATE orders SET status = ?, filled_quantity = ?, avg_fill_price = ?, "
            "broker_order_id = COALESCE(?, broker_order_id), updated_at = ? "
            "WHERE client_order_id = ?",
            (
                order.status.value,
                str(order.filled_quantity),
                _text(order.avg_fill_price),
                order.broker_order_id,
                _now(),
                order.client_order_id,
            ),
        )

    def record_fills(self, fills: Iterable[Fill], run_id: str | None = None) -> None:
        self._connection.executemany(
            "INSERT INTO fills (client_order_id, run_id, symbol, side, quantity, price, fee, filled_at) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            [
                (
                    f.client_order_id,
                    run_id,
                    f.symbol,
                    f.side.value,
                    str(f.quantity),
                    str(f.price),
                    str(f.fee),
                    (f.filled_at or dt.datetime.now(dt.UTC)).isoformat(),
                )
                for f in fills
            ],
        )

    @staticmethod
    def today() -> dt.date:
        """``placed_at`` と同じ時計（UTC）で見た今日。

        ``orders_today`` にはこれを渡すこと。ローカル時刻の日付を渡すと、
        JST の深夜〜朝 9 時は UTC がまだ前日で、当日の発注が 0 件に見える。
        """
        return dt.datetime.now(dt.UTC).date()

    def orders_today(self, day: dt.date | None = None) -> int:
        """当日**ブローカーへ送った**注文の件数。1日あたりの上限判定に使う。

        ``day`` は :meth:`today` と同じ UTC 日付。省略時は今日。
        ``was_placed`` と同じく dry-run は数えない。dry-run は1件も送って
        いないのに枠を消費すると、設定を確認するために dry-run を数回
        回しただけで、その日の発注枠が尽きる。
        """
        day = day or self.today()
        cursor = self._connection.execute(
            "SELECT count(*) AS n FROM orders WHERE date(placed_at) = ? AND status != ?",
            (day.isoformat(), self.DRY_RUN_STATUS),
        )
        return int(cursor.fetchone()["n"])

    # -- ストップ -----------------------------------------------------------

    def save_stops(self, stops: dict[str, Stop]) -> None:
        """ストップを保存する（既存を置き換える）。

        プロセスが落ちてもストップが失われないよう、毎サイクル保存する。
        ここが消えると、建玉が無防備になる。
        """
        self._connection.execute("DELETE FROM stops")
        self._connection.executemany(
            "INSERT INTO stops "
            "(symbol, stop_price, entry_price, created_on, trailing, atr_multiple, highest_close, "
            " initial_stop_price, initial_quantity, scaled_out, updated_at) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            [
                (
                    s.symbol,
                    str(s.stop_price),
                    str(s.entry_price),
                    s.created_on.isoformat(),
                    int(s.trailing),
                    str(s.atr_multiple),
                    _text(s.highest_close),
                    _text(s.initial_stop_price),
                    _text(s.initial_quantity),
                    int(s.scaled_out),
                    _now(),
                )
                for s in stops.values()
            ],
        )

    def load_stops(self) -> dict[str, Stop]:
        cursor = self._connection.execute("SELECT * FROM stops")
        result = {}
        for row in cursor.fetchall():
            result[row["symbol"]] = Stop(
                symbol=row["symbol"],
                stop_price=Decimal(row["stop_price"]),
                entry_price=Decimal(row["entry_price"]),
                created_on=dt.date.fromisoformat(row["created_on"]),
                trailing=bool(row["trailing"]),
                atr_multiple=Decimal(row["atr_multiple"]),
                highest_close=_dec(row["highest_close"]),
                initial_stop_price=_dec(row["initial_stop_price"]),
                initial_quantity=_dec(row["initial_quantity"]),
                scaled_out=bool(row["scaled_out"]),
            )
        return result

    # -- 調査 ---------------------------------------------------------------

    def explain(self, run_id: str) -> dict[str, list[dict[str, Any]]]:
        """1回の実行を丸ごと取り出す。

        「なぜこの注文が出たのか」「なぜ出なかったのか」を、
        シグナルから拒否理由まで一覧で追える。
        """
        tables = ("signals", "combined_signals", "targets", "orders", "risk_events")
        return {
            table: [
                dict(row)
                for row in self._connection.execute(
                    f"SELECT * FROM {table} WHERE run_id = ?", (run_id,)
                ).fetchall()
            ]
            for table in tables
        }
