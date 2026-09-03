"""実行品質——「そう判断した値」と「実際に約定した値」の差を残す。

**なぜ台帳とは別に要るか**

台帳（``state/*.db``）は「いま何を持っていて、いくら発注済みか」を答えるための
もので、運用に必要な最新の状態だけを持つ。実際 :meth:`accum.ledger.Ledger.update_status`
は約定額で ``amount`` を**上書き**するので、事後には「判断時にいくらのつもりだったか」を
復元できない。

改善に使いたいのはまさにその差分——想定より不利に約定していないか、どの理由で
どれだけ見送っているか——なので、上書きされない追記専用の表を別に持つ。

**形**

1 回の発注は**必ず 2 つの時点に分かれる**。発注した瞬間に約定価格は分からず
（:class:`wbcore.domain.models.OrderAck` は価格を持たない）、約定は次回以降の
照会で判明する。そこで 1 行を後から書き換えるのではなく、行を足す:

===========  ========================================================
``event``    いつ足すか
===========  ========================================================
``intent``   発注した（または dry-run で出さなかった）時点。想定価格・想定手数料
``fill``     約定を照会して確定した時点。約定価格・約定数量
``skip``     発注しなかった。``reason`` に理由コード
===========  ========================================================

突き合わせの鍵は ``client_order_id``（:func:`wbcore.domain.models.make_client_order_id`
が決定論的に作る）。``intent`` と ``fill`` を join すればスリッページが出る。

**金額の型**

ログ（``docs/LOGGING.md``）では金額を文字列にしているが、ここは集計のための表
なので ``Float64`` にする。正確な残高の記録は台帳が持っており、こちらは
「平均して何 bp 負けているか」を出すためのもの。
"""

from __future__ import annotations

import contextlib
import datetime as dt
from contextvars import ContextVar
from decimal import Decimal
from enum import StrEnum
from typing import Any

import polars as pl

from wbcore.history import HistoryStore

#: 実行品質の kind（:class:`wbcore.history.HistoryStore` のディレクトリ名）。
KIND = "execution"


class ReasonCode(StrEnum):
    """発注しなかった／できなかった理由。

    これまで理由は自由文（``decision.reason``、``PlannedOrder.note``、
    ``str(exc)``）だけだった。文言は変わるので、集計はこのコードで行う。
    人が読むための説明は ``note`` 列に自由文のまま残す。
    """

    #: 出した
    PLACED = "placed"
    #: ``--live`` が無い（判断と記録だけ）
    DRY_RUN = "dry_run"
    #: キルスイッチ
    KILL_SWITCH = "kill_switch"
    #: 発注してよい時間帯の外
    WINDOW_CLOSED = "window_closed"
    #: 休日
    HOLIDAY = "holiday"
    #: 予算が 1 単元に届かない
    LOT_TOO_SMALL = "lot_too_small"
    #: 買付余力が足りない
    INSUFFICIENT_FUNDS = "insufficient_funds"
    #: 同じ注文を送信済み（冪等）
    IDEMPOTENT = "idempotent"
    #: リスク判定で弾かれた
    RISK_REJECTED = "risk_rejected"
    #: 銘柄が対象外（allowlist・貸借銘柄でない 等）
    NOT_ELIGIBLE = "not_eligible"
    #: 気配が取れない・古い
    NO_QUOTE = "no_quote"
    #: ブローカーがエラーを返した
    BROKER_ERROR = "broker_error"
    #: 送ったが応答が無く、届いたか分からない
    UNCONFIRMED = "unconfirmed"
    #: 約定した（``fill`` の行）
    FILLED = "filled"
    #: 未約定のまま失効した
    EXPIRED = "expired"


#: 追記する表の形。列は増やしてよい（読み出しは ``diagonal_relaxed`` で前方互換）。
EXECUTION_SCHEMA: dict[str, Any] = {
    "event": pl.String,
    "app": pl.String,
    "symbol": pl.String,
    "side": pl.String,
    # 現物 / 信用新規 / 信用返済。アプリによっては無い
    "trade": pl.String,
    "client_order_id": pl.String,
    "broker_order_id": pl.String,
    "live": pl.Boolean,
    # 判断した時点の想定
    "quantity": pl.Int64,
    "intent_price": pl.Float64,
    "intent_amount": pl.Float64,
    "intent_fee": pl.Float64,
    # 実際
    "fill_quantity": pl.Int64,
    "fill_price": pl.Float64,
    "fill_fee": pl.Float64,
    #: 有利ならプラス、不利ならマイナス（:func:`slippage_bp`）
    "slippage_bp": pl.Float64,
    "reason": pl.String,
    "note": pl.String,
}


def _f(value: Any) -> float | None:
    """Decimal / str / int を float に。空は None。"""
    if value is None or value == "":
        return None
    if isinstance(value, Decimal | int | float):
        return float(value)
    try:
        return float(str(value))
    except ValueError:
        return None


def _i(value: Any) -> int | None:
    number = _f(value)
    return None if number is None else int(number)


def slippage_bp(side: str, intent_price: Any, fill_price: Any) -> float | None:
    """想定と約定の差を bp で。**有利ならプラス、不利ならマイナス。**

    買いは安く買えたら有利、売りは高く売れたら有利——向きが逆なので、
    符号を揃えておかないと平均が意味を持たなくなる。
    """
    intent = _f(intent_price)
    fill = _f(fill_price)
    if not intent or fill is None:
        return None
    diff = (intent - fill) if side.upper() == "BUY" else (fill - intent)
    return diff / intent * 10_000


def row(
    event: str,
    *,
    app: str,
    symbol: str,
    side: str,
    reason: ReasonCode | str,
    trade: str | None = None,
    client_order_id: str | None = None,
    broker_order_id: str | None = None,
    live: bool = False,
    quantity: Any = None,
    intent_price: Any = None,
    intent_amount: Any = None,
    intent_fee: Any = None,
    fill_quantity: Any = None,
    fill_price: Any = None,
    fill_fee: Any = None,
    note: str | None = None,
) -> dict[str, Any]:
    """1 行を作る。列は :data:`EXECUTION_SCHEMA` に揃える。"""
    return {
        "event": event,
        "app": app,
        "symbol": str(symbol),
        # StrEnum（Side / TradeType）で渡ることがあるので、必ず素の str にする
        "side": str(side).upper(),
        "trade": None if trade is None else str(trade),
        "client_order_id": client_order_id,
        "broker_order_id": broker_order_id,
        "live": live,
        "quantity": _i(quantity),
        "intent_price": _f(intent_price),
        "intent_amount": _f(intent_amount),
        "intent_fee": _f(intent_fee),
        "fill_quantity": _i(fill_quantity),
        "fill_price": _f(fill_price),
        "fill_fee": _f(fill_fee),
        "slippage_bp": slippage_bp(side, intent_price, fill_price),
        "reason": str(reason),
        "note": note,
    }


def frame(rows: list[dict[str, Any]]) -> pl.DataFrame:
    """行の並びを :data:`EXECUTION_SCHEMA` の DataFrame に。0 行でも形は保つ。"""
    if not rows:
        return pl.DataFrame(schema=EXECUTION_SCHEMA)
    ordered = [{key: item.get(key) for key in EXECUTION_SCHEMA} for item in rows]
    return pl.DataFrame(ordered, schema=EXECUTION_SCHEMA)


def record(store: HistoryStore, rows: list[dict[str, Any]], *, day: dt.date) -> None:
    """まとめて 1 ファイルとして足す。行が無ければ何もしない。

    0 行を書かないのは、``plan`` などと違って「発注の機会そのものが無かった」
    実行が大半だから。毎回書くと空ファイルばかりが増える。
    """
    if rows:
        store.append(KIND, frame(rows), day=day)


_buffer: ContextVar[list[dict[str, Any]] | None] = ContextVar("wbcore_execution_rows", default=None)


def collect(**fields: Any) -> None:
    """1 行を貯める。書き出しは実行の終わりに :func:`flush` でまとめて行う。

    発注のたびに Parquet を書くと、1 回の実行で十数個のファイルができてしまう。
    貯めておいて 1 ファイルにする。
    """
    rows = _buffer.get()
    if rows is None:
        rows = []
        _buffer.set(rows)
    rows.append(row(**fields))


def flush(store: HistoryStore, *, day: dt.date) -> None:
    """貯めた行を書き出して空にする。記録が失敗しても実行は落とさない。"""
    rows = _buffer.get()
    _buffer.set(None)
    if not rows:
        return
    with contextlib.suppress(OSError):
        record(store, rows, day=day)


def pending() -> list[dict[str, Any]]:
    """まだ書き出していない行（テスト用）。"""
    return list(_buffer.get() or [])


def reset() -> None:
    """貯めた行を捨てる（テスト用）。"""
    _buffer.set(None)


def summarize(executions: pl.DataFrame) -> pl.DataFrame:
    """実行品質の要約。理由コードごとの件数と、約定したものの平均スリッページ。"""
    if executions.is_empty():
        return pl.DataFrame(
            schema={
                "app": pl.String,
                "reason": pl.String,
                "count": pl.UInt32,
                "avg_slippage_bp": pl.Float64,
                "amount": pl.Float64,
            }
        )
    return (
        executions.group_by(["app", "reason"])
        .agg(
            pl.len().alias("count"),
            pl.col("slippage_bp").mean().alias("avg_slippage_bp"),
            pl.col("intent_amount").sum().alias("amount"),
        )
        .sort(["app", "count"], descending=[False, True])
    )
