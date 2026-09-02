"""積立計画。日足と積立型戦略から「その日いくら投下するか」を決める。

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

**増額分は翌週の最初の営業日に、基本予算以上に貯まっていれば出す**

判定は日次のまま、その日ぶんの増額を積み上げておき、翌週の最初の営業日
（月曜。休場なら火曜以降）に 1 件で出す。日ごとに出すと 1 件が数千円に
なり、最低手数料（5 万円以下 55 円）で 1.5% 取られるうえ、
10 口単位の ETF では 1 単元に届かない。2000 年以降の指数で検証すると、
週でまとめても平均取得単価は日次と ±0.2% しか変わらず、手数料は半減する
（月でまとめると「月の最初」に寄った場合 +0.9% 悪化するので週が上限）。

ただし 1 週ぶんでも基本予算（月 25,000 円なら ×4 で約 18,000 円）に届かない
ことが多い。そこで**累積が基本予算以上になった週の月曜だけ**出し、届かない
週は次週へ持ち越して積み続ける。1 件あたりの金額をまとめて手数料率を下げる
ためで、投下額の総量は変わらない（出す日が後ろにずれるだけ）。
**入金日には閾値に関係なく累積を出す**——基本分の注文がどのみち出るので、
同じ注文に乗せても手数料は増えず、持ち越しが 1 か月より長くならない。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from decimal import Decimal

import polars as pl

from accum.tactics import MULTIPLIER, BearStack, Tactic

#: 計画表の列。
PLAN_COLUMNS = ("date", "close", MULTIPLIER, "base", "extra", "amount", "reason")


@dataclass(frozen=True, slots=True)
class AccumulationSettings:
    """積立の設定。

    Attributes:
        monthly_budget: 毎月の基本予算（円）。入金日に全額投下する。
        tactic: 購入倍率を決める積立型戦略。既定は完全下降配列で4倍。
    """

    monthly_budget: Decimal = Decimal(25_000)
    tactic: Tactic = field(default_factory=BearStack)

    def __post_init__(self) -> None:
        if self.monthly_budget <= 0:
            raise ValueError(f"monthly_budget は正の値: {self.monthly_budget}")

    @property
    def warmup_bars(self) -> int:
        """戦略の倍率が意味を持つまでに必要な足の本数。"""
        return self.tactic.warmup_bars


def build_plan(
    bars: pl.DataFrame,
    settings: AccumulationSettings | None = None,
    *,
    signal_bars: pl.DataFrame | None = None,
    signal_strict: bool = False,
) -> pl.DataFrame:
    """日足から日ごとの投下額を決める。

    Args:
        bars: 買う銘柄の ``date`` 昇順の日足。``date`` と ``close`` 列が必要。
            入金日（月初の営業日）と株数の計算はこちらの足で決まる。
        settings: 省略時は既定値（月25,000円・完全下降配列で4倍）。
        signal_bars: 倍率の判定に使う別銘柄の日足。省略すれば ``bars`` で判定する。
            暦が違っても（東証の ETF を米国指数で判定する等）動くように、
            買う銘柄の各日に対して「その日以前で最新」の判定値を使う。
            判定値がまだ無い日は 1 倍（増額なし）。
        signal_strict: True なら「その日より**前**」の判定値だけを使う。判定用の
            市場の引けが買う銘柄の判断時刻より後（東証の銘柄を米国指数で判定）の
            とき、同じ日付の足はまだ存在しないのでこれを立てる。立て忘れると
            バックテストだけが翌朝の足で判定し、ライブと食い違う。

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

    if signal_bars is None:
        priced = bars.select("date", "close").with_columns(settings.tactic.multiplier())
    else:
        _validate(signal_bars)
        multipliers = signal_bars.select("date", "close").with_columns(settings.tactic.multiplier())
        priced = (
            bars.select("date", "close")
            .join_asof(
                multipliers.select("date", MULTIPLIER),
                on="date",
                strategy="backward",
                allow_exact_matches=not signal_strict,
            )
            .with_columns(pl.col(MULTIPLIER).fill_null(1.0))
        )

    plan = priced.with_columns(
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
        .alias("_accrued"),
    )
    plan = _defer_extras_to_next_week(plan, threshold=budget)

    return plan.with_columns(
        (pl.col("base") + pl.col("extra")).alias("amount"),
        _reason().alias("reason"),
    ).select(PLAN_COLUMNS)


def _defer_extras_to_next_week(plan: pl.DataFrame, *, threshold: int) -> pl.DataFrame:
    """日ごとに積み上げた増額（``_accrued``）を、翌週の最初の営業日に 1 件でまとめる。

    週は月曜始まり。翌週の月曜が休場なら、その週で最初に足のある日に出す。
    その日の累積が ``threshold``（基本予算）に届かなければ出さず、次の週へ
    持ち越して積み続ける。届いた週にまとめて出す。入金日（``_payday``）は
    基本分の注文が出るので、累積があれば閾値に関係なく同じ日に出す。
    足の最終週に積み上がった分や、まだ閾値に届いていない分は計画には現れない
    （ライブでは足が増えて届いた時点で目標に入る）。
    """
    next_week = (pl.col("date").dt.truncate("1w") + pl.duration(weeks=1)).alias("_next_week")
    accrued = (
        plan.filter(pl.col("_accrued") > 0)
        .select(next_week, "_accrued")
        .group_by("_next_week")
        .agg(pl.col("_accrued").sum().alias("extra"), pl.len().alias("_extra_days"))
        .sort("_next_week")
    )
    sessions = plan.select(pl.col("date").alias("_session")).sort("_session")
    scheduled = (
        accrued.join_asof(sessions, left_on="_next_week", right_on="_session", strategy="forward")
        .drop_nulls("_session")
        .group_by("_session")
        .agg(pl.col("extra").sum(), pl.col("_extra_days").sum())
        .rename({"_session": "date"})
        .sort("date")
    )
    paydays = plan.filter(pl.col("_payday"))["date"]
    scheduled = _release_when_reaching(scheduled, threshold, paydays=paydays)
    return plan.join(scheduled, on="date", how="left").with_columns(
        pl.col("extra").fill_null(0).cast(pl.Int64),
        pl.col("_extra_days").fill_null(0).cast(pl.Int64),
    )


def _release_when_reaching(
    scheduled: pl.DataFrame, threshold: int, *, paydays: pl.Series
) -> pl.DataFrame:
    """週ごとの増額を、累積が ``threshold`` 以上になった週か入金日にまとめて出す。

    候補日（翌週の最初の営業日と入金日）を古い順にたどり、累積が閾値に届いた
    日か入金日に全額出して 0 に戻す。候補日は年に 60 ほどなので Python の
    ループで十分。
    """
    payday_set = set(paydays.to_list())
    candidates = (
        pl.concat(
            [
                scheduled.select(
                    "date", pl.col("extra").cast(pl.Int64), pl.col("_extra_days").cast(pl.Int64)
                ),
                pl.DataFrame(
                    {"date": paydays, "extra": 0, "_extra_days": 0},
                    schema={"date": pl.Date, "extra": pl.Int64, "_extra_days": pl.Int64},
                ),
            ]
        )
        .group_by("date")
        .agg(pl.col("extra").sum(), pl.col("_extra_days").sum())
        .sort("date")
    )
    dates, extras, days = [], [], []
    carry_amount = carry_days = 0
    for row in candidates.iter_rows(named=True):
        carry_amount += int(row["extra"])
        carry_days += int(row["_extra_days"])
        if carry_amount <= 0:
            continue
        if carry_amount < threshold and row["date"] not in payday_set:
            continue
        dates.append(row["date"])
        extras.append(carry_amount)
        days.append(carry_days)
        carry_amount = carry_days = 0
    return pl.DataFrame(
        {"date": dates, "extra": extras, "_extra_days": days},
        schema={"date": pl.Date, "extra": pl.Int64, "_extra_days": pl.Int64},
    )


def _reason() -> pl.Expr:
    """なぜその金額になったかを日本語で残す。障害時の調査で効く。"""
    boosted = pl.format("累積の増額 {} 円（下降 {} 日ぶん）", "extra", "_extra_days")
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
