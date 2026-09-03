"""スイング売買のスクリーニング履歴（``wbjp screen`` の結果を積む）。

置き場は ``state/wbjp/history/screen/``（:class:`wbcore.history.HistoryStore`）。
1 回の ``screen`` が 1 ファイル。合成後の意見がある銘柄は**閾値未満も含めて全部**残し、
``passed``（閾値以上）と ``adopted``（上位 ``max_positions`` 件＝採用候補）の印を付ける。
"""

from __future__ import annotations

from collections.abc import Callable
from decimal import Decimal
from typing import TYPE_CHECKING, Any

import polars as pl

from wbcore.history import HistoryStore

if TYPE_CHECKING:
    from wbcore.domain.models import CombinedSignal
    from wbcore.settings import AppSettings

#: 戦略が ``meta`` に残す数値のうち履歴に持つもの（無ければ null）
META_KEYS = (
    "dryup_ratio",
    "drawdown",
    "atr_ratio",
    "dollar_volume",
    "dryup",
    "rs",
    "trend",
    "liquid",
)

SCREEN_SCHEMA: dict[str, Any] = {
    "rank": pl.Int32,
    "symbol": pl.Utf8,
    "score": pl.Float64,
    "passed": pl.Boolean,
    "adopted": pl.Boolean,
    "close": pl.Float64,
    "reason": pl.Utf8,
    **{key: pl.Float64 for key in META_KEYS},
    "entry_threshold": pl.Float64,
    "max_positions": pl.Int32,
    "combiner": pl.Utf8,
}


def store_for(settings: AppSettings) -> HistoryStore:
    return HistoryStore(settings.wbjp_history_dir)


def _num(value: object) -> float | None:
    """数値・Decimal を float に。None や型の違うものは null。"""
    if isinstance(value, int | float | Decimal):
        return float(value)
    return None


def screen_frame(
    combined: dict[str, CombinedSignal],
    meta: dict[str, dict[str, object]],
    reasons: dict[str, str],
    *,
    threshold: float,
    max_positions: int,
    combiner: str,
    close: Callable[[str], Decimal],
) -> pl.DataFrame:
    """合成後の全銘柄をスコア順に並べ、閾値と採用枠の印を付ける。"""
    ordered = sorted(combined.values(), key=lambda c: (-c.direction, c.symbol))
    rows: dict[str, list[Any]] = {key: [] for key in SCREEN_SCHEMA}
    passed_so_far = 0
    for rank, item in enumerate(ordered, start=1):
        m = meta.get(item.symbol, {})
        passed = item.direction >= threshold
        if passed:
            passed_so_far += 1
        rows["rank"].append(rank)
        rows["symbol"].append(item.symbol)
        rows["score"].append(float(item.direction))
        rows["passed"].append(passed)
        rows["adopted"].append(passed and passed_so_far <= max_positions)
        rows["close"].append(_num(m.get("close")) or _num(close(item.symbol)))
        rows["reason"].append(reasons.get(item.symbol, item.reason))
        for key in META_KEYS:
            rows[key].append(_num(m.get(key)))
        rows["entry_threshold"].append(float(threshold))
        rows["max_positions"].append(max_positions)
        rows["combiner"].append(combiner)
    return pl.DataFrame(rows, schema=SCREEN_SCHEMA)


__all__ = ["META_KEYS", "SCREEN_SCHEMA", "screen_frame", "store_for"]
