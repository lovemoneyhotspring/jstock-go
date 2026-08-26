"""積立計画の検証。計画表と日足から取得単価と評価額を出す。

**約定のずれ**

判断は終値、約定は翌営業日の寄付。:mod:`wbjp.engine.backtest` と
同じ規律で、当日終値で約定させると取れない価格で買えることになる。
寄付が無い系列（指数や投信の基準価額）では翌日の終値で代用する。

**対照群**

平均取得単価は「買わない」ほど下がるため、単独では戦略の良し悪しを
判定できない。そこで常に「**同じ総投入額**を毎月均等に投じた場合」を
対照群として並べ、配分の効果だけを取り出す。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from decimal import Decimal

import polars as pl

from wbjp.accumulate.plan import PLAN_COLUMNS


@dataclass(frozen=True, slots=True)
class AccumulationResult:
    """積立の結果。"""

    start: dt.date
    end: dt.date
    contributed: Decimal
    """総投入額（円）。"""
    units: float
    """取得した口数。"""
    average_cost: float
    """平均取得単価。``contributed / units``。"""
    terminal_value: float
    """期末評価額。"""
    control_average_cost: float
    """対照群（同額を毎月均等）の平均取得単価。"""
    capital_multiple: float
    """基本予算だけの場合に対する総投入額の倍率。"""
    boosted_days: int
    """増額が発動した営業日数。"""

    @property
    def cost_edge(self) -> float:
        """対照群に対する平均取得単価の差。負なら安く買えた。"""
        return self.average_cost / self.control_average_cost - 1.0

    @property
    def total_return(self) -> float:
        return self.terminal_value / float(self.contributed) - 1.0

    def summary(self) -> dict[str, object]:
        return {
            "期間": f"{self.start} 〜 {self.end}",
            "総投入額": f"{self.contributed:,.0f}円",
            "資金倍率": f"{self.capital_multiple:.2f}倍",
            "平均取得単価": f"{self.average_cost:,.2f}",
            "対照群比": f"{self.cost_edge:+.2%}",
            "期末評価額": f"{self.terminal_value:,.0f}円",
            "総リターン": f"{self.total_return:+.2%}",
            "増額発動日数": self.boosted_days,
        }


def simulate(
    bars: pl.DataFrame, plan: pl.DataFrame, *, monthly_budget: Decimal
) -> AccumulationResult:
    """計画どおりに積み立てた結果を出す。

    Args:
        bars: ``date`` 昇順の日足。``close`` は必須、``open`` があれば使う。
        plan: :func:`~wbjp.accumulate.plan.build_plan` の戻り値。
        monthly_budget: 資金倍率の分母に使う基本予算。

    Raises:
        ValueError: 計画表の形が違う、または一度も投下が無いとき。
    """
    if set(PLAN_COLUMNS) - set(plan.columns):
        raise ValueError(
            f"計画表の列が不足しています: {sorted(set(PLAN_COLUMNS) - set(plan.columns))}"
        )
    if plan.height == 0:
        raise ValueError("計画表が空です")

    fills = _fill_prices(bars)
    joined = plan.join(fills, on="date", how="left").sort("date")

    amount = joined["amount"].to_numpy()
    fill = joined["fill"].to_numpy()
    base = joined["base"].to_numpy()

    invested = amount > 0
    if not invested.any():
        raise ValueError("一度も投下がありません。足の本数が warmup に足りない可能性があります")

    units = float((amount[invested] / fill[invested]).sum())
    contributed = Decimal(int(amount.sum()))
    close_last = float(joined["close"][-1])

    # 対照群: 同じ総投入額を、入金日の回数で均等に割って投じる
    paydays = base > 0
    per_payday = float(contributed) / int(paydays.sum())
    control_units = float((per_payday / fill[paydays]).sum())

    return AccumulationResult(
        start=joined["date"][0],
        end=joined["date"][-1],
        contributed=contributed,
        units=units,
        average_cost=float(contributed) / units,
        terminal_value=units * close_last,
        control_average_cost=float(contributed) / control_units,
        capital_multiple=float(contributed) / float(monthly_budget * int(paydays.sum())),
        boosted_days=int((joined["extra"].to_numpy() > 0).sum()),
    )


def _fill_prices(bars: pl.DataFrame) -> pl.DataFrame:
    """約定価格＝翌営業日の寄付（無ければ翌営業日の終値）。

    最終日は翌日が無いため当日終値で埋める。積立は売却しないので、
    最終日の端数が結果を左右することはない。
    """
    source = "open" if "open" in bars.columns else "close"
    return bars.select(
        "date",
        pl.coalesce(pl.col(source).shift(-1), pl.col("close").shift(-1), pl.col("close"))
        .cast(pl.Float64)
        .alias("fill"),
    )
