"""アーカイブで同じ規則を検証する。

前夜の :func:`daytrade.universe.eligible_expr` と 9:00 の :func:`daytrade.select.gap_rank_expr`
をそのまま 10 年ぶんのパネルに当てる。資金は固定（複利なし）、100 株単位、手数料は
段階制の手数料（:mod:`daytrade.fees`）。研究ノートの表と同じ計算。
"""

from __future__ import annotations

import datetime as dt
import math
from dataclasses import dataclass
from decimal import Decimal
from pathlib import Path
from typing import Any

import polars as pl

from daytrade.config import DaytradeConfig
from daytrade.fees import FLAT_RATE_STEP, FLAT_RATE_TABLE, commission
from daytrade.select import gap_rank_expr, short_rank_expr
from daytrade.universe import (
    STOCK_PRODUCT,
    VOL_DAYS,
    cap_tercile_expr,
    eligible_expr,
    jsf_stop_expr,
    num,
    post_close_expr,
    segment_expr,
    short_eligible_expr,
    shortable_expr,
)
from wbcore.data.jquants_archive import Archive, endpoint

#: 1 年の営業日数（年率換算）。
TRADING_DAYS = 245


def _day_fee_expr(day_total: pl.Expr) -> pl.Expr:
    """:func:`daytrade.fees.commission` の polars 版。1 日の現物約定代金合計 → その日の手数料。"""
    chain: Any = pl.when(day_total <= 0).then(0.0)
    for bound, fee in FLAT_RATE_TABLE:
        chain = chain.when(day_total <= float(bound)).then(float(fee))
    top_bound, top_fee = FLAT_RATE_TABLE[-1]
    step_size, step_fee = FLAT_RATE_STEP
    extra_steps = ((day_total - float(top_bound)) / float(step_size)).ceil()
    result: pl.Expr = chain.otherwise(float(top_fee) + float(step_fee) * extra_steps)
    return result.cast(pl.Float64)


def _fee_expr(amount: pl.Expr, *, over: str = "Date") -> pl.Expr:
    """各取引の往復手数料。定額コースは 1 日の合計（買い＋売り = 2 × 代金）で段階が
    決まるので、その日の手数料を約定代金の比で各取引に配る。"""
    day_total = (2 * amount).sum().over(over)
    return pl.when(day_total > 0).then(_day_fee_expr(day_total) * amount / day_total * 2).otherwise(0.0)


def limit_width_expr(base: pl.Expr) -> pl.Expr:
    """:func:`wbcore.domain.jp_rules.price_limit_width` の polars 版。"""
    from wbcore.domain.jp_rules import price_limit_table

    expr: pl.Expr | None = None
    for bound, width in reversed(price_limit_table()):
        if bound == Decimal("Infinity"):
            expr = pl.lit(float(width))
            continue
        expr = pl.when(base < float(bound)).then(pl.lit(float(width))).otherwise(expr)
    assert expr is not None
    return expr


def load_panel(
    archive: Archive, start: dt.date, end: dt.date, config: DaytradeConfig
) -> pl.DataFrame:
    """(Date, Code) ごとの特徴量と当日の寄付・終値。前日までの情報だけで ``eligible`` を作る。"""
    lookback = start - dt.timedelta(days=config.universe.turnover_days * 2 + 10)
    bars = (
        archive.read(endpoint("equities_bars_daily"), lookback, end)
        .select(
            "Date",
            pl.col("Code").cast(pl.String),
            O=num("O"),
            C=num("C"),
            Va=num("Va"),
            AF=num("AdjFactor"),
            MktCap=num("MktCap"),
        )
        .filter(pl.col("O") > 0, pl.col("C") > 0)
        .sort("Code", "Date")
    )
    if bars.height == 0:
        raise ValueError("足がありません。jquants backfill / sync を先に")
    master_raw = archive.read(endpoint("equities_master"), lookback, end)
    master = master_raw.select(
        "Date",
        pl.col("Code").cast(pl.String),
        segment=segment_expr(),
        product=pl.col("ProdCat"),
        shortable=shortable_expr(master_raw),
    )
    days = bars.select("Date").unique().sort("Date").with_row_index("di")
    win = config.universe.turnover_days
    panel = (
        bars.join(master, on=["Date", "Code"], how="left")
        .with_columns(
            prev_close=pl.when(pl.col("AF") == 1).then(pl.col("C").shift(1)).over("Code"),
            # 翌営業日の寄付。ショートが引けストップ高で返済できなかったときの返済値（最終日は null）
            next_open=pl.col("O").shift(-1).over("Code"),
            turnover_med=pl.col("Va").shift(1).rolling_median(win, min_samples=win).over("Code"),
            mkt_cap=pl.col("MktCap").shift(1).over("Code"),
            vol20=(pl.col("C") / pl.col("C").shift(1) - 1)
            .rolling_std(VOL_DAYS)
            .shift(1)
            .over("Code"),
        )
        .filter(pl.col("Date") >= start, pl.col("product") == STOCK_PRODUCT)
        .join(days, on="Date")
    )
    # 分位は「株式かつ流動性の下限を満たす全銘柄」で日ごとに切る
    liquid = pl.col("turnover_med") >= float(config.universe.min_turnover)
    panel = panel.with_columns(
        cap_tercile=cap_tercile_expr("mkt_cap", mask=liquid, over="Date").fill_null(0)
    )
    # 決算（前日引け後）→ 翌営業日にフラグ。当日開示の予定は SchDate。日々公表は翌営業日
    fins = archive.read(endpoint("fins_summary"), start - dt.timedelta(days=7), end)
    if fins.height and "DiscTime" in fins.columns:
        post = (
            fins.filter(pl.col("DiscTime").is_not_null(), post_close_expr())
            .select(pl.col("Code").cast(pl.String), Date=pl.col("DiscDate"))
            .join(days, on="Date")
            .select("Code", edi=pl.col("di") + 1)
            .unique()
            .with_columns(earn_prev=True)
        )
        panel = panel.join(post, left_on=["Code", "di"], right_on=["Code", "edi"], how="left")
    else:
        panel = panel.with_columns(earn_prev=pl.lit(False))
    sched = archive.read(endpoint("fins_earnings_date"), start - dt.timedelta(days=120), end)
    if sched.height and "SchDate" in sched.columns:
        today = (
            sched.filter(pl.col("PubDate") < pl.col("SchDate"))
            .select(pl.col("Code").cast(pl.String), Date=pl.col("SchDate"))
            .unique()
            .with_columns(disc_today=True)
        )
        panel = panel.join(today, on=["Code", "Date"], how="left")
    else:
        panel = panel.with_columns(disc_today=pl.lit(False))
    alert = archive.read(endpoint("markets_margin_alert"), start - dt.timedelta(days=7), end)
    if alert.height and "PubDate" in alert.columns:
        # 規制の理由を問わず「載った」フラグと、売り禁（新規売り不可）のフラグ。どちらも翌営業日に効く
        for name, rows in (("alert", alert), ("jsf_stop", alert.filter(jsf_stop_expr(alert)))):
            flagged = (
                rows.select(pl.col("Code").cast(pl.String), Date=pl.col("PubDate"))
                .join(days, on="Date")
                .select("Code", edi=pl.col("di") + 1)
                .unique()
                .with_columns(pl.lit(True).alias(name))
            )
            panel = panel.join(
                flagged, left_on=["Code", "di"], right_on=["Code", "edi"], how="left"
            )
    else:
        panel = panel.with_columns(alert=pl.lit(False), jsf_stop=pl.lit(False))
    has_prev = pl.col("prev_close").is_not_null()
    short_eligible = (
        short_eligible_expr(config.margin) & has_prev if config.margin.enabled else pl.lit(False)
    )
    return (
        panel.with_columns(pl.col("earn_prev", "disc_today", "alert", "jsf_stop").fill_null(False))
        .with_columns(segment=pl.col("segment").fill_null("other"))
        .with_columns(shortable=pl.col("shortable").fill_null(False))
        .with_columns(
            eligible=eligible_expr(config.universe) & has_prev, short_eligible=short_eligible
        )
        .with_columns(gap=pl.col("O") / pl.col("prev_close") - 1)
    )


def iv_by_day(archive: Archive, start: dt.date, end: dt.date) -> pl.DataFrame:
    """日経 225 オプションの ``BaseVol`` の日次中央値（前日値を ``iv_prev`` に）。"""
    opt = archive.read(
        endpoint("derivatives_bars_daily_options_225"), start - dt.timedelta(days=10), end
    )
    if opt.height == 0 or "BaseVol" not in opt.columns:
        return pl.DataFrame(
            {"Date": pl.Series([], dtype=pl.Date), "iv_prev": pl.Series([], dtype=pl.Float64)}
        )
    return (
        opt.select("Date", bv=num("BaseVol"))
        .group_by("Date")
        .agg(iv=pl.col("bv").median())
        .sort("Date")
        .with_columns(iv_prev=pl.col("iv").shift(1))
        .select("Date", "iv_prev")
    )


@dataclass(frozen=True, slots=True)
class Summary:
    days: int
    traded_days: int
    capital: Decimal
    total_pnl: float
    mean_daily: float
    annual_return: float
    sharpe: float
    max_drawdown: float
    win_rate: float
    monthly_mean: float
    monthly_median: float
    monthly_p10: float
    monthly_win: float
    avg_positions: float
    round_trip_bp: float


@dataclass(frozen=True, slots=True)
class Result:
    daily: pl.DataFrame
    trades: pl.DataFrame
    summary: Summary

    def yearly(self) -> pl.DataFrame:
        return (
            self.daily.group_by(pl.col("Date").dt.year().alias("year"))
            .agg(
                days=pl.len(),
                traded=(pl.col("n") > 0).sum(),
                pnl=pl.col("pnl").sum(),
                mean_daily=pl.col("pnl").mean(),
                win=(pl.col("pnl") > 0).filter(pl.col("n") > 0).mean(),
            )
            .sort("year")
        )

    def monthly(self) -> pl.DataFrame:
        return (
            self.daily.group_by_dynamic("Date", every="1mo")
            .agg(pnl=pl.col("pnl").sum(), days=pl.len())
            .sort("Date")
        )


def simulate(
    panel: pl.DataFrame,
    config: DaytradeConfig,
    *,
    iv: pl.DataFrame | None = None,
    drift: pl.DataFrame | None = None,
    us: pl.DataFrame | None = None,
) -> Result:
    """パネルに規則を当て、資金固定で日次損益を出す。

    危険信号（:mod:`daytrade.regime`）は日次損益に後から掛ける: 止めた日は 0。
    実運用の ``open`` と同じ判定（:func:`daytrade.regime.evaluate`）を日ごとに呼ぶ。
    """
    n = config.capital.positions
    if n == 0:
        raise ValueError("max_capital が 0 のため検証できません（買わない設定）")
    budget = float(config.capital.budget_per_order)
    capital = float(config.capital.max_capital)
    eligible = panel.filter(pl.col("eligible"))
    if config.signal.skip_limit_down:
        # 寄付がストップ安（前日終値 − 制限値幅）以下なら買わない。実運用と同じ条件
        low = (pl.col("prev_close") - limit_width_expr(pl.col("prev_close"))).clip(lower_bound=1.0)
        eligible = eligible.filter(pl.col("O") > low)
    picks = (
        eligible.with_columns(shares=(pl.lit(budget) / (pl.col("O") * 100)).floor() * 100)
        .filter(pl.col("shares") >= 100)
        .with_columns(rank=gap_rank_expr(config.signal, over="Date"))
        .filter(pl.col("rank").is_not_null(), pl.col("rank") <= n)
    )
    if config.capital.weighting == "inverse_vol":
        # 選んだ N の中で 20 日ボラの逆数で按分（実運用の select.weights と同じ）
        from daytrade.select import VOL_FLOOR

        if "vol20" not in picks.columns:
            picks = picks.with_columns(vol20=pl.lit(None, dtype=pl.Float64))

        picks = (
            picks.with_columns(
                w=1.0 / pl.max_horizontal(pl.col("vol20").fill_null(VOL_FLOOR), pl.lit(VOL_FLOOR))
            )
            .with_columns(
                shares=(
                    pl.lit(capital)
                    * pl.col("w")
                    / pl.col("w").sum().over("Date")
                    / (pl.col("O") * 100)
                ).floor()
                * 100
            )
            .filter(pl.col("shares") >= 100)
        )
    picks = (
        picks.with_columns(amount=pl.col("shares") * pl.col("O"))
        .with_columns(
            fees=_fee_expr(pl.col("amount")),
            gross=pl.col("shares") * (pl.col("C") - pl.col("O")),
        )
        .with_columns(pnl=pl.col("gross") - pl.col("fees"))
    )
    daily = (
        picks.group_by("Date")
        .agg(
            pnl=pl.col("pnl").sum(),
            gross=pl.col("gross").sum(),
            fees=pl.col("fees").sum(),
            amount=pl.col("amount").sum(),
            n=pl.len(),
        )
        .sort("Date")
    )
    # 取引の無い営業日も 0 で並べる（日次の統計を暦日ベースにする）
    all_days = panel.select("Date").unique().sort("Date")
    daily = all_days.join(daily, on="Date", how="left").with_columns(
        pl.col("pnl", "gross", "fees", "amount").fill_null(0.0), pl.col("n").fill_null(0)
    )
    market_gap = (
        panel.filter(pl.col("eligible")).group_by("Date").agg(market_gap=pl.col("gap").median())
    )
    daily = _apply_regime(daily, config, iv=iv, drift=drift, market_gap=market_gap, us=us)
    picks = picks.join(daily.select("Date", "on"), on="Date", how="left").filter(pl.col("on"))
    return Result(daily=daily, trades=picks.sort("Date", "rank"), summary=_summary(daily, capital))


# --------------------------------------------------------------------------
# jp_gap_fade_margin: ロング（gap_fade）+ ショート（信用売り）
# --------------------------------------------------------------------------


def _pick_and_price(
    eligible: pl.DataFrame,
    rank_expr: pl.Expr,
    *,
    n: int,
    budget: float,
    capital: float,
    weighting: str,
    sign: int,
    extra_cost_bp: float = 0.0,
    commission: bool = True,
) -> pl.DataFrame:
    """ランク付け・按分・価格付けの共通部分（:func:`simulate` のロング側の計算を一般化）。

    ``sign`` が損益の向き（買い +1: ``C − O``、売り −1: ``O − C``）。``extra_cost_bp`` は
    約定代金に対する往復の概算コスト（貸株料・金利・滑り、bp）。``commission`` を
    偽にすると現物の定額手数料（:func:`_fee_expr`）を掛けない（立花証券の信用取引は 0 円）。
    """
    picks = (
        eligible.with_columns(shares=(pl.lit(budget) / (pl.col("O") * 100)).floor() * 100)
        .filter(pl.col("shares") >= 100)
        .with_columns(rank=rank_expr)
        .filter(pl.col("rank").is_not_null(), pl.col("rank") <= n)
    )
    if weighting == "inverse_vol":
        from daytrade.select import VOL_FLOOR

        if "vol20" not in picks.columns:
            picks = picks.with_columns(vol20=pl.lit(None, dtype=pl.Float64))
        picks = (
            picks.with_columns(
                w=1.0 / pl.max_horizontal(pl.col("vol20").fill_null(VOL_FLOOR), pl.lit(VOL_FLOOR))
            )
            .with_columns(
                shares=(
                    pl.lit(capital)
                    * pl.col("w")
                    / pl.col("w").sum().over("Date")
                    / (pl.col("O") * 100)
                ).floor()
                * 100
            )
            .filter(pl.col("shares") >= 100)
        )
    base_fee = _fee_expr(pl.col("amount")) if commission else pl.lit(0.0)
    return (
        picks.with_columns(amount=pl.col("shares") * pl.col("O"))
        .with_columns(
            fees=base_fee + pl.col("amount") * (extra_cost_bp / 1e4),
            gross=pl.col("shares") * sign * (pl.col("C") - pl.col("O")),
        )
        .with_columns(pnl=pl.col("gross") - pl.col("fees"))
    )


def _apply_carry(picks: pl.DataFrame, limit_up: pl.Expr, penalty: float) -> pl.DataFrame:
    """引けがストップ高（``C`` が制限値幅の上限）の売建は、買い気配に張り付いて返済買いが
    約定しないので翌営業日の寄付（``next_open``）で返済したことにする。

    損益は ``penalty`` の割合だけ翌寄りに置き換える（1 で全額、0 で無視）——実際に約定しない
    割合は日足からは分からないため。``carried`` 列に該当を残す。最終日（翌寄りが無い）は引け値のまま。
    """
    if "next_open" not in picks.columns:
        return picks.with_columns(carried=pl.lit(False))
    # 浮動小数の丸め（952.4 + 150 など）で上限をわずかに下回ることがあるので 1e-6 の余裕を持つ
    carried = (pl.col("C") >= limit_up - 1e-6) & pl.col("next_open").is_not_null()
    extra = (
        pl.when(carried)
        .then(penalty * pl.col("shares") * (pl.col("C") - pl.col("next_open")))
        .otherwise(0.0)
    )
    return picks.with_columns(carried=carried, gross=pl.col("gross") + extra).with_columns(
        pnl=pl.col("gross") - pl.col("fees")
    )


def _daily_from_picks(picks: pl.DataFrame, panel: pl.DataFrame) -> pl.DataFrame:
    """1 レッグぶんの日次集計（取引の無い日も 0 で並べる）。"""
    daily = (
        picks.group_by("Date")
        .agg(
            pnl=pl.col("pnl").sum(),
            gross=pl.col("gross").sum(),
            fees=pl.col("fees").sum(),
            amount=pl.col("amount").sum(),
            n=pl.len(),
        )
        .sort("Date")
    )
    all_days = panel.select("Date").unique().sort("Date")
    return all_days.join(daily, on="Date", how="left").with_columns(
        pl.col("pnl", "gross", "fees", "amount").fill_null(0.0), pl.col("n").fill_null(0)
    )


@dataclass(frozen=True, slots=True)
class MarginResult:
    """:func:`simulate_margin` の結果。ロング・ショートを合算した ``daily`` に加え、
    レッグごとの内訳（``long_*`` / ``short_*`` 列と個別の :class:`Summary`）を持つ。
    """

    daily: pl.DataFrame
    long_trades: pl.DataFrame
    short_trades: pl.DataFrame
    summary: Summary
    long_summary: Summary
    short_summary: Summary

    def yearly(self) -> pl.DataFrame:
        return (
            self.daily.group_by(pl.col("Date").dt.year().alias("year"))
            .agg(
                days=pl.len(),
                traded=(pl.col("n") > 0).sum(),
                pnl=pl.col("pnl").sum(),
                long_pnl=pl.col("long_pnl").sum(),
                short_pnl=pl.col("short_pnl").sum(),
                mean_daily=pl.col("pnl").mean(),
                win=(pl.col("pnl") > 0).filter(pl.col("n") > 0).mean(),
            )
            .sort("year")
        )


def simulate_margin(
    panel: pl.DataFrame,
    config: DaytradeConfig,
    *,
    iv: pl.DataFrame | None = None,
    drift: pl.DataFrame | None = None,
    us: pl.DataFrame | None = None,
) -> MarginResult:
    """ロング（``jp_gap_fade`` と同じ規則）とショート（信用売り）を合わせて検証する。

    ショート側の資金配分は、ロング側の資産曲線ゲート（``regime.equity_curve_days`` /
    ``equity_curve_scale``）に連動する「シーソー」——ロング側が通常運転の日は
    ``margin.multiplier_normal`` 倍、縮小された日は ``margin.multiplier_long_weak`` 倍。
    危険信号そのもの（月・IV 等、``verdict.trade is False``）で止まる日は両側とも休む。

    倍率によるショートの増減は、実際にその倍率で銘柄を
    選び直す（単元の切り捨てをやり直す）のではなく、基準資金で選んだ結果の
    損益を後から掛け増す近似——既存の ``equity_curve_scale`` によるロングの
    縮小と同じ手法。
    """
    if not config.margin.enabled:
        raise ValueError(
            "margin.enabled が false です（jp_gap_fade と同じ結果になるので simulate を使う）"
        )
    n_long = config.capital.positions
    n_short = config.margin.positions
    if n_long == 0:
        raise ValueError("capital.max_capital が 0 のため検証できません")
    if n_short == 0:
        raise ValueError("margin.max_capital が 0 のためショートを検証できません")

    long_capital = float(config.capital.max_capital)
    short_capital = float(config.margin.max_capital)

    long_eligible = panel.filter(pl.col("eligible"))
    if config.signal.skip_limit_down:
        low = (pl.col("prev_close") - limit_width_expr(pl.col("prev_close"))).clip(lower_bound=1.0)
        long_eligible = long_eligible.filter(pl.col("O") > low)
    long_picks = _pick_and_price(
        long_eligible,
        gap_rank_expr(config.signal, over="Date"),
        n=n_long,
        budget=float(config.capital.budget_per_order),
        capital=long_capital,
        weighting=config.capital.weighting,
        sign=1,
        # 信用買い（日計り）なら手数料 0 円。金利・滑りは long_extra_cost_bp で見る
        extra_cost_bp=float(config.margin.long_extra_cost_bp)
        if config.margin.long_via_margin
        else 0.0,
        commission=not config.margin.long_via_margin,
    )

    # ショートの母集団はロングと別（[margin] の segments / 除外。前夜の plan と同じ式）
    short_eligible = panel.filter(pl.col("short_eligible"))
    limit_up = pl.col("prev_close") + limit_width_expr(pl.col("prev_close"))
    if config.margin.skip_limit_up:
        short_eligible = short_eligible.filter(pl.col("O") < limit_up)
    short_picks = _pick_and_price(
        short_eligible,
        short_rank_expr(config.margin, over="Date"),
        n=n_short,
        budget=float(config.margin.budget_per_order),
        capital=short_capital,
        weighting=config.margin.weighting,
        sign=-1,
        extra_cost_bp=float(config.margin.extra_cost_bp),
        commission=False,  # 立花証券の信用取引は手数料 0 円
    )
    short_picks = _apply_carry(short_picks, limit_up, float(config.margin.carry_penalty))

    long_daily = _daily_from_picks(long_picks, panel)
    short_daily = _daily_from_picks(short_picks, panel)

    market_gap = (
        panel.filter(pl.col("eligible")).group_by("Date").agg(market_gap=pl.col("gap").median())
    )
    combined = _apply_regime_seesaw(
        long_daily, short_daily, config, iv=iv, drift=drift, market_gap=market_gap, us=us
    )

    long_trades = long_picks.join(
        combined.select("Date", "long_scale"), on="Date", how="left"
    ).filter(pl.col("long_scale") > 0)
    short_trades = short_picks.join(
        combined.select("Date", "short_multiplier"), on="Date", how="left"
    ).filter(pl.col("short_multiplier") > 0)

    return MarginResult(
        daily=combined,
        long_trades=long_trades.sort("Date", "rank"),
        short_trades=short_trades.sort("Date", "rank"),
        summary=_summary(combined, long_capital + short_capital),
        long_summary=_summary(
            combined.select(
                "Date",
                pnl=pl.col("long_pnl"),
                amount=pl.col("long_amount"),
                fees=pl.col("long_fees"),
                n=pl.col("long_n"),
            ),
            long_capital,
        ),
        short_summary=_summary(
            combined.select(
                "Date",
                pnl=pl.col("short_pnl"),
                amount=pl.col("short_amount"),
                fees=pl.col("short_fees"),
                n=pl.col("short_n"),
            ),
            short_capital,
        ),
    )


def _apply_regime_seesaw(
    long_daily: pl.DataFrame,
    short_daily: pl.DataFrame,
    config: DaytradeConfig,
    *,
    iv: pl.DataFrame | None,
    drift: pl.DataFrame | None,
    market_gap: pl.DataFrame,
    us: pl.DataFrame | None,
) -> pl.DataFrame:
    """日ごとに :func:`daytrade.regime.evaluate` を呼び、ロングの資産曲線ゲートに
    応じてショートの資金をシーソーさせる。

    「戦略自身の直近の損益」（資産曲線ゲートの入力）は :func:`_apply_regime` と同じ定義
    ——**ロング側**（gap_fade）の実現損益のみを見る。ショート側の成績でロング側を
    動かすことはしない（ロングは既存 ``jp_gap_fade`` と同じ挙動を保つため）。
    """
    from daytrade.regime import Signals, evaluate

    r = config.regime
    m = config.margin
    if r.iv_gate > 0 and (iv is None or iv.height == 0):
        raise ValueError("iv_gate を使うにはオプションのアーカイブが要ります")
    if r.drift_gate is not None and (drift is None or drift.height == 0):
        raise ValueError("drift_gate を使うには TOPIX のアーカイブが要ります")
    if r.us_skip_high is not None and (us is None or us.height == 0):
        raise ValueError("us_skip_high を使うには米国市場のデータが要ります（FRED）")

    frame = (
        long_daily.rename(
            {
                "pnl": "long_pnl",
                "gross": "long_gross",
                "fees": "long_fees",
                "amount": "long_amount",
                "n": "long_n",
            }
        )
        .join(
            short_daily.rename(
                {
                    "pnl": "short_pnl",
                    "gross": "short_gross",
                    "fees": "short_fees",
                    "amount": "short_amount",
                    "n": "short_n",
                }
            ),
            on="Date",
        )
        .join(market_gap, on="Date", how="left")
    )
    frame = (
        frame.join(iv, on="Date", how="left")
        if iv is not None and iv.height
        else frame.with_columns(iv_prev=None)
    )
    frame = (
        frame.join(drift, on="Date", how="left")
        if drift is not None and drift.height
        else frame.with_columns(drift=None)
    )
    frame = (
        frame.join(us, on="Date", how="left")
        if us is not None and us.height
        else frame.with_columns(spx_ret=None, vix=None)
    )
    frame = frame.sort("Date")

    long_pnl_list = frame["long_pnl"].to_list()
    long_scales: list[float] = []
    short_multipliers: list[float] = []
    for i, row in enumerate(frame.iter_rows(named=True)):
        recent = None
        if r.equity_curve_days > 0 and i >= r.equity_curve_days:
            # 前日までの「ロング側」実現損益（縮めた日はその倍率で数える＝実運用と同じ）
            recent = sum(
                long_pnl_list[j] * long_scales[j] for j in range(i - r.equity_curve_days, i)
            )
        signals = Signals(
            day=row["Date"],
            iv_prev=row.get("iv_prev"),
            drift=row.get("drift"),
            market_gap=row.get("market_gap"),
            recent_pnl=recent,
            us_ret=row.get("spx_ret"),
            vix=row.get("vix"),
        )
        verdict = evaluate(r, signals)
        weak = verdict.trade and verdict.scale < 1.0  # 資産曲線の合図（地合いが弱い）
        if not verdict.trade:
            long_scale = 0.0
        elif weak and not m.long_shrink:
            long_scale = 1.0  # 合図はショートにだけ使い、ロングは縮めない
        else:
            long_scale = verdict.scale
        long_scales.append(long_scale)
        if not verdict.trade:
            short_multiplier = 0.0  # 危険信号そのものはショートも止める
        elif weak:
            short_multiplier = float(m.multiplier_long_weak)  # シーソーで増強
        else:
            short_multiplier = float(m.multiplier_normal)
        short_multipliers.append(short_multiplier)

    frame = frame.with_columns(
        long_scale=pl.Series(long_scales, dtype=pl.Float64),
        short_multiplier=pl.Series(short_multipliers, dtype=pl.Float64),
    )
    frame = frame.with_columns(
        long_pnl=pl.col("long_pnl") * pl.col("long_scale"),
        long_gross=pl.col("long_gross") * pl.col("long_scale"),
        long_fees=pl.col("long_fees") * pl.col("long_scale"),
        long_amount=pl.col("long_amount") * pl.col("long_scale"),
        long_n=pl.when(pl.col("long_scale") > 0).then(pl.col("long_n")).otherwise(0),
        short_pnl=pl.col("short_pnl") * pl.col("short_multiplier"),
        short_gross=pl.col("short_gross") * pl.col("short_multiplier"),
        short_fees=pl.col("short_fees") * pl.col("short_multiplier"),
        short_amount=pl.col("short_amount") * pl.col("short_multiplier"),
        short_n=pl.when(pl.col("short_multiplier") > 0).then(pl.col("short_n")).otherwise(0),
    )
    return frame.with_columns(
        pnl=pl.col("long_pnl") + pl.col("short_pnl"),
        gross=pl.col("long_gross") + pl.col("short_gross"),
        fees=pl.col("long_fees") + pl.col("short_fees"),
        amount=pl.col("long_amount") + pl.col("short_amount"),
        n=pl.col("long_n") + pl.col("short_n"),
        on=(pl.col("long_scale") > 0) | (pl.col("short_multiplier") > 0),
    )


def _apply_regime(
    daily: pl.DataFrame,
    config: DaytradeConfig,
    *,
    iv: pl.DataFrame | None,
    drift: pl.DataFrame | None,
    market_gap: pl.DataFrame,
    us: pl.DataFrame | None = None,
) -> pl.DataFrame:
    """日ごとに :func:`daytrade.regime.evaluate` を呼び、止めた日の損益を 0 にする。"""
    from daytrade.regime import Signals, evaluate

    r = config.regime
    if r.iv_gate > 0 and (iv is None or iv.height == 0):
        raise ValueError("iv_gate を使うにはオプションのアーカイブが要ります")
    if r.drift_gate is not None and (drift is None or drift.height == 0):
        raise ValueError("drift_gate を使うには TOPIX のアーカイブが要ります")
    if r.us_skip_high is not None and (us is None or us.height == 0):
        raise ValueError("us_skip_high を使うには米国市場のデータが要ります（FRED）")
    frame = daily.join(market_gap, on="Date", how="left")
    frame = (
        frame.join(iv, on="Date", how="left")
        if iv is not None and iv.height
        else frame.with_columns(iv_prev=None)
    )
    frame = (
        frame.join(drift, on="Date", how="left")
        if drift is not None and drift.height
        else frame.with_columns(drift=None)
    )
    if us is not None and us.height:
        frame = frame.join(us, on="Date", how="left")
    else:
        frame = frame.with_columns(spx_ret=None, vix=None)
    frame = frame.sort("Date")
    pnl = frame["pnl"].to_list()
    scales: list[float] = []
    for i, row in enumerate(frame.iter_rows(named=True)):
        recent = None
        if r.equity_curve_days > 0 and i >= r.equity_curve_days:
            # 前日までの実現損益（縮めた日はその倍率で、止めた日は 0 で数える＝実運用と同じ）
            recent = sum(pnl[j] * scales[j] for j in range(i - r.equity_curve_days, i))
        signals = Signals(
            day=row["Date"],
            iv_prev=row.get("iv_prev"),
            drift=row.get("drift"),
            market_gap=row.get("market_gap"),
            recent_pnl=recent,
            us_ret=row.get("spx_ret"),
            vix=row.get("vix"),
        )
        verdict = evaluate(r, signals)
        scales.append(verdict.scale if verdict.trade else 0.0)
    frame = frame.with_columns(scale=pl.Series(scales, dtype=pl.Float64)).with_columns(
        on=pl.col("scale") > 0
    )
    return frame.with_columns(
        *((pl.col(c) * pl.col("scale")).alias(c) for c in ("pnl", "gross", "fees", "amount")),
        n=pl.when("on").then(pl.col("n")).otherwise(0),
    )


def _f(value: Any) -> float:
    """polars の集計値（None を含む）を float に。"""
    return float(value or 0.0)


def _summary(daily: pl.DataFrame, capital: float) -> Summary:
    pnl = daily["pnl"]
    days = len(pnl)
    if days == 0:
        raise ValueError("対象日がありません")
    equity = pnl.cum_sum()
    mdd = _f((equity - equity.cum_max()).min())
    std = _f(pnl.std())
    sharpe = _f(pnl.mean()) / std * math.sqrt(TRADING_DAYS) if std > 0 else 0.0
    monthly = daily.group_by_dynamic("Date", every="1mo").agg(
        pnl=pl.col("pnl").sum(), days=pl.len()
    )
    monthly = monthly.filter(pl.col("days") >= 15)
    m = monthly["pnl"] if monthly.height else pl.Series([0.0])
    traded = daily.filter(pl.col("n") > 0)
    amount = _f(traded["amount"].sum())
    fees = _f(traded["fees"].sum())
    return Summary(
        days=days,
        traded_days=traded.height,
        capital=Decimal(capital),
        total_pnl=_f(pnl.sum()),
        mean_daily=_f(pnl.mean()),
        annual_return=_f(pnl.mean()) * TRADING_DAYS / capital,
        sharpe=sharpe,
        max_drawdown=mdd,
        win_rate=_f((traded["pnl"] > 0).mean()) if traded.height else 0.0,
        monthly_mean=_f(m.mean()),
        monthly_median=_f(m.median()),
        monthly_p10=_f(m.quantile(0.1)),
        monthly_win=_f((m > 0).mean()),
        avg_positions=_f(traded["n"].mean()) if traded.height else 0.0,
        round_trip_bp=fees / amount * 1e4 if amount > 0 else 0.0,
    )


def run(
    archive: Archive,
    config: DaytradeConfig,
    start: dt.date,
    end: dt.date,
    *,
    us_cache: Path | None = None,
) -> Result:
    """アーカイブ（＋米国市場のキャッシュ）から検証する。"""
    from daytrade.regime import topix_drift_series
    from daytrade.usmarket import as_of_frame, history

    panel = load_panel(archive, start, end, config)
    iv = iv_by_day(archive, start, end)
    drift = topix_drift_series(archive, start, end, config.regime.drift_days)
    us = None
    if config.regime.us_skip_high is not None:
        cache = us_cache or (archive.root.parent / "daytrade" / "us.parquet")
        us = as_of_frame(history(cache, start, end), panel.select("Date").unique())
    return simulate(panel, config, iv=iv, drift=drift, us=us)


def run_margin(
    archive: Archive,
    config: DaytradeConfig,
    start: dt.date,
    end: dt.date,
    *,
    us_cache: Path | None = None,
) -> MarginResult:
    """:func:`run` のロング＋ショート版（:func:`simulate_margin`）。"""
    from daytrade.regime import topix_drift_series
    from daytrade.usmarket import as_of_frame, history

    panel = load_panel(archive, start, end, config)
    iv = iv_by_day(archive, start, end)
    drift = topix_drift_series(archive, start, end, config.regime.drift_days)
    us = None
    if config.regime.us_skip_high is not None:
        cache = us_cache or (archive.root.parent / "daytrade" / "us.parquet")
        us = as_of_frame(history(cache, start, end), panel.select("Date").unique())
    return simulate_margin(panel, config, iv=iv, drift=drift, us=us)


def daily_commission(day_total: Decimal) -> Decimal:
    """テスト用: polars 版（:func:`_day_fee_expr`）と :func:`daytrade.fees.commission` を突き合わせる入口。"""
    return commission(day_total)
