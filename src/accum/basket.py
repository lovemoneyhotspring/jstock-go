"""複数銘柄への配分（バスケット）。「その月の予算をどの銘柄にいくら振るか」を決める。

**なぜ積立型戦略（Tactic）と分けるのか**

:mod:`accum.tactics` は1銘柄の足だけを見て「今日は何倍買うか」を
返す。銘柄をまたぐ判断（バフェットの保有比率で買う、コアとサテライトに
分ける）はそこに入れられない。そこで **配分は銘柄をまたぐ層** として
独立させ、倍率は従来どおり銘柄ごとの積立型戦略に任せる。

    予算 × 配分比率（この層） × 倍率（戦略） = その日その銘柄の投下額

**配分は時間で変わる**

13F 追従では四半期ごとに比率が変わる。:class:`WeightSchedule` は
「この日から有効な比率表」の列で、判定日に有効な最新の表を引く。
提出日 **当日** の表は使わない（引けた後に公開されることが多いため、
翌営業日から）。

**足が無い銘柄の扱い**

上場前・上場廃止後・足の未取得で、その日に足が無い銘柄には振れない。
その分は現金として残さず、足のある銘柄の間で比率を正規化して投じる。
現金の滞留は平均取得単価を確実に悪化させる（:mod:`~accum.plan`）。

**評価の物差し**

投下額が時期ごとに違うので、単純な総リターンでは比較にならない。
:class:`BasketResult` は内部収益率（XIRR）と、時間加重の評価額指数から
求めた最大ドローダウンを持ち、同じ資金の流れを基準銘柄（VOO 等）に
投じた場合と並べる。
"""

from __future__ import annotations

import datetime as dt
from collections.abc import Iterable, Mapping
from dataclasses import dataclass, field
from decimal import Decimal

import numpy as np
import polars as pl

from accum.plan import PLAN_COLUMNS, AccumulationSettings, build_plan
from accum.tactics import MULTIPLIER, Constant, Tactic


@dataclass(frozen=True, slots=True)
class WeightSchedule:
    """「この日から有効」な比率表の列。

    Attributes:
        entries: ``(有効開始日, {銘柄: 比率})`` を日付昇順で。比率は正で、
            合計は 1 でなくてよい（引くときに正規化する）。
    """

    entries: tuple[tuple[dt.date, dict[str, float]], ...]

    @classmethod
    def static(cls, weights: Mapping[str, float]) -> WeightSchedule:
        """時間で変わらない配分。"""
        return cls(((dt.date.min, dict(weights)),))

    @classmethod
    def from_pairs(cls, pairs: Iterable[tuple[dt.date, Mapping[str, float]]]) -> WeightSchedule:
        entries = tuple(sorted(((d, dict(w)) for d, w in pairs), key=lambda e: e[0]))
        for _, weights in entries:
            for symbol, value in weights.items():
                if value < 0:
                    raise ValueError(f"比率は負にできません: {symbol}={value}")
        return cls(entries)

    def at(self, date: dt.date) -> dict[str, float]:
        """``date`` に有効な比率。開始日と同じ日は **まだ** 前の表を使う。"""
        current: dict[str, float] = {}
        for effective, weights in self.entries:
            if effective < date:
                current = weights
            else:
                break
        return current

    @property
    def symbols(self) -> list[str]:
        """一度でも登場する銘柄。足の取得対象になる。"""
        return sorted({s for _, w in self.entries for s in w})

    def blend(self, core: Mapping[str, float], satellite_share: float) -> WeightSchedule:
        """固定のコアと組み合わせる。

        ``core`` を ``1 - satellite_share`` で、この表を ``satellite_share`` で
        持つ。コア・サテライト（VOO 70% ＋ バフェット銘柄 30%）を作る。
        """
        if not 0 < satellite_share <= 1:
            raise ValueError(f"satellite_share は 0 より大きく 1 以下: {satellite_share}")
        core_total = sum(core.values())
        if core and core_total <= 0:
            raise ValueError("core の比率の合計が正ではありません")
        scaled_core = {s: v / core_total * (1 - satellite_share) for s, v in core.items()}
        entries = []
        for effective, weights in self.entries:
            total = sum(weights.values())
            merged = dict(scaled_core)
            for symbol, value in weights.items():
                merged[symbol] = merged.get(symbol, 0.0) + value / total * satellite_share
            entries.append((effective, merged))
        return WeightSchedule(tuple(entries))


@dataclass(frozen=True, slots=True)
class DrawdownTilt:
    """構成銘柄のうち「高値から深く下げているもの」へ配分を寄せる。

    バフェットの「良い会社を安いときに」を価格だけで近似したもの。
    財務指標（PER・FCF 利回り）は過去時点の値が取りにくくルックアヘッドを
    避けにくいので、まず価格ベースで効くかを確かめる。

    各銘柄の比率に ``1 + strength × 下落率`` を掛けてから正規化する。
    下落率は ``lookback`` 本の高値に対する割合（0〜1）。
    ``strength=2`` なら 30% 下げた銘柄の比率が 1.6 倍になる。

    積立型戦略（:class:`~accum.tactics.Tactic`）と違い予算の総額は
    変えない。銘柄 **間** で配分を動かすだけなので追加資金は要らない。
    """

    strength: float = 2.0
    lookback: int = 252

    def __post_init__(self) -> None:
        if self.strength <= 0:
            raise ValueError(f"strength は正の値: {self.strength}")
        if self.lookback < 2:
            raise ValueError(f"lookback は 2 以上: {self.lookback}")

    def factor(self, frame: pl.DataFrame) -> pl.Series:
        """日ごとの係数（1 以上）。高値未確定の期間は 1。"""
        close = pl.col("close").cast(pl.Float64)
        peak = close.rolling_max(self.lookback, min_samples=1)
        drawdown = (1.0 - close / peak).clip(0.0, 1.0)
        return frame.select((1.0 + self.strength * drawdown).alias("f"))["f"]


@dataclass(frozen=True, slots=True)
class BasketSettings:
    """バスケット全体の設定。

    Attributes:
        monthly_budget: バスケット **全体** の毎月の基本予算（円）。
        schedule: 配分。
        tactic: 各銘柄に掛ける倍率戦略。既定は定額。
        tilt: 銘柄間で配分を傾ける規則。None なら配分表どおり。
    """

    monthly_budget: Decimal
    schedule: WeightSchedule
    tactic: Tactic = field(default_factory=Constant)
    tilt: DrawdownTilt | None = None

    def __post_init__(self) -> None:
        if self.monthly_budget <= 0:
            raise ValueError(f"monthly_budget は正の値: {self.monthly_budget}")


def build_basket_plan(
    bars: Mapping[str, pl.DataFrame], settings: BasketSettings
) -> dict[str, pl.DataFrame]:
    """銘柄ごとの計画表を作る。

    銘柄単位の :func:`~accum.plan.build_plan` をバスケットの予算で
    走らせ、日ごとの配分比率を掛ける。比率はその日に足のある銘柄の間で
    正規化する。

    Returns:
        ``銘柄 → 計画表``。計画表は :data:`~accum.plan.PLAN_COLUMNS`
        に ``weight`` 列を加えたもの。配分が 0 の銘柄は含めない。
    """
    if not bars:
        raise ValueError("足がありません")
    available = {s: set(f["date"].to_list()) for s, f in bars.items()}
    dates = sorted(set().union(*available.values()))

    # 傾斜の係数（日付 → 係数）。傾斜が無ければ空のまま
    tilt_factor: dict[str, dict[dt.date, float]] = {}
    if settings.tilt is not None:
        for symbol, frame in bars.items():
            factors = settings.tilt.factor(frame).to_list()
            tilt_factor[symbol] = dict(zip(frame["date"].to_list(), factors, strict=True))

    # 日 × 銘柄の比率表。判定は日ごとだが、比率表が切り替わる日は年に数回
    # しか無いので、傾斜が無く前日と同じ表なら計算を使い回す。
    weight_rows: dict[str, list[float]] = {s: [] for s in bars}
    last_key: tuple[int, frozenset[str]] | None = None
    normalized: dict[str, float] = {}
    for date in dates:
        raw = settings.schedule.at(date)
        present = frozenset(s for s in raw if s in available and date in available[s])
        key = (id(raw), present)
        if key != last_key or tilt_factor:
            tilted = {s: raw[s] * tilt_factor.get(s, {}).get(date, 1.0) for s in present}
            total = sum(tilted.values())
            normalized = {s: v / total for s, v in tilted.items()} if total > 0 else {}
            last_key = key
        for symbol in bars:
            weight_rows[symbol].append(normalized.get(symbol, 0.0))

    # 入金日はバスケット共通の暦（全銘柄の営業日の和集合）で決める。
    # 銘柄ごとに決めると、途中上場した銘柄が上場初日に「その月の入金日」を
    # 持ってしまい、その月だけ予算が二重になる。
    calendar = pl.DataFrame({"date": dates}).with_columns(
        pl.col("date").dt.truncate("1mo").is_first_distinct().alias("_payday")
    )
    budget = int(settings.monthly_budget)
    date_index = {d: i for i, d in enumerate(dates)}
    plans: dict[str, pl.DataFrame] = {}
    for symbol, frame in bars.items():
        weights = np.asarray(weight_rows[symbol])
        rows = [date_index[d] for d in frame["date"].to_list()]
        per_symbol = weights[rows]
        if not per_symbol.any():
            continue
        base_plan = build_plan(
            frame, AccumulationSettings(settings.monthly_budget, settings.tactic)
        ).join(calendar, on="date", how="left")
        scaled = base_plan.with_columns(pl.Series("weight", per_symbol)).with_columns(
            (pl.when(pl.col("_payday")).then(budget).otherwise(0) * pl.col("weight"))
            .floor()
            .cast(pl.Int64)
            .alias("base"),
            (pl.col("extra") * pl.col("weight")).floor().cast(pl.Int64).alias("extra"),
        )
        plans[symbol] = scaled.with_columns(
            (pl.col("base") + pl.col("extra")).alias("amount")
        ).select([*PLAN_COLUMNS, "weight"])
    return plans


# -- 検証 -----------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class Leg:
    """1本の資金の流れの結果（バスケット本体、または基準銘柄）。"""

    contributed: Decimal
    terminal_value: float
    xirr: float
    max_drawdown: float
    """時間加重の評価額指数の最大下落率（0〜1）。"""

    @property
    def total_return(self) -> float:
        return self.terminal_value / float(self.contributed) - 1.0


@dataclass(frozen=True, slots=True)
class BasketResult:
    start: dt.date
    end: dt.date
    basket: Leg
    benchmark: Leg | None
    symbols: list[str]
    """一度でも投下した銘柄。"""
    boosted_days: int

    def summary(self) -> dict[str, object]:
        out: dict[str, object] = {
            "期間": f"{self.start} 〜 {self.end}",
            "総投入額": f"{self.basket.contributed:,.0f}円",
            "期末評価額": f"{self.basket.terminal_value:,.0f}円",
            "総リターン": f"{self.basket.total_return:+.2%}",
            "XIRR": f"{self.basket.xirr:+.2%}",
            "最大DD": f"{self.basket.max_drawdown:.1%}",
            "銘柄数": len(self.symbols),
            "増額発動日数": self.boosted_days,
        }
        if self.benchmark:
            out["基準 期末評価額"] = f"{self.benchmark.terminal_value:,.0f}円"
            out["基準 XIRR"] = f"{self.benchmark.xirr:+.2%}"
            out["基準 最大DD"] = f"{self.benchmark.max_drawdown:.1%}"
        return out


def _fill(frame: pl.DataFrame) -> pl.DataFrame:
    """約定価格＝翌営業日の寄付（無ければ翌日終値、最終日は当日終値）。"""
    source = "open" if "open" in frame.columns else "close"
    return frame.select(
        "date",
        pl.col("close").cast(pl.Float64),
        pl.coalesce(pl.col(source).shift(-1), pl.col("close").shift(-1), pl.col("close"))
        .cast(pl.Float64)
        .alias("fill"),
    )


def _leg(
    dates: list[dt.date],
    cashflows: np.ndarray,
    holdings: pl.DataFrame,
) -> Leg:
    """評価額の推移から内部収益率と最大DDを出す。

    Args:
        dates: 営業日の列（昇順）。
        cashflows: ``dates`` に対応する投下額（円）。
        holdings: ``date``, ``value`` 列。その日の引け時点の評価額。
    """
    values = (
        pl.DataFrame({"date": dates})
        .join(holdings, on="date", how="left")
        .sort("date")["value"]
        .fill_null(0.0)
        .to_numpy()
    )
    contributed = Decimal(int(cashflows.sum()))
    terminal = float(values[-1])
    # 時間加重指数: 当日の投下を除いた評価額の変化率をつなぐ。
    # 投下は翌営業日の寄付で約定するので、値上がり率は
    # (今日の評価額) / (昨日の評価額 + 昨日の投下額) で近似する。
    prev = np.concatenate([[0.0], values[:-1]]) + np.concatenate([[0.0], cashflows[:-1]])
    ratio = np.where(prev > 0, values / np.where(prev > 0, prev, 1.0), 1.0)
    index = np.cumprod(ratio)
    drawdown = 1.0 - index / np.maximum.accumulate(index)
    return Leg(
        contributed=contributed,
        terminal_value=terminal,
        xirr=xirr(dates, -cashflows, terminal),
        max_drawdown=float(drawdown.max()) if len(drawdown) else 0.0,
    )


def xirr(dates: list[dt.date], flows: np.ndarray, terminal: float) -> float:
    """不定期キャッシュフローの内部収益率（年率）。

    ``flows`` は投下を負で。最終日に ``terminal`` を正で加えて解く。
    二分法で解くので初期値に依存せず、解が無いときは 0 を返す。
    """
    t = np.array([(d - dates[0]).days for d in dates], dtype=float) / 365.0
    cf = np.asarray(flows, dtype=float).copy()
    cf[-1] += terminal
    if not (cf < 0).any() or not (cf > 0).any():
        return 0.0

    def npv(rate: float) -> float:
        return float((cf / (1.0 + rate) ** t).sum())

    # 打ち切りは金額に対する相対誤差で。絶対値だと、割引が効く長期の
    # 系列で NPV が最初から小さく、粗い解で止まってしまう。
    tolerance = 1e-9 * float(np.abs(cf).sum())
    lo, hi = -0.99, 10.0
    f_lo, f_hi = npv(lo), npv(hi)
    if f_lo * f_hi > 0:
        return 0.0
    for _ in range(200):
        mid = (lo + hi) / 2
        f_mid = npv(mid)
        if abs(f_mid) < tolerance:
            break
        if f_lo * f_mid < 0:
            hi, f_hi = mid, f_mid
        else:
            lo, f_lo = mid, f_mid
    return (lo + hi) / 2


def simulate_basket(
    bars: Mapping[str, pl.DataFrame],
    plans: Mapping[str, pl.DataFrame],
    *,
    benchmark: pl.DataFrame | None = None,
) -> BasketResult:
    """計画どおりに買った結果。基準銘柄には同じ日に同じ額を投じる。"""
    if not plans:
        raise ValueError("計画表が空です（配分が 0 か、足がありません）")

    # 銘柄ごとに口数を積み上げ、日ごとの評価額を出す
    values: list[pl.DataFrame] = []
    flows: list[pl.DataFrame] = []
    boosted = 0
    for symbol, plan in plans.items():
        joined = plan.join(_fill(bars[symbol]), on="date", how="left", suffix="_bar").sort("date")
        units = (joined["amount"] / joined["fill"]).fill_nan(0.0).cum_sum()
        # 投下日の翌営業日に約定するので、評価は翌日から。当日は前日までの口数
        held = units.shift(1).fill_null(0.0)
        values.append(joined.select("date", (held * joined["close"]).alias("value")))
        flows.append(joined.select("date", "amount"))
        boosted += int((joined["extra"].to_numpy() > 0).sum())

    value_by_date = pl.concat(values).group_by("date").agg(pl.col("value").sum()).sort("date")
    flow_by_date = pl.concat(flows).group_by("date").agg(pl.col("amount").sum()).sort("date")
    # 配分が始まる前（13F の最初の提出前など）の日は結果に含めない。
    # 起点が何年も前にあると XIRR の割引が効きすぎて解が不安定になる。
    first_flow = flow_by_date.filter(pl.col("amount") > 0)["date"].min()
    if first_flow is None:
        raise ValueError("一度も投下がありません")
    value_by_date = value_by_date.filter(pl.col("date") >= first_flow)
    flow_by_date = flow_by_date.filter(pl.col("date") >= first_flow)
    dates = flow_by_date["date"].to_list()
    cashflows = flow_by_date["amount"].cast(pl.Float64).to_numpy()

    # 最終日の評価は当日引けの口数で（未約定分は当日終値で買えたとみなす）
    last = value_by_date["date"].max()
    terminal = 0.0
    for symbol, plan in plans.items():
        joined = plan.join(_fill(bars[symbol]), on="date", how="left")
        units_total = float((joined["amount"] / joined["fill"]).fill_nan(0.0).sum())
        terminal += units_total * float(bars[symbol].filter(pl.col("date") <= last)["close"][-1])
    value_by_date = value_by_date.with_columns(
        pl.when(pl.col("date") == last).then(terminal).otherwise(pl.col("value")).alias("value")
    )
    basket = _leg(dates, cashflows, value_by_date)

    bench_leg = None
    if benchmark is not None:
        b = (
            _fill(benchmark)
            .filter(pl.col("date") >= first_flow)
            .join(flow_by_date, on="date", how="left")
            .sort("date")
        )
        b = b.with_columns(pl.col("amount").fill_null(0).cast(pl.Float64))
        b_units = (b["amount"] / b["fill"]).cum_sum()
        b_values = b.select("date", (b_units.shift(1).fill_null(0.0) * b["close"]).alias("value"))
        b_terminal = float(b_units[-1]) * float(b["close"][-1])
        b_values = b_values.with_columns(
            pl.when(pl.col("date") == b_values["date"].max())
            .then(b_terminal)
            .otherwise(pl.col("value"))
            .alias("value")
        )
        b_flows = b.select("date", "amount")
        b_dates = b_flows["date"].to_list()
        bench_leg = _leg(b_dates, b_flows["amount"].to_numpy(), b_values)

    return BasketResult(
        start=dates[0],
        end=dates[-1],
        basket=basket,
        benchmark=bench_leg,
        symbols=sorted(s for s, p in plans.items() if p["amount"].sum() > 0),
        boosted_days=boosted,
    )


__all__ = [
    "MULTIPLIER",
    "BasketResult",
    "BasketSettings",
    "DrawdownTilt",
    "Leg",
    "WeightSchedule",
    "build_basket_plan",
    "simulate_basket",
    "xirr",
]
