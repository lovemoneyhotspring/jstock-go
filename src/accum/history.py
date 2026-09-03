"""積立の判断履歴（``accum run`` が「今日いくら出すか」を決めた記録）。

置き場は ``state/accum/history/``（:class:`wbcore.history.HistoryStore`）。

**なぜ台帳では足りないか**

台帳（``state/accum-<env>.db``）には**発注したもの**しか残らない。積立の判断は
「倍率 × 予算」で決まり、下落局面ほど多く買う設計なので、改善に使いたいのは
むしろ「どの倍率で、いくらの株価のときに、いくら出したか」の並び。発注に
至らなかった日（時間帯の外・単元未満）も含めて残さないと、後から
「倍率の付け方は効いていたのか」を確かめられない。

デイトレ（``state/daytrade/history/``）とスイング（``state/wbjp/history/screen/``）で
既にやっていることを、積立にも揃える。
"""

from __future__ import annotations

from decimal import Decimal
from typing import TYPE_CHECKING, Any

import polars as pl

from wbcore.history import HistoryStore

if TYPE_CHECKING:
    from accum.execute import Contribution
    from wbcore.settings import AppSettings

#: 判断を積む kind。
KIND = "decision"

DECISION_SCHEMA: dict[str, Any] = {
    "symbol": pl.Utf8,
    "market": pl.Utf8,
    #: 判断に使った足の日付（``day`` は実行日なので別に持つ）
    "judged_on": pl.Date,
    #: どの月の積立か
    "month": pl.Date,
    #: 判断に使った終値
    "close": pl.Float64,
    #: 今日出す額（差額）
    "due": pl.Float64,
    #: 今月の目標と、すでに発注済みの額
    "target": pl.Float64,
    "placed": pl.Float64,
    #: 下落局面で増やすための倍率。**評価はこれが効いたかを見る**
    "multiplier": pl.Float64,
    "tactic": pl.Utf8,
    "reason": pl.Utf8,
}


def store_for(settings: AppSettings) -> HistoryStore:
    return HistoryStore(settings.accum_history_dir)


def _num(value: object) -> float | None:
    if isinstance(value, int | float | Decimal):
        return float(value)
    return None


def decision_frame(contributions: list[Contribution]) -> pl.DataFrame:
    """その実行で決まった投下を 1 行ずつに。0 件でも形は保つ。"""
    rows: dict[str, list[Any]] = {key: [] for key in DECISION_SCHEMA}
    for c in contributions:
        rows["symbol"].append(c.symbol)
        rows["market"].append(c.market.value)
        rows["judged_on"].append(c.date)
        rows["month"].append(c.month)
        rows["close"].append(_num(c.close))
        rows["due"].append(_num(c.amount))
        rows["target"].append(_num(c.target))
        rows["placed"].append(_num(c.placed))
        rows["multiplier"].append(float(c.multiplier))
        rows["tactic"].append(c.tactic.describe())
        rows["reason"].append(c.reason)
    return pl.DataFrame(rows, schema=DECISION_SCHEMA)


__all__ = ["DECISION_SCHEMA", "KIND", "decision_frame", "store_for"]
