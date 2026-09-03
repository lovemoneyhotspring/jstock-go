"""スクリーニングの妥当性——**選んだ銘柄は、選ばなかった銘柄より上がったか**。

``daytrade evaluate`` と同じ考え方をスイング売買に当てはめる。あちらは寄付から
大引までの 1 日で答えが出るが、こちらは数日〜数週間かけて建てて外すので、
「判断から ``horizon`` 営業日後までのリターン」を実績とする。

比べる相手が要るのが要点。採用した銘柄（``adopted``）の平均リターンだけを見ても、
相場全体が上がった日なら当然プラスになる。**同じ日に候補には挙がったが採用しなかった
銘柄**（``passed`` / ``rest``）と並べて初めて、順位付けが効いているかが分かる。

===========  ==========================================================
``group``    中身
===========  ==========================================================
``adopted``  上位 ``max_positions`` 件。次のサイクルで建てる候補
``passed``   閾値は超えたが採用枠から溢れた
``rest``     閾値未満
===========  ==========================================================

判断そのものは ``wbjp screen`` が ``state/wbjp/history/screen/`` に積んでいる。
ここはそれに後日の足を当てるだけで、**判断のロジックには一切触れない**。
"""

from __future__ import annotations

import datetime as dt
from typing import Any

import polars as pl

from wbcore.data.store import BarStore

#: 評価の結果を積む kind。
KIND = "evaluation"

#: 群の並び順（表示と集計で共通）。
GROUPS = ("adopted", "passed", "rest")

EVALUATION_SCHEMA: dict[str, Any] = {
    # 判断のときの値（screen の履歴から写す）
    "symbol": pl.Utf8,
    "rank": pl.Int32,
    "score": pl.Float64,
    "group": pl.Utf8,
    "entry_close": pl.Float64,
    # 実績
    "horizon": pl.Int32,
    "exit_date": pl.Date,
    "exit_close": pl.Float64,
    "ret_bp": pl.Float64,
    #: 判断の材料になった screen の実行（ログと突き合わせる鍵）
    "screen_run_id": pl.Utf8,
}


def _group(row: dict[str, Any]) -> str:
    if row.get("adopted"):
        return "adopted"
    return "passed" if row.get("passed") else "rest"


def forward_close(
    frame: pl.DataFrame, entry_day: dt.date, horizon: int
) -> tuple[dt.date, float] | None:
    """``entry_day`` から ``horizon`` 本先の足（終値）。足りなければ None。

    暦日ではなく**足の本数**で数える。休場を挟んでも「何営業日後」の意味が
    変わらないようにするため。
    """
    after = frame.filter(pl.col("date") > entry_day).sort("date")
    if after.height < horizon:
        return None
    row = after.row(horizon - 1, named=True)
    return row["date"], float(row["close"])


def evaluate(
    screens: pl.DataFrame,
    store: BarStore,
    *,
    day: dt.date,
    horizon: int,
) -> pl.DataFrame:
    """1 日ぶんのスクリーニング結果に、``horizon`` 営業日後の終値を当てる。

    足が届いていない銘柄（新しすぎる判断・上場廃止）は**行を落とさず**実績だけ
    null にする。落とすと「評価できた銘柄だけ」の偏った集計になるため。
    """
    rows: dict[str, list[Any]] = {key: [] for key in EVALUATION_SCHEMA}
    cache: dict[str, pl.DataFrame | None] = {}
    for row in screens.iter_rows(named=True):
        symbol = row["symbol"]
        if symbol not in cache:
            cache[symbol] = store.read(symbol) if store.has(symbol) else None
        bars = cache[symbol]
        entry = row.get("close")
        exit_date: dt.date | None = None
        exit_close: float | None = None
        ret_bp: float | None = None
        if bars is not None and bars.height:
            found = forward_close(bars, day, horizon)
            if found is not None and entry:
                exit_date, exit_close = found
                ret_bp = (exit_close / float(entry) - 1) * 10_000

        rows["symbol"].append(symbol)
        rows["rank"].append(row.get("rank"))
        rows["score"].append(row.get("score"))
        rows["group"].append(_group(row))
        rows["entry_close"].append(float(entry) if entry else None)
        rows["horizon"].append(horizon)
        rows["exit_date"].append(exit_date)
        rows["exit_close"].append(exit_close)
        rows["ret_bp"].append(ret_bp)
        rows["screen_run_id"].append(row.get("run_id"))
    return pl.DataFrame(rows, schema=EVALUATION_SCHEMA).sort("rank", nulls_last=True)


def _scored(evaluation: pl.DataFrame) -> pl.DataFrame:
    """実績の出た行だけ。**履歴が 1 件も無いと列自体が無い**空フレームが来るので、
    列の有無から確かめる（``pl.col`` は無い列に当てると例外になる）。"""
    if "ret_bp" not in evaluation.columns:
        return evaluation.clear()
    return evaluation.filter(pl.col("ret_bp").is_not_null())


def summarize(evaluation: pl.DataFrame) -> pl.DataFrame:
    """群ごとの件数・平均リターン・勝率。実績が出ていない行は除いて数える。"""
    scored = _scored(evaluation)
    if scored.is_empty():
        return pl.DataFrame(
            schema={
                "group": pl.Utf8,
                "count": pl.UInt32,
                "avg_ret_bp": pl.Float64,
                "win_rate": pl.Float64,
            }
        )
    order = {name: i for i, name in enumerate(GROUPS)}
    return (
        scored.group_by("group")
        .agg(
            pl.len().alias("count"),
            pl.col("ret_bp").mean().alias("avg_ret_bp"),
            (pl.col("ret_bp") > 0).mean().alias("win_rate"),
        )
        .with_columns(pl.col("group").replace_strict(order, default=99).alias("_o"))
        .sort("_o")
        .drop("_o")
    )


def review(evaluations: pl.DataFrame) -> pl.DataFrame:
    """日ごとに ``adopted`` / ``passed`` / ``rest`` の平均リターンを横に並べる。

    ``adopted_bp`` が ``rest_bp`` を上回る日が多いほど、順位付けが効いている。
    """
    scored = _scored(evaluations)
    if scored.is_empty():
        return pl.DataFrame(
            schema={
                "day": pl.Date,
                "horizon": pl.Int32,
                "adopted": pl.UInt32,
                "adopted_bp": pl.Float64,
                "passed_bp": pl.Float64,
                "rest_bp": pl.Float64,
            }
        )
    return (
        scored.group_by(["day", "horizon"])
        .agg(
            (pl.col("group") == "adopted").sum().cast(pl.UInt32).alias("adopted"),
            pl.col("ret_bp").filter(pl.col("group") == "adopted").mean().alias("adopted_bp"),
            pl.col("ret_bp").filter(pl.col("group") == "passed").mean().alias("passed_bp"),
            pl.col("ret_bp").filter(pl.col("group") == "rest").mean().alias("rest_bp"),
        )
        .sort("day")
    )


def review_totals(table: pl.DataFrame) -> pl.DataFrame:
    """期間ぶんの合計。``選定が勝った日の割合`` が、規則が効いているかの目安。"""
    if table.is_empty():
        return pl.DataFrame(
            schema={
                "days": pl.UInt32,
                "avg_adopted_bp": pl.Float64,
                "avg_rest_bp": pl.Float64,
                "beat_rest_rate": pl.Float64,
            }
        )
    return table.select(
        pl.len().cast(pl.UInt32).alias("days"),
        pl.col("adopted_bp").mean().alias("avg_adopted_bp"),
        pl.col("rest_bp").mean().alias("avg_rest_bp"),
        (pl.col("adopted_bp") > pl.col("rest_bp")).mean().alias("beat_rest_rate"),
    )


__all__ = [
    "EVALUATION_SCHEMA",
    "GROUPS",
    "KIND",
    "evaluate",
    "forward_close",
    "review",
    "review_totals",
    "summarize",
]
