"""上昇トレンド中の RSI(3) 押し目買い（勝率重視のスイング）。

`trend_pullback` の元版（コミット 165931f）を復元したもの。現行の `trend_pullback` は
出来高枯れ＋ブレイクアウト型に書き換えられたため、勝率 90% だった元の入口を
別名で残す。出口の検証用に、戦略側の手仕舞い（RSI 買われすぎ・高値到達・SMA 回復）は
それぞれ個別に無効化できる。

考え方:
    「長期上昇トレンドが確認済みの銘柄が、短期的に売られた所を拾い、
    平均回帰で小さく取る」。ブレイクアウトより勝率が高く、利幅は小さい。

エントリー（全条件 AND。1つでも欠ければ意見を出さない）:
    1. 終値 > SMA200 かつ SMA50 > SMA200            … 長期上昇トレンド
    2. SMA50 が上向き（10日前より高い）              … トレンドが生きている
    3. 直近 ``high_lookback`` 日高値からの下落率 ≦ 上限 … 押し目であって崩れではない
    4. RSI(3) < 閾値                                  … 短期の売られすぎ
    5. 反転確認: 当日が陽線（終値 > 前日終値）          … 落ちるナイフを受けない
       反転確認を使うときは、RSI は**前日**の値で判定する。反発した当日の
       RSI(3) は必ず跳ね上がるため、当日の値で見ると条件が両立しない。
    6. ATR/終値 が下限〜上限                          … 動かない株・荒れ過ぎの株を除外
    7. 20日平均売買代金 ≧ 下限                        … 指値が乗る流動性
    8. 地合い: ベンチマーク（SPY）の終値 > その SMA     … 市場全体の下げ局面では建てない
    9. 決算ブラックアウト外                            … 日足ではギャップを避けられない

順位付け:
    条件を満たした銘柄には 0〜1 のスコアを付け、``direction`` に写す
    （0.3〜1.0）。サイジングは direction の高い順に ``max_positions`` の
    枠を埋めるので、**スコアがそのまま採用順位になる**。

    スコアの内訳（meta に残す）:
        - dip     … RSI がどれだけ深いか。押し目の深さ
        - stretch … SMA20 からの乖離を ATR で割った値。平均回帰の余地
        - trend   … SMA200 からの距離。トレンドの強さ
        - liquid  … 売買代金（対数）。滑りにくさ
    重みは押し目の質（dip / stretch）を重く見る。勝率に最も効くのが
    「どれだけ売られた所を拾えたか」だから。

手仕舞い（保有中に direction = -1 を出す）:
    - RSI(3) が買われすぎ圏（≧ ``rsi_exit``）
    - 含み益がある状態で終値が SMA20 を回復した（第一目標）
    - 終値が直近高値に達した（上限目標）
    - 決算ブラックアウトに入った
    損切り・時間切れ・トレーリングは :mod:`wbjp.risk.stops` が担当する。
"""

from __future__ import annotations

import datetime as dt
import math
import tomllib
from pathlib import Path
from typing import Any, ClassVar

import polars as pl

from wbcore.domain.models import Signal
from wbcore.indicators.ohlcv import atr, rsi, sma
from wbjp.strategy.base import IndicatorStrategy, StrategyContext

#: 意見を出すときの direction の下限。entry_threshold（既定 0.3）以上。
_DIRECTION_FLOOR = 0.3


class RsiPullbackStrategy(IndicatorStrategy):
    name: ClassVar[str] = "rsi_pullback"

    def __init__(
        self,
        sma_long: int = 200,
        sma_mid: int = 50,
        sma_short: int = 20,
        slope_lookback: int = 10,
        rsi_period: int = 3,
        rsi_entry: float = 20.0,
        rsi_exit: float = 80.0,
        high_lookback: int = 60,
        max_drawdown_from_high: float = 0.15,
        atr_period: int = 14,
        min_atr_ratio: float = 0.015,
        max_atr_ratio: float = 0.05,
        min_dollar_volume: float = 5_000_000.0,
        volume_lookback: int = 20,
        require_reversal_bar: bool = True,
        benchmark: str | None = "SPY",
        benchmark_sma: int = 50,
        blackout_file: str | None = None,
        blackout_days_before: int = 3,
        exit_before_earnings: bool = True,
        exit_on_rsi: bool = True,
        exit_on_high: bool = True,
        exit_on_sma_recovery: bool = True,
    ) -> None:
        if not sma_short < sma_mid < sma_long:
            raise ValueError("sma_short < sma_mid < sma_long を満たすこと")
        if not 0 < rsi_entry < rsi_exit < 100:
            raise ValueError("0 < rsi_entry < rsi_exit < 100 を満たすこと")
        if not 0 < min_atr_ratio < max_atr_ratio:
            raise ValueError("0 < min_atr_ratio < max_atr_ratio を満たすこと")

        self.sma_long = sma_long
        self.sma_mid = sma_mid
        self.sma_short = sma_short
        self.slope_lookback = slope_lookback
        self.rsi_period = rsi_period
        self.rsi_entry = rsi_entry
        self.rsi_exit = rsi_exit
        self.high_lookback = high_lookback
        self.max_drawdown_from_high = max_drawdown_from_high
        self.atr_period = atr_period
        self.min_atr_ratio = min_atr_ratio
        self.max_atr_ratio = max_atr_ratio
        self.min_dollar_volume = min_dollar_volume
        self.volume_lookback = volume_lookback
        self.require_reversal_bar = require_reversal_bar
        self.benchmark = benchmark
        self.benchmark_sma = benchmark_sma
        self.blackout_days_before = blackout_days_before
        self.exit_before_earnings = exit_before_earnings
        self.exit_on_rsi = exit_on_rsi
        self.exit_on_high = exit_on_high
        self.exit_on_sma_recovery = exit_on_sma_recovery
        self.blackout: dict[str, list[dt.date]] = (
            load_blackout(Path(blackout_file)) if blackout_file else {}
        )
        self.warmup_bars = max(sma_long, high_lookback, benchmark_sma) + slope_lookback + 1

    # -- 指標 ---------------------------------------------------------------

    def indicators(self) -> list[pl.Expr]:
        return [
            sma(self.sma_long),
            sma(self.sma_mid),
            sma(self.sma_short),
            rsi(self.rsi_period),
            atr(self.atr_period),
            pl.col("high").rolling_max(self.high_lookback).alias(self._high_col),
            (pl.col("close") * pl.col("volume"))
            .rolling_mean(self.volume_lookback)
            .alias(self._dollar_volume_col),
            pl.col("close").shift(1).alias("prev_close"),
            rsi(self.rsi_period).shift(1).alias(f"prev_rsi_{self.rsi_period}"),
        ]

    @property
    def _high_col(self) -> str:
        return f"high_{self.high_lookback}"

    @property
    def _dollar_volume_col(self) -> str:
        return f"dollar_volume_{self.volume_lookback}"

    # -- 判断 ---------------------------------------------------------------

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        self._market_ok = self._benchmark_allows_entry(ctx)
        return super().on_bars(ctx)

    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        if symbol == self.benchmark:
            return None  # ベンチマークは売買対象にしない

        latest = frame.row(-1, named=True)
        close = latest["close"]
        rsi_value = latest[f"rsi_{self.rsi_period}"]
        sma_short = latest[f"sma_{self.sma_short}"]
        sma_long = latest[f"sma_{self.sma_long}"]
        high = latest[self._high_col]
        if None in (close, rsi_value, sma_short, sma_long, high):
            return None

        in_blackout = self._in_blackout(symbol, ctx.as_of)
        position = ctx.position(symbol)
        held = position is not None and position.quantity > 0

        if held:
            return self._evaluate_exit(symbol, latest, position, in_blackout)

        if in_blackout:
            return None
        return self._evaluate_entry(symbol, frame, latest)

    def _evaluate_entry(
        self, symbol: str, frame: pl.DataFrame, latest: dict[str, Any]
    ) -> Signal | None:
        checks = self.screen(frame)
        if not checks.passed:
            return None

        score, parts = self._score(latest, checks)
        direction = _DIRECTION_FLOOR + (1.0 - _DIRECTION_FLOOR) * score
        return Signal(
            self.name,
            symbol,
            direction=round(direction, 4),
            confidence=1.0,
            reason=(
                f"押し目: RSI{self.rsi_period} {checks.rsi:.1f}, 高値から {checks.drawdown:.1%}, "
                f"ATR {checks.atr_ratio:.1%}, スコア {score:.2f}"
            ),
            meta={"score": score, **parts, **checks.as_meta()},
        )

    def _evaluate_exit(
        self, symbol: str, latest: dict[str, Any], position: Any, in_blackout: bool
    ) -> Signal | None:
        close = latest["close"]
        rsi_value = latest[f"rsi_{self.rsi_period}"]
        sma_short = latest[f"sma_{self.sma_short}"]
        high = latest[self._high_col]
        cost = float(position.cost_price)

        reason = None
        if in_blackout and self.exit_before_earnings:
            reason = "決算前のため手仕舞い"
        elif self.exit_on_rsi and rsi_value >= self.rsi_exit:
            reason = f"RSI{self.rsi_period} {rsi_value:.1f} が買われすぎ圏（≧{self.rsi_exit:g}）"
        elif self.exit_on_high and close >= high:
            reason = f"直近 {self.high_lookback} 日高値に到達"
        elif self.exit_on_sma_recovery and close > sma_short and close > cost:
            reason = f"含み益で SMA{self.sma_short} を回復（第一目標）"

        if reason is None:
            # 保有継続。意見が無いと sizer が「シグナル消滅」で手仕舞うため、
            # 明示的に弱い買いを返す。
            return Signal(self.name, symbol, direction=0.5, reason="保有継続（押し目の回復待ち）")
        return Signal(self.name, symbol, direction=-1.0, reason=reason, meta={"rsi": rsi_value})

    # -- スクリーニング（screen コマンドからも使う） ------------------------

    def screen(self, frame: pl.DataFrame) -> ScreenResult:
        """1銘柄の足に対して、エントリー条件を1つずつ評価する。

        ``passed`` が False でも各条件の値を返すので、「なぜ落ちたか」を
        表示できる。
        """
        latest = frame.row(-1, named=True)
        close = latest["close"]
        sma_long = latest[f"sma_{self.sma_long}"]
        sma_mid = latest[f"sma_{self.sma_mid}"]
        sma_short = latest[f"sma_{self.sma_short}"]
        rsi_value = latest[f"rsi_{self.rsi_period}"]
        atr_value = latest[f"atr_{self.atr_period}"]
        high = latest[self._high_col]
        dollar_volume = latest[self._dollar_volume_col]
        prev_close = latest["prev_close"]
        # 反転確認を使うなら「前日に売られすぎていた」ことを見る
        if self.require_reversal_bar:
            rsi_value = latest[f"prev_rsi_{self.rsi_period}"]

        older_mid = (
            frame[f"sma_{self.sma_mid}"][-1 - self.slope_lookback]
            if frame.height > self.slope_lookback
            else None
        )

        values = (close, sma_long, sma_mid, sma_short, rsi_value, atr_value, high, dollar_volume)
        if any(v is None for v in values):
            return ScreenResult(failed=["指標が未計算"], close=close or 0.0)

        drawdown = 1.0 - close / high if high else 1.0
        atr_ratio = atr_value / close if close else 0.0

        failed = []
        if not (close > sma_long and sma_mid > sma_long):
            failed.append(f"トレンド: 終値 > SMA{self.sma_long} > … を満たさない")
        if older_mid is not None and sma_mid <= older_mid:
            failed.append(f"SMA{self.sma_mid} が下向き")
        if drawdown > self.max_drawdown_from_high:
            failed.append(f"高値から {drawdown:.1%} 下落（上限 {self.max_drawdown_from_high:.0%}）")
        if rsi_value >= self.rsi_entry:
            failed.append(f"RSI{self.rsi_period} {rsi_value:.1f} ≧ {self.rsi_entry:g}")
        if self.require_reversal_bar and (prev_close is None or close <= prev_close):
            failed.append("反転確認なし（当日が陽線でない）")
        if not (self.min_atr_ratio <= atr_ratio <= self.max_atr_ratio):
            failed.append(f"ATR比 {atr_ratio:.1%} が範囲外")
        if dollar_volume < self.min_dollar_volume:
            failed.append(f"売買代金 {dollar_volume:,.0f} が下限未満")
        if not self._market_ok:
            failed.append(f"地合い: {self.benchmark} が SMA{self.benchmark_sma} 割れ")

        return ScreenResult(
            failed=failed,
            close=close,
            rsi=rsi_value,
            drawdown=drawdown,
            atr_ratio=atr_ratio,
            dollar_volume=dollar_volume,
            trend_distance=close / sma_long - 1.0,
            stretch=(sma_short - close) / atr_value if atr_value else 0.0,
        )

    def _score(
        self, latest: dict[str, Any], checks: ScreenResult
    ) -> tuple[float, dict[str, float]]:
        dip = _clamp((self.rsi_entry - checks.rsi) / self.rsi_entry)
        stretch = _clamp(checks.stretch / 3.0)
        trend = _clamp(checks.trend_distance / 0.20)
        liquid = _clamp(
            (math.log10(max(checks.dollar_volume, 1.0)) - math.log10(self.min_dollar_volume)) / 2.0
        )
        parts = {"dip": dip, "stretch": stretch, "trend": trend, "liquid": liquid}
        score = 0.35 * dip + 0.30 * stretch + 0.20 * trend + 0.15 * liquid
        return round(score, 4), parts

    # -- フィルタ -----------------------------------------------------------

    _market_ok: bool = True

    def _benchmark_allows_entry(self, ctx: StrategyContext) -> bool:
        if not self.benchmark:
            return True
        if not ctx.has_bars(self.benchmark, self.benchmark_sma + 1):
            # ベンチマークが無いと地合いを判断できない。黙って通すより止める。
            return False
        frame = ctx.bars(self.benchmark)
        col = f"sma_{self.benchmark_sma}"
        if col not in frame.columns:
            frame = frame.with_columns(sma(self.benchmark_sma))
        latest = frame.row(-1, named=True)
        value = latest[col]
        return value is not None and latest["close"] > value

    def _in_blackout(self, symbol: str, as_of: dt.date) -> bool:
        for earnings in self.blackout.get(symbol, ()):
            if earnings - dt.timedelta(days=self.blackout_days_before) <= as_of <= earnings:
                return True
        return False

    def describe(self) -> str:
        return (
            f"{self.name}(sma={self.sma_short}/{self.sma_mid}/{self.sma_long}, "
            f"rsi{self.rsi_period}<{self.rsi_entry:g}, benchmark={self.benchmark})"
        )


class ScreenResult:
    """1銘柄のスクリーニング結果。"""

    __slots__ = (
        "atr_ratio",
        "close",
        "dollar_volume",
        "drawdown",
        "failed",
        "rsi",
        "stretch",
        "trend_distance",
    )

    def __init__(
        self,
        *,
        failed: list[str],
        close: float,
        rsi: float = 0.0,
        drawdown: float = 0.0,
        atr_ratio: float = 0.0,
        dollar_volume: float = 0.0,
        trend_distance: float = 0.0,
        stretch: float = 0.0,
    ) -> None:
        self.failed = failed
        self.close = close
        self.rsi = rsi
        self.drawdown = drawdown
        self.atr_ratio = atr_ratio
        self.dollar_volume = dollar_volume
        self.trend_distance = trend_distance
        self.stretch = stretch

    @property
    def passed(self) -> bool:
        return not self.failed

    def as_meta(self) -> dict[str, float]:
        return {
            "rsi": self.rsi,
            "drawdown": self.drawdown,
            "atr_ratio": self.atr_ratio,
            "dollar_volume": self.dollar_volume,
            "trend_distance": self.trend_distance,
            "stretch_atr": self.stretch,
        }


def load_blackout(path: Path) -> dict[str, list[dt.date]]:
    """決算日のブラックアウト表を読む。

    形式（TOML）::

        [earnings]
        AAPL = ["2026-10-29", "2027-01-28"]
        MSFT = ["2026-10-27"]

    日付は決算発表日。発表が引け後でも、当日は「翌日ギャップの前日」
    なのでブラックアウトに含める。
    """
    if not path.is_file():
        raise FileNotFoundError(f"ブラックアウト表が見つかりません: {path}")
    with path.open("rb") as fh:
        data = tomllib.load(fh)
    table = data.get("earnings", data)
    result: dict[str, list[dt.date]] = {}
    for symbol, dates in table.items():
        if not isinstance(dates, list):
            raise ValueError(f"{symbol}: 日付のリストを指定してください")
        result[symbol] = [
            d if isinstance(d, dt.date) else dt.date.fromisoformat(str(d)) for d in dates
        ]
    return result


def _clamp(value: float, low: float = 0.0, high: float = 1.0) -> float:
    return max(low, min(high, value))
