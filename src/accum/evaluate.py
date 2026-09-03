"""積立の判断が効いたか——**増額した日は、本当に安かったのか**。

積立は売らないので、デイトレやスイングのような「建てて外した損益」は無い。
代わりに問うのは倍率の当たり外れになる:

    下落局面で倍率を上げて多く買った日の取得単価は、
    その後の価格より安かったか（＝増額は報われたか）。

判断は ``state/accum/history/decision/`` に積んである。ここではそれに
``horizon`` 営業日後の終値を当て、**倍率の帯ごとに**その後のリターンを並べる。
倍率 1.0 の日（通常の積立）が対照群になる。

``accum backtest`` との違いは、こちらが**実際に判断した記録**を材料にすること。
バックテストは規則を過去に当て直すが、こちらは運用が実際に見た値を使う。
"""

from __future__ import annotations

import datetime as dt
from typing import Any

import polars as pl

from wbcore.data.store import BarStore

#: 評価を積む kind。
KIND = "evaluation"

#: 倍率の帯。積立は 1.0 が通常で、下落局面ほど大きくなる。
BUCKETS = ("1.0（通常）", "1.0〜1.5", "1.5〜2.0", "2.0 以上")

EVALUATION_SCHEMA: dict[str, Any] = {
    "symbol": pl.Utf8,
    "judged_on": pl.Date,
    "multiplier": pl.Float64,
    "bucket": pl.Utf8,
    "due": pl.Float64,
    "entry_close": pl.Float64,
    "horizon": pl.Int32,
    "exit_date": pl.Date,
    "exit_close": pl.Float64,
    "ret_bp": pl.Float64,
}


def bucket_of(multiplier: float) -> str:
    """倍率の帯。境界は「通常か、増額か、どれだけ増額か」で切る。"""
    if multiplier <= 1.0:
        return BUCKETS[0]
    if multiplier < 1.5:
        return BUCKETS[1]
    if multiplier < 2.0:
        return BUCKETS[2]
    return BUCKETS[3]


def evaluate(
    decisions: pl.DataFrame,
    store: BarStore,
    *,
    horizon: int,
) -> pl.DataFrame:
    """判断の並びに ``horizon`` 営業日後の終値を当てる。

    足が届いていない判断（新しすぎる）は**行を落とさず**実績だけ null にする。
    落とすと「評価できた判断だけ」の偏った集計になる。
    """
    from wbjp.evaluate import forward_close

    rows: dict[str, list[Any]] = {key: [] for key in EVALUATION_SCHEMA}
    cache: dict[str, pl.DataFrame | None] = {}
    for row in decisions.iter_rows(named=True):
        symbol = row["symbol"]
        if symbol not in cache:
            cache[symbol] = store.read(symbol) if store.has(symbol) else None
        bars = cache[symbol]
        judged_on = row.get("judged_on")
        entry = row.get("close")
        exit_date: dt.date | None = None
        exit_close: float | None = None
        ret_bp: float | None = None
        if bars is not None and bars.height and judged_on is not None:
            found = forward_close(bars, judged_on, horizon)
            if found is not None and entry:
                exit_date, exit_close = found
                ret_bp = (exit_close / float(entry) - 1) * 10_000

        multiplier = float(row.get("multiplier") or 1.0)
        rows["symbol"].append(symbol)
        rows["judged_on"].append(judged_on)
        rows["multiplier"].append(multiplier)
        rows["bucket"].append(bucket_of(multiplier))
        rows["due"].append(row.get("due"))
        rows["entry_close"].append(float(entry) if entry else None)
        rows["horizon"].append(horizon)
        rows["exit_date"].append(exit_date)
        rows["exit_close"].append(exit_close)
        rows["ret_bp"].append(ret_bp)
    return pl.DataFrame(rows, schema=EVALUATION_SCHEMA).sort("judged_on", nulls_last=True)


def _scored(evaluation: pl.DataFrame) -> pl.DataFrame:
    """実績の出た行だけ。**履歴が 1 件も無いと列自体が無い**空フレームが来るので、
    列の有無から確かめる（``pl.col`` は無い列に当てると例外になる）。"""
    if "ret_bp" not in evaluation.columns:
        return evaluation.clear()
    return evaluation.filter(pl.col("ret_bp").is_not_null())


def summarize(evaluation: pl.DataFrame) -> pl.DataFrame:
    """倍率の帯ごとの件数・平均リターン・投下額。

    増額の帯（1.0 超）が通常の帯より高いリターンなら、倍率の付け方は効いている。
    """
    scored = _scored(evaluation)
    if scored.is_empty():
        return pl.DataFrame(
            schema={
                "bucket": pl.Utf8,
                "count": pl.UInt32,
                "avg_ret_bp": pl.Float64,
                "win_rate": pl.Float64,
                "due": pl.Float64,
            }
        )
    order = {name: i for i, name in enumerate(BUCKETS)}
    return (
        scored.group_by("bucket")
        .agg(
            pl.len().alias("count"),
            pl.col("ret_bp").mean().alias("avg_ret_bp"),
            (pl.col("ret_bp") > 0).mean().alias("win_rate"),
            pl.col("due").sum().alias("due"),
        )
        .with_columns(pl.col("bucket").replace_strict(order, default=99).alias("_o"))
        .sort("_o")
        .drop("_o")
    )


__all__ = ["BUCKETS", "EVALUATION_SCHEMA", "KIND", "bucket_of", "evaluate", "summarize"]
