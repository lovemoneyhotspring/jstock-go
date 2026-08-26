"""積立計画。日足と戦術から「その日いくら投下するか」を決める。

**投下の設計（検証で選んだ形）**

基本分は入金日にその月の予算を全額投下する。分割しない。現金を
持っている日数と平均取得単価の悪化はほぼ完全な比例関係にあり
（実測で約 0.068%/日）、月内に分割するとその日数ぶんだけ確実に
高く買う。

増額分だけは日次で判定し、倍率が 1 を超えた日に分けて入れる。
月に一度しか見ないと短い下降局面を取り逃がし、かといって基本分ごと
日次に分けると待機コストを払うことになるため、**判定は日次・
基本分の投下は月次**という組み合わせにしている。10年窓の検証では
この形が勝率と最悪値の両方で最良だった。

増額分の原資は新規資金（賞与・余剰収入）を想定している。積立予算を
取り置いて作ると待機が発生し、増額の利益をそのまま打ち消す。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from decimal import Decimal

import polars as pl

from wbjp.accumulate.tactics import MULTIPLIER, BearStack, Tactic

#: 計画表の列。
PLAN_COLUMNS = ("date", "close", MULTIPLIER, "base", "extra", "amount", "reason")


@dataclass(frozen=True, slots=True)
class AccumulationSettings:
    """積立の設定。

    Attributes:
        monthly_budget: 毎月の基本予算（円）。入金日に全額投下する。
        tactic: 購入倍率を決める戦術。既定は完全下降配列で4倍。
    """

    monthly_budget: Decimal = Decimal(25_000)
    tactic: Tactic = field(default_factory=BearStack)

    def __post_init__(self) -> None:
        if self.monthly_budget <= 0:
            raise ValueError(f"monthly_budget は正の値: {self.monthly_budget}")

    @property
    def warmup_bars(self) -> int:
        """戦術の倍率が意味を持つまでに必要な足の本数。"""
        return self.tactic.warmup_bars


def build_plan(bars: pl.DataFrame, settings: AccumulationSettings | None = None) -> pl.DataFrame:
    """日足から日ごとの投下額を決める。

    Args:
        bars: ``date`` 昇順の日足。``date`` と ``close`` 列が必要。
        settings: 省略時は既定値（月25,000円・完全下降配列で4倍）。

    Returns:
        :data:`PLAN_COLUMNS` の列を持つ計画表。``base``/``extra``/``amount``
        は円単位の整数。

    Raises:
        ValueError: 必要な列が無い、または足が日付昇順でないとき。
    """
    settings = settings or AccumulationSettings()
    _validate(bars)

    budget = int(settings.monthly_budget)
    month = pl.col("date").dt.truncate("1mo")

    plan = bars.select("date", "close").with_columns(
        settings.tactic.multiplier(),
        # 入金日＝その月の最初の営業日。分割せず全額をここで投下する。
        month.is_first_distinct().alias("_payday"),
        # 増額分はその月の営業日数で按分する。こうすると倍率が月を通して
        # 続いた場合にちょうど (倍率-1)×予算 になり、月ごとの上限を
        # 別途設けなくても資金が発散しない。
        pl.len().over(month).alias("_days_in_month"),
    )

    plan = plan.with_columns(
        pl.when(pl.col("_payday")).then(budget).otherwise(0).cast(pl.Int64).alias("base"),
        ((pl.col(MULTIPLIER) - 1.0) * budget / pl.col("_days_in_month"))
        .floor()
        .cast(pl.Int64)
        .alias("extra"),
    )

    return plan.with_columns(
        (pl.col("base") + pl.col("extra")).alias("amount"),
        _reason().alias("reason"),
    ).select(PLAN_COLUMNS)


def _reason() -> pl.Expr:
    """なぜその金額になったかを日本語で残す。障害時の調査で効く。"""
    boosted = pl.format("{}倍で増額 {} 円", pl.col(MULTIPLIER).round(2), "extra")
    return (
        pl.when((pl.col("base") > 0) & (pl.col("extra") > 0))
        .then(pl.format("入金日 {} 円＋", "base") + boosted)
        .when(pl.col("base") > 0)
        .then(pl.format("入金日 {} 円", "base"))
        .when(pl.col("extra") > 0)
        .then(boosted)
        .otherwise(pl.lit("投下なし"))
    )


def _validate(bars: pl.DataFrame) -> None:
    missing = {"date", "close"} - set(bars.columns)
    if missing:
        raise ValueError(f"足に必要な列がありません: {sorted(missing)}")
    if bars.height and not bars["date"].is_sorted():
        raise ValueError("足は date 昇順である必要があります")
