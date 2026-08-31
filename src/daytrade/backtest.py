"""アーカイブで同じ規則を検証する。

前夜の :func:`daytrade.universe.eligible_expr` と 9:00 の :func:`daytrade.select.gap_rank_expr`
をそのまま 10 年ぶんのパネルに当てる。資金は固定（複利なし）、100 株単位、手数料は
Webull の段階制（:mod:`daytrade.fees`）。研究ノートの表と同じ計算。
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
from daytrade.fees import commission
from daytrade.select import gap_rank_expr
from daytrade.universe import (
    STOCK_PRODUCT,
    cap_tercile_expr,
    eligible_expr,
    num,
    post_close_expr,
    segment_expr,
)
from wbcore.data.jquants_archive import Archive, endpoint

#: 1 年の営業日数（年率換算）。
TRADING_DAYS = 245


def _fee_expr(amount: pl.Expr) -> pl.Expr:
    """:func:`daytrade.fees.commission` の polars 版（片道）。"""
    return (
        pl.when(amount <= 0)
        .then(0)
        .when(amount <= 50_000)
        .then(55)
        .when(amount <= 100_000)
        .then(99)
        .when(amount <= 200_000)
        .then(115)
        .when(amount <= 1_000_000)
        .then(275)
        .when(amount <= 1_500_000)
        .then(535)
        .when(amount <= 30_000_000)
        .then(640)
        .otherwise(1070)
        .cast(pl.Float64)
    )


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
    master = archive.read(endpoint("equities_master"), lookback, end).select(
        "Date",
        pl.col("Code").cast(pl.String),
        segment=segment_expr(),
        product=pl.col("ProdCat"),
    )
    days = bars.select("Date").unique().sort("Date").with_row_index("di")
    win = config.universe.turnover_days
    panel = (
        bars.join(master, on=["Date", "Code"], how="left")
        .with_columns(
            prev_close=pl.when(pl.col("AF") == 1).then(pl.col("C").shift(1)).over("Code"),
            turnover_med=pl.col("Va").shift(1).rolling_median(win, min_samples=win).over("Code"),
            mkt_cap=pl.col("MktCap").shift(1).over("Code"),
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
        flagged = (
            alert.select(pl.col("Code").cast(pl.String), Date=pl.col("PubDate"))
            .join(days, on="Date")
            .select("Code", edi=pl.col("di") + 1)
            .unique()
            .with_columns(alert=True)
        )
        panel = panel.join(flagged, left_on=["Code", "di"], right_on=["Code", "edi"], how="left")
    else:
        panel = panel.with_columns(alert=pl.lit(False))
    return (
        panel.with_columns(pl.col("earn_prev", "disc_today", "alert").fill_null(False))
        .with_columns(segment=pl.col("segment").fill_null("other"))
        .with_columns(eligible=eligible_expr(config.universe) & pl.col("prev_close").is_not_null())
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
    picks = (
        panel.filter(pl.col("eligible"))
        .with_columns(shares=(pl.lit(budget) / (pl.col("O") * 100)).floor() * 100)
        .filter(pl.col("shares") >= 100)
        .with_columns(rank=gap_rank_expr(config.signal, over="Date"))
        .filter(pl.col("rank").is_not_null(), pl.col("rank") <= n)
        .with_columns(amount=pl.col("shares") * pl.col("O"))
        .with_columns(
            fees=2 * _fee_expr(pl.col("amount")),
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
        raise ValueError("us_skip_high を使うには米国市場のデータが要ります（yfinance）")
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
    on: list[bool] = []
    for i, row in enumerate(frame.iter_rows(named=True)):
        recent = None
        if r.equity_curve_days > 0 and i >= r.equity_curve_days:
            # 前日までの実現損益（止めた日は 0 として数える＝実運用と同じ）
            recent = sum(pnl[j] if on[j] else 0.0 for j in range(i - r.equity_curve_days, i))
        signals = Signals(
            day=row["Date"],
            iv_prev=row.get("iv_prev"),
            drift=row.get("drift"),
            market_gap=row.get("market_gap"),
            recent_pnl=recent,
            us_ret=row.get("spx_ret"),
            vix=row.get("vix"),
        )
        on.append(evaluate(r, signals).trade)
    frame = frame.with_columns(on=pl.Series(on, dtype=pl.Boolean))
    return frame.with_columns(
        *(
            pl.when("on").then(pl.col(c)).otherwise(0.0).alias(c)
            for c in ("pnl", "gross", "fees", "amount")
        ),
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


def daily_commission(amount: Decimal) -> Decimal:
    """テスト用: polars 版と :func:`daytrade.fees.commission` が一致することを確かめる入口。"""
    return commission(amount)
