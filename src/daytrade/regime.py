"""危険信号（局面のゲート）。毎日計算して記録し、設定で有効にしたものだけが取引を止める。

2018・2021 年の負けは「市場が寄り高・引け安を 1 年続けた」ことによる（研究ノート）。
前日までに観測できる信号は次の 4 つ。効き方が検証で違うので、**個別に有効化**する。

- 月（``skip_months``）: 12 月は 9 年中 7 年がマイナス。IS/OOS ともに改善。既定で有効
- 市場の日中ドリフト（``drift_gate``）: TOPIX の寄り→引けの 20 日平均。IS では 2018・2021 を
  黒字にするが OOS では利益を 3 割削る。既定は無効（診断値として記録）
- 資産曲線（``equity_curve_days`` / ``equity_curve_scale``）: 戦略自身の直近 20 日の実現損益が 0 以下なら
  資金を半分にする。休むのではなく縮める（MaxDD −50→−30 万、利益 −2%）。既定で有効
- IV（``iv_gate``）: 日経 225 オプションの前日 IV
- 前夜の米国（``us_skip_high``）: S&P500 が小幅高（0〜+1%）で VIX が低い翌日は、東証の
  ギャップダウンが市場全体ではなく個別のニュースによるもので、逆張りが効かない（損益 ≈ 0）。
  IS/OOS ともに改善し、閾値の端（0.8〜1.5%、VIX 20〜99）でも崩れない。既定で有効

市場のギャップ（``|中央値ギャップ| > 1%`` の日）は例外で、ドリフトが負でも取引する。
急落・急騰の寄付は逆張りが最も効く日（+1.7 万円/日）で、ゲートで外すと損をする。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field
from decimal import Decimal
from typing import Any

import polars as pl

from daytrade.config import RegimeConfig
from daytrade.universe import num
from wbcore.data.jquants_archive import Archive, endpoint


@dataclass(frozen=True, slots=True)
class Signals:
    """判定日の朝に分かる値。無いものは None（そのゲートは効かせない）。"""

    day: dt.date
    #: 前日の日経 225 オプション IV（BaseVol 中央値）
    iv_prev: float | None = None
    #: TOPIX の寄り→引けリターンの直近 N 日平均（前日まで）
    drift: float | None = None
    #: 9:00 の市場ギャップ。候補全体の中央値ギャップで代用する（TOPIX の寄付は取れない）
    market_gap: float | None = None
    #: 戦略自身の直近 N 日の損益（円、前日まで）
    recent_pnl: float | None = None
    #: 前夜の S&P500 の終値リターン
    us_ret: float | None = None
    #: 前夜の VIX 終値
    vix: float | None = None


@dataclass(frozen=True, slots=True)
class Verdict:
    trade: bool
    #: 止めた理由（複数）。取引するときは空
    reasons: list[str] = field(default_factory=list)
    #: 記録用の診断値
    notes: dict[str, float | int | None] = field(default_factory=dict)
    #: 資金に掛ける倍率（1 = そのまま）。資産曲線で縮めるときに 1 未満
    scale: float = 1.0
    #: 縮めた理由（縮めていなければ空）
    scale_reason: str = ""


def evaluate(config: RegimeConfig, s: Signals) -> Verdict:
    """設定で有効なゲートだけを見て、取引してよいか決める。"""
    reasons: list[str] = []
    if s.day.month in config.skip_months:
        reasons.append(f"{s.day.month} 月は休む")
    gate = config.iv_gate
    if gate > 0 and s.iv_prev is not None and Decimal(str(s.iv_prev)) <= gate:
        reasons.append(f"IV {s.iv_prev:.1f} ≤ {gate}")
    big_gap = s.market_gap is not None and abs(s.market_gap) > float(config.drift_gap_override)
    if (
        config.drift_gate is not None
        and s.drift is not None
        and not big_gap
        and Decimal(str(s.drift)) <= config.drift_gate
    ):
        reasons.append(
            f"市場の日中ドリフト {s.drift * 1e4:+.1f} bp ≤ {config.drift_gate * 10_000:+.0f} bp"
        )
    scale = 1.0
    scale_reason = ""
    if config.equity_curve_days > 0 and s.recent_pnl is not None and s.recent_pnl <= 0:
        text = f"直近 {config.equity_curve_days} 日の損益 {s.recent_pnl:,.0f} 円 ≤ 0"
        if config.equity_curve_scale <= 0:
            reasons.append(text)
        else:
            scale = float(config.equity_curve_scale)
            scale_reason = f"{text} → 資金を {scale:g} 倍に縮小"
    if (
        config.us_skip_high is not None
        and s.us_ret is not None
        and float(config.us_skip_low) <= s.us_ret < float(config.us_skip_high)
        and (s.vix is None or Decimal(str(s.vix)) <= config.us_vix_override)
    ):
        reasons.append(
            f"前夜の S&P500 {s.us_ret:+.2%} が小幅高（{config.us_skip_low:+.1%}〜{config.us_skip_high:+.1%}）"
            + (f"、VIX {s.vix:.1f}" if s.vix is not None else "")
        )
    notes: dict[str, float | int | None] = {
        "month": s.day.month,
        "iv_prev": s.iv_prev,
        "drift_bp": round(s.drift * 1e4, 2) if s.drift is not None else None,
        "market_gap_bp": round(s.market_gap * 1e4, 1) if s.market_gap is not None else None,
        "recent_pnl": s.recent_pnl,
        "us_ret_bp": round(s.us_ret * 1e4, 1) if s.us_ret is not None else None,
        "vix": s.vix,
        "scale": scale,
    }
    return Verdict(
        trade=not reasons, reasons=reasons, notes=notes, scale=scale, scale_reason=scale_reason
    )


# --------------------------------------------------------------------------
# 信号の計算
# --------------------------------------------------------------------------


def _f(value: Any) -> float:
    return float(value or 0.0)


def topix_drift(archive: Archive, as_of: dt.date, days: int) -> float | None:
    """TOPIX の寄り→引けリターンの ``days`` 日平均（``as_of`` まで）。足が足りなければ None。"""
    if days <= 0:
        return None
    frame = archive.read(
        endpoint("indices_bars_daily_topix"), as_of - dt.timedelta(days=days * 2 + 20), as_of
    )
    if frame.height == 0:
        return None
    series = (
        frame.select("Date", r=num("C") / num("O") - 1)
        .filter(pl.col("r").is_not_null())
        .sort("Date")
        .tail(days)
    )
    if series.height < days:
        return None
    return _f(series["r"].mean())


def topix_drift_series(archive: Archive, start: dt.date, end: dt.date, days: int) -> pl.DataFrame:
    """バックテスト用: 日付ごとの ``drift``（前日までの平均）。"""
    frame = archive.read(
        endpoint("indices_bars_daily_topix"), start - dt.timedelta(days=days * 2 + 20), end
    )
    if frame.height == 0 or days <= 0:
        return pl.DataFrame(
            {"Date": pl.Series([], dtype=pl.Date), "drift": pl.Series([], dtype=pl.Float64)}
        )
    return (
        frame.select("Date", r=num("C") / num("O") - 1)
        .sort("Date")
        .with_columns(drift=pl.col("r").rolling_mean(days, min_samples=days).shift(1))
        .select("Date", "drift")
    )


def market_gap_of(gaps: list[float]) -> float | None:
    """候補全体のギャップの中央値（9:00 の市場ギャップの代用）。"""
    if not gaps:
        return None
    return _f(pl.Series(gaps).median())
