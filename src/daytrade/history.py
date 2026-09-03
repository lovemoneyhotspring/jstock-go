"""デイトレの選定履歴。前夜の候補、9:00 の気配・順位表・選定、実行の要約を積む。

置き場は ``state/daytrade/history/<kind>/``（:class:`wbcore.history.HistoryStore`）。
1 回の ``plan`` / ``open`` が 1 ファイルで、上書きしない。cron が 9:01・9:04・9:07 と
3 回 ``open`` を叩けば 3 ファイル残り、``run_id`` で区別できる。

種類（``kind``）と 1 行の意味:

| kind        | 1 行                                    | いつ                          |
|-------------|-----------------------------------------|-------------------------------|
| ``plan``      | 母集団の 1 銘柄（除外理由の列ごと）           | ``plan`` のたび                 |
| ``plan_meta`` | ``plan`` 1 回の要約（件数・信号）            | ``plan`` のたび                 |
| ``quotes``    | 9:00 に受け取った気配 1 銘柄（使えたかの印付き） | ``open`` が気配を取ったとき        |
| ``ranking``   | 順位表 1 行（ロング・ショート両方、選定の印付き）  | ``open`` が順位を付けたとき        |
| ``open_run``  | ``open`` 1 回の要約（危険信号・件数・結末）     | ``open`` が判断まで進んだとき       |

順位表は JSONL ログと違い**全行**を持つ。「なぜ X が選ばれなかったか」は ``ranking`` に
無ければ ``quotes``（ギャップが条件外・ストップ安・気配なし）で追える。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import asdict
from decimal import Decimal
from typing import TYPE_CHECKING, Any

import polars as pl

from wbcore.clock import ensure_utc
from wbcore.history import HistoryStore

if TYPE_CHECKING:
    from daytrade.plan import Plan
    from daytrade.select import Pick, Quote, Ranked
    from wbcore.settings import AppSettings

UTC_TS = pl.Datetime("us", "UTC")

PLAN_META_SCHEMA: dict[str, Any] = {
    "prev_day": pl.Date,
    "positions": pl.Int32,
    "budget_per_order": pl.Float64,
    "iv_prev": pl.Float64,
    "iv_gate": pl.Float64,
    "drift": pl.Float64,
    "candidates": pl.Int32,
    "eligible": pl.Int32,
    "short_eligible": pl.Int32,
    "created_at": pl.Utf8,
}

QUOTES_SCHEMA: dict[str, Any] = {
    "symbol": pl.Utf8,
    "price": pl.Float64,
    "quote_at": UTC_TS,
    "source": pl.Utf8,
    "delayed": pl.Boolean,
    #: 鮮度の検査を通り、順位付けに使えた気配か
    "usable": pl.Boolean,
    "prev_close": pl.Float64,
    "gap": pl.Float64,
}

RANKING_SCHEMA: dict[str, Any] = {
    #: BUY（ロング: ギャップ下位）/ SELL（ショート: ギャップ上位）
    "side": pl.Utf8,
    "rank": pl.Int32,
    "symbol": pl.Utf8,
    "code": pl.Utf8,
    "name": pl.Utf8,
    "prev_close": pl.Float64,
    "price": pl.Float64,
    "gap": pl.Float64,
    "vol20": pl.Float64,
    "picked": pl.Boolean,
    "quantity": pl.Float64,
    "amount": pl.Float64,
    "n": pl.Int32,
    "budget": pl.Float64,
}

OPEN_RUN_SCHEMA: dict[str, Any] = {
    #: live / dry_run / watch（資金 0）
    "mode": pl.Utf8,
    #: picked / regime / no_quotes / no_picks / no_capital
    "outcome": pl.Utf8,
    "quotes_requested": pl.Int32,
    "quotes_received": pl.Int32,
    "quotes_usable": pl.Int32,
    "trade": pl.Boolean,
    "reasons": pl.Utf8,
    "scale": pl.Float64,
    "weak": pl.Boolean,
    "iv_prev": pl.Float64,
    "drift_bp": pl.Float64,
    "market_gap_bp": pl.Float64,
    "recent_pnl": pl.Float64,
    "us_ret_bp": pl.Float64,
    "vix": pl.Float64,
    "n": pl.Int32,
    "budget": pl.Float64,
    "weighting": pl.Utf8,
    "short_n": pl.Int32,
    "short_budget": pl.Float64,
    "short_multiplier": pl.Float64,
    "long_picks": pl.Int32,
    "short_picks": pl.Int32,
    #: 台帳に書いた注文の数（dry-run を含む）と、通らなかった数
    "orders": pl.Int32,
    "failures": pl.Int32,
}


def store_for(settings: AppSettings) -> HistoryStore:
    return HistoryStore(settings.daytrade_history_dir)


def _num(value: object) -> float | None:
    """数値・Decimal・数字の文字列を float に。None や型の違うものは null。"""
    if isinstance(value, bool):
        return float(value)
    if isinstance(value, int | float | str | Decimal):
        return float(value)
    return None


def _one_row(row: dict[str, Any], schema: dict[str, Any]) -> pl.DataFrame:
    """スキーマの列を全部持つ 1 行（無い項目は null）。列の型を日によらず揃える。"""
    return pl.DataFrame({k: [row.get(k)] for k in schema}, schema=schema)


def plan_frames(plan: Plan) -> tuple[pl.DataFrame, pl.DataFrame]:
    """``plan`` の母集団（全銘柄）と要約（1 行）。"""
    raw = asdict(plan.meta)
    meta = {
        **raw,
        "prev_day": dt.date.fromisoformat(raw["prev_day"]),
        "budget_per_order": _num(raw["budget_per_order"]),
        "iv_gate": _num(raw["iv_gate"]),
    }
    meta.pop("day", None)
    return plan.frame, _one_row(meta, PLAN_META_SCHEMA)


def quotes_frame(
    received: dict[str, Quote], usable: set[str], prev_close: dict[str, float | None]
) -> pl.DataFrame:
    """受け取った気配の全件。``usable`` は鮮度の検査を通ったもの。"""
    rows = sorted(received.values(), key=lambda q: q.symbol)
    prev = [prev_close.get(q.symbol) for q in rows]
    return pl.DataFrame(
        {
            "symbol": [q.symbol for q in rows],
            "price": [float(q.price) for q in rows],
            "quote_at": [ensure_utc(q.at) for q in rows],
            "source": [q.source for q in rows],
            "delayed": [q.delayed for q in rows],
            "usable": [q.symbol in usable for q in rows],
            "prev_close": [_num(p) for p in prev],
            "gap": [
                float(q.price) / float(p) - 1 if p else None
                for q, p in zip(rows, prev, strict=True)
            ],
        },
        schema=QUOTES_SCHEMA,
    )


def ranking_frame(
    ranking: list[Ranked], picks: list[Pick], *, side: str, n: int, budget: Decimal
) -> pl.DataFrame:
    """順位表の全行に、選ばれた銘柄の株数・金額を付ける。"""
    picked = {p.symbol: p for p in picks}
    return pl.DataFrame(
        {
            "side": [side] * len(ranking),
            "rank": [r.rank for r in ranking],
            "symbol": [r.symbol for r in ranking],
            "code": [r.code for r in ranking],
            "name": [r.name for r in ranking],
            "prev_close": [float(r.prev_close) for r in ranking],
            "price": [float(r.price) for r in ranking],
            "gap": [float(r.gap) for r in ranking],
            "vol20": [r.vol for r in ranking],
            "picked": [r.symbol in picked for r in ranking],
            "quantity": [
                float(picked[r.symbol].quantity) if r.symbol in picked else None for r in ranking
            ],
            "amount": [
                float(picked[r.symbol].amount) if r.symbol in picked else None for r in ranking
            ],
            "n": [n] * len(ranking),
            "budget": [float(budget)] * len(ranking),
        },
        schema=RANKING_SCHEMA,
    )


def open_run_frame(**fields: Any) -> pl.DataFrame:
    """``open`` 1 回の要約。:data:`OPEN_RUN_SCHEMA` に無い項目は捨てる。"""
    row = {k: v for k, v in fields.items() if k in OPEN_RUN_SCHEMA}
    for key, dtype in OPEN_RUN_SCHEMA.items():
        if dtype is pl.Float64 and row.get(key) is not None:
            row[key] = float(row[key])
    if isinstance(row.get("reasons"), list):
        row["reasons"] = "、".join(row["reasons"])
    return _one_row(row, OPEN_RUN_SCHEMA)


__all__ = [
    "OPEN_RUN_SCHEMA",
    "PLAN_META_SCHEMA",
    "QUOTES_SCHEMA",
    "RANKING_SCHEMA",
    "open_run_frame",
    "plan_frames",
    "quotes_frame",
    "ranking_frame",
    "store_for",
]
