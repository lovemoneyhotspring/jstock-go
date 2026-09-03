"""候補の結果の評価（``daytrade evaluate``）と、選定の妥当性の集計（``daytrade review``）。

朝の順位表（``history/ranking``）にある全銘柄——選んだものも、次点で見送ったものも——に
その日の日足（寄付 → 大引）を当て、「建てていたらいくらだったか」を残す。実際に建てた
ものは台帳の約定と並べる。

9:00 の順位表が無い日（発注経路を止めていて ``open`` が走らない、気配が取れなかった）は、
前夜の ``plan`` と当日の**始値**から同じ規則（:mod:`daytrade.select`）で順位を作り直す。
これはバックテストと同じ近似で、``ranking_source`` 列で区別する:

- ``quotes``       … 9:00 の気配で作った順位表（``open`` の記録）
- ``archive_open`` … 前夜の plan × 当日の始値で作り直した順位表

結果は ``history/evaluation`` に追記する（1 回の evaluate が 1 ファイル。上書きしない）。
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from typing import TYPE_CHECKING, Any

import polars as pl

from daytrade.config import DaytradeConfig
from daytrade.history import ranking_frame
from daytrade.select import Quote, pick, rank, rank_short, shares_for
from wbcore.data.jquants_archive import Archive, endpoint
from wbcore.domain.jp_rules import DEFAULT_LOT_SIZE, price_limit_range
from wbcore.domain.models import Side

if TYPE_CHECKING:
    from daytrade.ledger import LedgerOrder
    from daytrade.plan import Plan

BARS = endpoint("equities_bars_daily")

#: 「次点」として見る順位の幅（N の先の件数）。``daytrade.cli.RANKING_EXTRA`` と同じ値
NEXT_ROWS = 5

#: ランキングの列（``daytrade.history.RANKING_SCHEMA`` と同じ）
_RANKING_COLUMNS = (
    "side",
    "rank",
    "symbol",
    "code",
    "name",
    "prev_close",
    "price",
    "gap",
    "vol20",
    "picked",
    "quantity",
    "amount",
    "n",
    "budget",
)

EVALUATION_SCHEMA: dict[str, Any] = {
    #: quotes（9:00 の気配）/ archive_open（plan × 始値で作り直し）
    "ranking_source": pl.Utf8,
    #: 元にした順位表の run_id（作り直しなら null）
    "ranking_run_id": pl.Utf8,
    "side": pl.Utf8,
    "rank": pl.Int32,
    "symbol": pl.Utf8,
    "code": pl.Utf8,
    "name": pl.Utf8,
    #: picked（選んだ）/ next（N の先 NEXT_ROWS 件）/ rest（それ以外の候補）
    "rank_group": pl.Utf8,
    "prev_close": pl.Float64,
    #: 順位付けに使った価格とギャップ（quotes なら 9:00 の気配、archive_open なら始値）
    "price": pl.Float64,
    "gap": pl.Float64,
    "vol20": pl.Float64,
    "picked": pl.Boolean,
    "quantity": pl.Float64,
    "amount": pl.Float64,
    "n": pl.Int32,
    "budget": pl.Float64,
    "open": pl.Float64,
    "high": pl.Float64,
    "low": pl.Float64,
    "close": pl.Float64,
    #: 実際の寄付ギャップ（始値 ÷ 前日終値 − 1）。quotes の gap との差が気配の当たり具合
    "gap_open": pl.Float64,
    #: 寄付 → 大引のリターン（符号は建て方向で見る前）
    "ret_oc": pl.Float64,
    #: 建て方向で見た損益（bp）。gross は費用前、net は費用（滑り・貸株料等）後
    "gross_bp": pl.Float64,
    "cost_bp": pl.Float64,
    "net_bp": pl.Float64,
    #: 「建てていたら」の株数と円損益。選んだ銘柄は記録の株数、それ以外は予算で買える株数
    "hypo_quantity": pl.Float64,
    "hypo_pnl": pl.Float64,
    #: 大引がストップ高／安（売建は返済が約定せず持ち越す。バックテストの carry と同じ条件）
    "limit_up_close": pl.Boolean,
    "limit_down_close": pl.Boolean,
    #: J-Quants のストップ高／安フラグ
    "ul_flag": pl.Boolean,
    "ll_flag": pl.Boolean,
    #: 台帳に本発注の約定があるか、その約定数量と実現損益（返済が未確定なら null）
    "traded": pl.Boolean,
    "filled_quantity": pl.Float64,
    "actual_pnl": pl.Float64,
}

RANK_GROUPS = ("picked", "next", "rest")


def _num(column: str) -> pl.Expr:
    return pl.col(column).cast(pl.Float64, strict=False)


def _flag(frame: pl.DataFrame, column: str, name: str) -> pl.Expr:
    if column in frame.columns:
        return (pl.col(column).cast(pl.String) == "1").alias(name)
    return pl.lit(None, dtype=pl.Boolean).alias(name)


def bars_for(archive: Archive, day: dt.date) -> pl.DataFrame:
    """その日の日足（``code``, ``open``, ``high``, ``low``, ``close``, ``ul_flag``, ``ll_flag``）。"""
    raw = archive.read(BARS, day, day)
    if raw.height == 0 or "Date" not in raw.columns:
        return pl.DataFrame(
            schema={
                "code": pl.String,
                "open": pl.Float64,
                "high": pl.Float64,
                "low": pl.Float64,
                "close": pl.Float64,
                "ul_flag": pl.Boolean,
                "ll_flag": pl.Boolean,
            }
        )
    raw = raw.filter(pl.col("Date") == day)
    return (
        raw.select(
            pl.col("Code").cast(pl.String).alias("code"),
            _num("O").alias("open"),
            _num("H").alias("high"),
            _num("L").alias("low"),
            _num("C").alias("close"),
            _flag(raw, "UL", "ul_flag"),
            _flag(raw, "LL", "ll_flag"),
        )
        .filter(pl.col("open") > 0, pl.col("close") > 0)
        .unique(subset=["code"], keep="last")
    )


def reconstruct_ranking(
    plan: Plan, bars: pl.DataFrame, config: DaytradeConfig, *, at: dt.datetime
) -> pl.DataFrame:
    """前夜の plan と当日の始値から、``open`` と同じ規則で順位表を作り直す。

    9:00 の気配の代わりに始値を使う（バックテストと同じ近似）。ロングは N・予算・配分を
    ``[capital]`` から、ショートは ``[margin]`` の通常日の倍率で。
    """
    opens = dict(bars.select("code", "open").iter_rows())
    quotes: dict[str, Quote] = {}
    for code, symbol in plan.frame.select("Code", "symbol").iter_rows():
        price = opens.get(code)
        if price is not None and price > 0:
            quotes[symbol] = Quote(
                symbol=symbol, price=Decimal(str(price)), at=at, source="archive_open"
            )
    n = config.capital.positions
    budget = config.capital.budget_per_order
    long_ranking = rank(plan.eligible, quotes, config.signal)
    long_picks = pick(
        plan.eligible,
        quotes,
        n=n,
        budget=budget,
        config=config.signal,
        weighting=config.capital.weighting,
        ranked=long_ranking,
    )
    frames = [ranking_frame(long_ranking, long_picks, side="BUY", n=n, budget=budget)]
    if (
        config.margin.enabled
        and config.margin.positions > 0
        and "short_eligible" in plan.frame.columns
    ):
        short_budget = (config.margin.budget_per_order * config.margin.multiplier_normal).quantize(
            Decimal(1)
        )
        short_ranking = rank_short(plan.short_eligible, quotes, config.margin)
        short_picks = pick(
            plan.short_eligible,
            quotes,
            n=config.margin.positions,
            budget=short_budget,
            config=config.signal,
            weighting=config.margin.weighting,
            ranked=short_ranking,
            side=Side.SELL,
        )
        frames.append(
            ranking_frame(
                short_ranking,
                short_picks,
                side="SELL",
                n=config.margin.positions,
                budget=short_budget,
            )
        )
    return pl.concat(frames)


def _actuals(orders: list[LedgerOrder]) -> dict[tuple[str, str], tuple[float, float | None]]:
    """台帳の本発注から (銘柄, 脚) → (約定数量, 実現損益)。返済の単価が無ければ損益は None。"""
    live = [o for o in orders if not o.is_dry_run and not o.is_dead]
    entries = {(o.symbol, o.leg): o for o in live if o.is_entry}
    exits = {(o.symbol, o.leg): o for o in live if o.is_exit}
    result: dict[tuple[str, str], tuple[float, float | None]] = {}
    for key, entry in entries.items():
        filled = float(entry.filled_quantity)
        if filled <= 0:
            continue
        exit_ = exits.get(key)
        pnl: float | None = None
        if exit_ is not None and exit_.avg_fill_price is not None and entry.avg_fill_price:
            if entry.side is Side.BUY:
                buy, sell = entry.avg_fill_price, exit_.avg_fill_price
            else:
                buy, sell = exit_.avg_fill_price, entry.avg_fill_price
            pnl = float((sell - buy) * exit_.filled_quantity)
        result[key] = (filled, pnl)
    return result


def _cost_bp(side: str, amount: float | None, config: DaytradeConfig) -> float:
    """往復の費用（bp）。信用は設定の見込み値、現物は手数料込みの実測式。"""
    if side == "SELL":
        return float(config.margin.extra_cost_bp)
    if config.margin.enabled and config.margin.long_via_margin:
        return float(config.margin.long_extra_cost_bp)
    from daytrade.fees import round_trip_bp

    return float(round_trip_bp(Decimal(str(amount or 0))))


def evaluate(
    ranking: pl.DataFrame,
    bars: pl.DataFrame,
    config: DaytradeConfig,
    orders: list[LedgerOrder],
    *,
    source: str,
) -> pl.DataFrame:
    """順位表の全行に日足と台帳を当てる。日足が無い銘柄は結果の列が null のまま残る。"""
    run_id = str(ranking["run_id"][0]) if "run_id" in ranking.columns and ranking.height else None
    base = ranking.select([c for c in _RANKING_COLUMNS if c in ranking.columns])
    joined = base.join(bars, on="code", how="left")
    actual = _actuals(orders)
    rows: dict[str, list[Any]] = {k: [] for k in EVALUATION_SCHEMA}

    for r in joined.iter_rows(named=True):
        side = r["side"]
        sign = -1.0 if side == "SELL" else 1.0
        n = int(r["n"] or 0)
        rank_ = int(r["rank"])
        picked = bool(r["picked"])
        group = "picked" if picked else ("next" if rank_ <= n + NEXT_ROWS else "rest")
        o, c, prev = r.get("open"), r.get("close"), r["prev_close"]
        budget = float(r["budget"] or 0)
        out: dict[str, Any] = {
            "ranking_source": source,
            "ranking_run_id": run_id,
            "rank_group": group,
            "open": o,
            "high": r.get("high"),
            "low": r.get("low"),
            "close": c,
            "ul_flag": r.get("ul_flag"),
            "ll_flag": r.get("ll_flag"),
        }
        for key in _RANKING_COLUMNS:
            out[key] = r.get(key)
        if o and c and prev:
            width_low, width_high = price_limit_range(Decimal(str(prev)))
            ret = c / o - 1
            gross = sign * ret * 1e4
            qty = (
                float(r["quantity"])
                if picked and r["quantity"]
                else float(shares_for(Decimal(str(budget)), Decimal(str(o)), DEFAULT_LOT_SIZE))
            )
            cost = _cost_bp(side, r.get("amount") or qty * o, config)
            out.update(
                gap_open=o / prev - 1,
                ret_oc=ret,
                gross_bp=gross,
                cost_bp=cost,
                net_bp=gross - cost,
                hypo_quantity=qty,
                hypo_pnl=qty * sign * (c - o) - qty * o * cost / 1e4,
                limit_up_close=c >= float(width_high) - 1e-6,
                limit_down_close=c <= float(width_low) + 1e-6,
            )
        leg = "short" if side == "SELL" else "long"
        filled, pnl = actual.get((r["symbol"], leg), (None, None))
        out.update(traded=filled is not None, filled_quantity=filled, actual_pnl=pnl)
        for key in EVALUATION_SCHEMA:
            rows[key].append(out.get(key))
    frame = pl.DataFrame(rows, schema=EVALUATION_SCHEMA)
    return frame.sort(["side", "rank"], descending=[False, False])


def summarize(evaluation: pl.DataFrame) -> pl.DataFrame:
    """脚 × 群（picked / next / rest）ごとの件数・平均 net bp・勝率・損益。"""
    if evaluation.height == 0:
        return pl.DataFrame()
    scored = evaluation.filter(pl.col("net_bp").is_not_null())
    order = {g: i for i, g in enumerate(RANK_GROUPS)}
    return (
        scored.group_by(["side", "rank_group"])
        .agg(
            pl.len().alias("count"),
            pl.col("net_bp").mean().alias("avg_net_bp"),
            (pl.col("net_bp") > 0).mean().alias("win_rate"),
            pl.col("hypo_pnl").sum().alias("hypo_pnl"),
            pl.col("actual_pnl").sum().alias("actual_pnl"),
            pl.col("traded").sum().alias("traded"),
        )
        .with_columns(
            pl.col("rank_group").replace_strict(order, default=99).alias("_o"),
        )
        .sort(["side", "_o"])
        .drop("_o")
    )


def latest_per_day(evaluations: pl.DataFrame) -> pl.DataFrame:
    """同じ日に何度も evaluate していれば最後の 1 回だけを残す。"""
    if evaluations.height == 0:
        return evaluations
    return evaluations.filter(pl.col("recorded_at") == pl.col("recorded_at").max().over("day"))


def review(evaluations: pl.DataFrame) -> pl.DataFrame:
    """日 × 脚ごとに「選んだ N」「次点」「候補全体」の平均 net bp と損益を並べる。

    選定が妥当なら、picked ≥ next ≥ all の順に並ぶ日が多いはず。逆が続くなら順位付けの
    規則（ギャップの小さい順／大きい順）がその相場で効いていない。
    """
    if evaluations.height == 0:
        return pl.DataFrame()
    scored = latest_per_day(evaluations).filter(pl.col("net_bp").is_not_null())

    def _avg(group: str) -> pl.Expr:
        return pl.col("net_bp").filter(pl.col("rank_group") == group).mean().alias(f"{group}_bp")

    return (
        scored.group_by(["day", "side"])
        .agg(
            pl.col("ranking_source").first().alias("source"),
            pl.col("picked").sum().alias("picked_n"),
            _avg("picked"),
            _avg("next"),
            pl.col("net_bp").mean().alias("all_bp"),
            pl.col("hypo_pnl").filter(pl.col("picked")).sum().alias("picked_pnl"),
            pl.col("actual_pnl").sum().alias("actual_pnl"),
            pl.col("traded").sum().alias("traded"),
            pl.len().alias("candidates"),
        )
        .sort(["day", "side"])
    )


def review_totals(table: pl.DataFrame) -> pl.DataFrame:
    """:func:`review` の表を脚ごとに合計・平均する。"""
    if table.height == 0:
        return pl.DataFrame()
    return (
        table.group_by("side")
        .agg(
            pl.len().alias("days"),
            pl.col("picked_bp").mean().alias("picked_bp"),
            pl.col("next_bp").mean().alias("next_bp"),
            pl.col("all_bp").mean().alias("all_bp"),
            (pl.col("picked_bp") > 0).mean().alias("picked_win_days"),
            (pl.col("picked_bp") > pl.col("all_bp")).mean().alias("beat_all_days"),
            pl.col("picked_pnl").sum().alias("picked_pnl"),
            pl.col("actual_pnl").sum().alias("actual_pnl"),
            pl.col("traded").sum().alias("traded"),
        )
        .sort("side")
    )


__all__ = [
    "BARS",
    "EVALUATION_SCHEMA",
    "NEXT_ROWS",
    "RANK_GROUPS",
    "bars_for",
    "evaluate",
    "latest_per_day",
    "reconstruct_ranking",
    "review",
    "review_totals",
    "summarize",
]
