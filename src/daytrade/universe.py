"""母集団（前夜に確定する条件）。J-Quants のアーカイブから「明日買ってよい銘柄」を絞る。

ここにあるのは polars の式と純粋関数だけ。同じ式を前夜の ``plan``（1 日ぶん）と
``backtest``（10 年ぶんのパネル）で使う——検証と実運用で条件がずれないために。

入力の列名は J-Quants V2 の生の列名（``Code`` / ``C`` / ``Va`` / ``MktCap`` / ``MktNm`` …）。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from decimal import Decimal

import polars as pl

from daytrade.config import MarginConfig, UniverseConfig

#: 東証の引け時刻が 15:00 → 15:30 に変わった日。決算開示が「引け後」かの判定に使う。
CLOSE_CHANGED_ON = dt.date(2024, 11, 5)

#: J-Quants の ``ProdCat``。011 が株式（ETF・REIT・優先株等を除く）。
STOCK_PRODUCT = "011"

#: 銘柄ごとのボラティリティ（日次リターンの標準偏差）を取る日数。配分の重みに使う。
VOL_DAYS = 20


def to_broker_symbol(code: str) -> str:
    """J-Quants の 5 桁コード（``72030`` / ``130A0``）を Webull の表記（``7203`` / ``130A``）に。"""
    code = code.strip()
    if len(code) == 5 and code.endswith("0"):
        return code[:4]
    return code


def segment_expr(column: str = "MktNm") -> pl.Expr:
    """市場区分名を prime / standard / growth / other に畳む。

    2022-04 の再編前（東証一部・二部・マザーズ・JASDAQ）も同じ呼び方に寄せる。
    ``JASDAQ グロース`` はグロース扱い。
    """
    name = pl.col(column).fill_null("")
    return (
        pl.when(name.str.contains("プライム") | name.str.contains("一部"))
        .then(pl.lit("prime"))
        .when(name.str.contains("グロース") | name.str.contains("マザーズ"))
        .then(pl.lit("growth"))
        .when(
            name.str.contains("スタンダード")
            | name.str.contains("二部")
            | name.str.contains("JASDAQ")
        )
        .then(pl.lit("standard"))
        .otherwise(pl.lit("other"))
    )


def shortable_expr(master: pl.DataFrame) -> pl.Expr:
    """貸借銘柄（制度信用で新規売りができる）か。``equities/master`` の ``Mrgn``: 1=信用 2=貸借 3=その他。

    列が無いフレーム（古い断片・テストの最小データ）では全て偽＝売れない側に倒す。
    """
    if "Mrgn" not in master.columns:
        return pl.lit(False)
    return pl.col("Mrgn").cast(pl.String) == "2"


def jsf_stop_expr(alert: pl.DataFrame) -> pl.Expr:
    """日証金の申込停止（売り禁）か。``markets/margin-alert`` の ``PubReason`` は JSON 文字列で、
    日々公表・監視・注意喚起・増担保・売り禁のどれで載ったかのフラグを持つ。
    ``RestrictedByJSF`` が "1" なら新規売りが出せない。列が無ければ全て偽。
    """
    if "PubReason" not in alert.columns:
        return pl.lit(False)
    return pl.col("PubReason").fill_null("").str.contains('"RestrictedByJSF": "1"')


def num(column: str) -> pl.Expr:
    """アーカイブは全列が文字列。数値に読む（読めなければ null）。"""
    return pl.col(column).cast(pl.Float64, strict=False)


def post_close_expr(date_column: str = "DiscDate", time_column: str = "DiscTime") -> pl.Expr:
    """決算開示が引け後か。引け時刻は 2024-11-05 から 15:30。"""
    close_at = (
        pl.when(pl.col(date_column) < CLOSE_CHANGED_ON)
        .then(pl.lit("15:00"))
        .otherwise(pl.lit("15:30"))
    )
    return pl.col(time_column).str.slice(0, 5) >= close_at


def cap_tercile_expr(
    column: str = "MktCap", *, mask: pl.Expr | None = None, over: str | None = "Date"
) -> pl.Expr:
    """時価総額の 3 分位（1=下位、3=上位）。

    ``mask`` が真の行だけを母集団にして順位を付ける（偽の行は null）。``over`` が None なら
    1 日ぶんの断面。順位も件数も母集団の中で数えないと、流動性の無い小型株が下位を埋めて
    分位が上に偏る。
    """
    value = pl.col(column) if mask is None else pl.when(mask).then(pl.col(column))
    rank = value.rank("ordinal")
    n = value.count()
    if over:
        rank, n = rank.over(over), n.over(over)
    return (rank * 3 / n).ceil().cast(pl.Int32).clip(1, 3)


def eligible_expr(config: UniverseConfig) -> pl.Expr:
    """母集団の条件。次の列を持つフレームに適用する。

    ``segment``（prime/standard/growth/other）, ``turnover_med``（売買代金の中央値）,
    ``cap_tercile``（1〜3）, ``earn_prev`` / ``disc_today`` / ``alert``（bool）。
    """
    cond = pl.col("segment").is_in(config.segments) & (
        pl.col("turnover_med") >= float(config.min_turnover)
    )
    if config.exclude_cap_terciles:
        cond = cond & (pl.col("cap_tercile") > config.exclude_cap_terciles)
    if config.exclude_earnings_prev:
        cond = cond & ~pl.col("earn_prev")
    if config.exclude_earnings_today:
        cond = cond & ~pl.col("disc_today")
    if config.exclude_margin_alert:
        cond = cond & ~pl.col("alert")
    return cond


def short_eligible_expr(config: MarginConfig) -> pl.Expr:
    """ショート（信用新規売り）の母集団。:func:`eligible_expr` と同じ列に加えて
    ``shortable``（貸借銘柄）と ``jsf_stop``（売り禁）を持つフレームに適用する。
    ロングとは区分・分位・規制の扱いが違う（``[margin]`` の各項目）。
    """
    cond = (
        pl.col("shortable")
        & pl.col("segment").is_in(config.segments)
        & (pl.col("turnover_med") >= float(config.min_turnover))
    )
    if config.exclude_cap_terciles:
        cond = cond & (pl.col("cap_tercile") > config.exclude_cap_terciles)
    if config.exclude_earnings_prev:
        cond = cond & ~pl.col("earn_prev")
    if config.exclude_earnings_today:
        cond = cond & ~pl.col("disc_today")
    if config.exclude_margin_alert:
        cond = cond & ~pl.col("alert")
    if config.exclude_jsf_stop:
        cond = cond & ~pl.col("jsf_stop")
    return cond


# --------------------------------------------------------------------------
# 1 日ぶんの候補（前夜の plan）
# --------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class Inputs:
    """``plan`` に渡すアーカイブの断片。すべて J-Quants の生の列名のまま。

    Attributes:
        bars: ``equities/bars/daily``。``turnover_days`` 日以上の直近の日足（全銘柄）。
        master: ``equities/master``。判定日以前の最新 1 日ぶん。
        fins: ``fins/summary``。前営業日の開示（無ければ空）。
        earnings_dates: ``fins/earnings-date``。当日を ``SchDate`` に持つ行（無ければ空）。
        margin_alert: ``markets/margin-alert``。前営業日 ``PubDate`` の行（無ければ空）。
    """

    bars: pl.DataFrame
    master: pl.DataFrame
    fins: pl.DataFrame
    earnings_dates: pl.DataFrame
    margin_alert: pl.DataFrame


def _flag(frame: pl.DataFrame, name: str) -> pl.DataFrame:
    if frame.height == 0 or "Code" not in frame.columns:
        return pl.DataFrame(
            {"Code": pl.Series([], dtype=pl.String), name: pl.Series([], dtype=pl.Boolean)}
        )
    return (
        frame.select(pl.col("Code").cast(pl.String)).unique().with_columns(pl.lit(True).alias(name))
    )


def candidates(
    inputs: Inputs,
    day: dt.date,
    prev_day: dt.date,
    config: UniverseConfig,
    margin: MarginConfig | None = None,
) -> pl.DataFrame:
    """判定日 ``day`` の候補。列: Code, symbol, name, segment, prev_close, turnover_med, mkt_cap,
    vol20（20 日の日次ボラ）, cap_tercile, earn_prev, disc_today, alert, jsf_stop, shortable,
    eligible, short_eligible。

    ``eligible`` が真の行が翌朝のロングの対象、``short_eligible`` が真の行がショートの対象
    （``margin`` を省くか無効なら全て偽）。真でない行も残す（なぜ外れたかを見せるため）。
    """
    bars = inputs.bars.filter(pl.col("Date") <= prev_day).with_columns(
        pl.col("Code").cast(pl.String)
    )
    if bars.height == 0:
        raise ValueError(f"{prev_day} までの足がありません")
    latest = bars.select(pl.col("Date").max()).item()
    if latest != prev_day:
        raise ValueError(
            f"前営業日 {prev_day} の足がありません（最新 {latest}）。jquants sync を先に"
        )
    feats = (
        bars.sort("Code", "Date")
        .with_columns(ret=(num("C") / num("C").shift(1) - 1).over("Code"))
        .group_by("Code", maintain_order=True)
        .agg(
            prev_close=num("C").last(),
            last_date=pl.col("Date").last(),
            turnover_med=num("Va").tail(config.turnover_days).median(),
            turnover_n=num("Va").tail(config.turnover_days).len(),
            mkt_cap=num("MktCap").last(),
            vol20=pl.col("ret").tail(VOL_DAYS).std(),
        )
        .filter(pl.col("last_date") == prev_day, pl.col("prev_close") > 0)
    )
    master = inputs.master.with_columns(pl.col("Code").cast(pl.String)).select(
        "Code",
        name=pl.col("CoName"),
        segment=segment_expr(),
        product=pl.col("ProdCat"),
        shortable=shortable_expr(inputs.master),
    )
    frame = (
        feats.join(master, on="Code", how="inner")
        .filter(pl.col("product") == STOCK_PRODUCT)
        # 分位は「株式かつ流動性の下限を満たす全銘柄」で切る（研究と同じ）
        .with_columns(
            cap_tercile=cap_tercile_expr(
                "mkt_cap", mask=pl.col("turnover_med") >= float(config.min_turnover), over=None
            )
        )
    )
    fins = inputs.fins
    if fins.height and "DiscTime" in fins.columns:
        fins = fins.filter(pl.col("DiscDate") == prev_day, post_close_expr())
    alert = inputs.margin_alert
    stopped = alert.filter(jsf_stop_expr(alert)) if alert.height else alert
    short_eligible = (
        short_eligible_expr(margin) if margin is not None and margin.enabled else pl.lit(False)
    )
    frame = (
        frame.join(_flag(fins, "earn_prev"), on="Code", how="left")
        .join(_flag(inputs.earnings_dates, "disc_today"), on="Code", how="left")
        .join(_flag(alert, "alert"), on="Code", how="left")
        .join(_flag(stopped, "jsf_stop"), on="Code", how="left")
        .with_columns(
            pl.col("earn_prev", "disc_today", "alert", "jsf_stop", "shortable").fill_null(False)
        )
        .with_columns(
            cap_tercile=pl.col("cap_tercile").fill_null(0).cast(pl.Int32),
            symbol=pl.col("Code").map_elements(to_broker_symbol, return_dtype=pl.String),
        )
        .with_columns(eligible=eligible_expr(config), short_eligible=short_eligible)
        .select(
            "Code",
            "symbol",
            "name",
            "segment",
            "prev_close",
            "turnover_med",
            "mkt_cap",
            "vol20",
            "cap_tercile",
            "earn_prev",
            "disc_today",
            "alert",
            "jsf_stop",
            "shortable",
            "eligible",
            "short_eligible",
        )
        .sort("Code")
    )
    return frame


def threshold_price(prev_close: Decimal, max_gap: Decimal) -> Decimal:
    """「寄付がこの値未満なら候補」の閾値（前日終値 × (1 + max_gap)）。気配で絞るときに使う。"""
    return (prev_close * (Decimal(1) + max_gap)).quantize(Decimal("0.1"))
