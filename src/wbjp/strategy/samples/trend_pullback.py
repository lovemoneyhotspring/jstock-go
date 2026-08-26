"""上昇トレンド銘柄の「押し目からのブレイクアウト」（勝率重視のスイング）。

考え方:
    平均回帰（オシレーターの逆張り）ではなく、**「上昇トレンド銘柄が
    健全な押し目をつけ、出来高が枯れた後に反発を始めた瞬間」を拾う**。
    押し目そのもの（下落中）では買わない。出来高が細り、下値が固まって
    から、直近高値（または保ち合いレンジ）を上抜けた日にだけ意見を出す。

エントリー（全条件 AND。1つでも欠ければ意見を出さない）:
    環境認識（母集団の選定）:
    1. 終値 > SMA200 かつ SMA50 > SMA200            … 長期上昇トレンド
    2. SMA200 と SMA50 がともに上向き（``slope_lookback`` 日前より高い）
                                                       … トレンドが生きている
    3. 相対的強さ（RS）: 過去 ``rs_lookback`` 日の騰落率がベンチマーク
       （既定 SPY）を上回る                            … 資金が向かっている銘柄
    4. 20日平均売買代金 ≧ 下限                        … 指値が乗る流動性

    エントリートリガー（購入の合図）:
    5. 直近 ``high_lookback`` 日高値からの下落率 ≦ 上限 … 押し目であって崩れではない
    6. 出来高の急減: 前日の出来高が、過去 ``volume_lookback`` 日平均の
       ``volume_dryup_max`` 倍以下                     … 売り手が尽きたサイン
    7. 反発トリガー: 終値が「直近 ``breakout_lookback`` 日高値」を上抜けた
                                                       … 既定 1 日 = 前日高値のブレイク
    8. ATR/終値 が下限〜上限                          … 動かない株・荒れ過ぎの株を除外
    9. 地合い: ベンチマークの終値 > その SMA           … 市場全体の下げ局面では建てない
    10. 決算ブラックアウト外                           … 日足ではギャップを避けられない

順位付け:
    条件を満たした銘柄には 0〜1 のスコアを付け、``direction`` に写す
    （0.3〜1.0）。サイジングは direction の高い順に ``max_positions`` の
    枠を埋めるので、**スコアがそのまま採用順位になる**。

    スコアの内訳（meta に残す）:
        - dryup  … 出来高がどれだけ枯れたか。押し目の質
        - rs     … ベンチマークに対する超過リターン。資金の向かい先
        - trend  … SMA200 からの距離。トレンドの強さ
        - liquid … 売買代金（対数）。滑りにくさ
    重みは押し目の質（dryup）と資金の向かい先（rs）を重く見る。

手仕舞い:
    損切り・建値ストップ・時間切れ・2段階利確・利確後のトレンド追従は
    すべて :mod:`wbjp.risk.stops` が担当する（``config.stops`` で設定）。
    この戦略が保有中に direction=-1 を出すのは、決算ブラックアウトに
    入ったときだけ。それ以外は弱い買い（0.5）を返してストップ管理に委ねる。
"""

from __future__ import annotations

import datetime as dt
import math
import tomllib
from pathlib import Path
from typing import ClassVar

import polars as pl

from wbjp.domain.models import Signal
from wbjp.indicators.ohlcv import atr, donchian_high, roc, sma
from wbjp.strategy.base import IndicatorStrategy, StrategyContext

#: 意見を出すときの direction の下限。entry_threshold（既定 0.3）以上。
_DIRECTION_FLOOR = 0.3


class TrendPullbackStrategy(IndicatorStrategy):
    name: ClassVar[str] = "trend_pullback"

    def __init__(
        self,
        sma_long: int = 200,
        sma_mid: int = 50,
        sma_short: int = 20,
        slope_lookback: int = 10,
        rs_lookback: int = 63,
        high_lookback: int = 60,
        max_drawdown_from_high: float = 0.15,
        breakout_lookback: int = 1,
        volume_lookback: int = 20,
        volume_dryup_max: float = 0.7,
        atr_period: int = 14,
        min_atr_ratio: float = 0.015,
        max_atr_ratio: float = 0.05,
        min_dollar_volume: float = 5_000_000.0,
        benchmark: str | None = "SPY",
        benchmark_sma: int = 50,
        blackout_file: str | None = None,
        blackout_days_before: int = 3,
        exit_before_earnings: bool = True,
    ) -> None:
        if not sma_short < sma_mid < sma_long:
            raise ValueError("sma_short < sma_mid < sma_long を満たすこと")
        if not 0 < min_atr_ratio < max_atr_ratio:
            raise ValueError("0 < min_atr_ratio < max_atr_ratio を満たすこと")
        if not 0 < volume_dryup_max < 1:
            raise ValueError("volume_dryup_max は 0 より大きく 1 未満")

        self.sma_long = sma_long
        self.sma_mid = sma_mid
        self.sma_short = sma_short
        self.slope_lookback = slope_lookback
        self.rs_lookback = rs_lookback
        self.high_lookback = high_lookback
        self.max_drawdown_from_high = max_drawdown_from_high
        self.breakout_lookback = breakout_lookback
        self.volume_lookback = volume_lookback
        self.volume_dryup_max = volume_dryup_max
        self.atr_period = atr_period
        self.min_atr_ratio = min_atr_ratio
        self.max_atr_ratio = max_atr_ratio
        self.min_dollar_volume = min_dollar_volume
        self.benchmark = benchmark
        self.benchmark_sma = benchmark_sma
        self.blackout_days_before = blackout_days_before
        self.exit_before_earnings = exit_before_earnings
        self.blackout: dict[str, list[dt.date]] = (
            load_blackout(Path(blackout_file)) if blackout_file else {}
        )
        self.warmup_bars = (
            max(sma_long, high_lookback, benchmark_sma, rs_lookback) + slope_lookback + 1
        )

    # -- 指標 ---------------------------------------------------------------

    def indicators(self) -> list[pl.Expr]:
        return [
            sma(self.sma_long),
            sma(self.sma_mid),
            sma(self.sma_short),
            atr(self.atr_period),
            roc(self.rs_lookback),
            donchian_high(self.breakout_lookback),
            pl.col("high").rolling_max(self.high_lookback).alias(self._high_col),
            (pl.col("close") * pl.col("volume"))
            .rolling_mean(self.volume_lookback)
            .alias(self._dollar_volume_col),
            (
                pl.col("volume").shift(1)
                / pl.col("volume").rolling_mean(self.volume_lookback).shift(1)
            ).alias(self._dryup_col),
        ]

    @property
    def _high_col(self) -> str:
        return f"high_{self.high_lookback}"

    @property
    def _dollar_volume_col(self) -> str:
        return f"dollar_volume_{self.volume_lookback}"

    @property
    def _dryup_col(self) -> str:
        return f"volume_dryup_{self.volume_lookback}"

    @property
    def _breakout_col(self) -> str:
        return f"donchian_high_{self.breakout_lookback}"

    @property
    def _rs_col(self) -> str:
        return f"roc_{self.rs_lookback}"

    # -- 判断 ---------------------------------------------------------------

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        self._market_ok = self._benchmark_allows_entry(ctx)
        self._benchmark_return = self._benchmark_rs_return(ctx)
        return super().on_bars(ctx)

    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        if symbol == self.benchmark:
            return None  # ベンチマークは売買対象にしない

        in_blackout = self._in_blackout(symbol, ctx.as_of)
        position = ctx.position(symbol)
        held = position is not None and position.quantity > 0

        if held:
            return self._evaluate_exit(symbol, in_blackout)

        if in_blackout:
            return None
        return self._evaluate_entry(symbol, frame)

    def _evaluate_entry(self, symbol: str, frame: pl.DataFrame) -> Signal | None:
        checks = self.screen(frame)
        if not checks.passed:
            return None

        score, parts = self._score(checks)
        direction = _DIRECTION_FLOOR + (1.0 - _DIRECTION_FLOOR) * score
        return Signal(
            self.name,
            symbol,
            direction=round(direction, 4),
            confidence=1.0,
            reason=(
                f"押し目からのブレイク: 出来高比 {checks.dryup_ratio:.0%}, "
                f"高値から {checks.drawdown:.1%}, RS +{checks.rs_margin:.1f}pt, "
                f"スコア {score:.2f}"
            ),
            meta={"score": score, **parts, **checks.as_meta()},
        )

    def _evaluate_exit(self, symbol: str, in_blackout: bool) -> Signal | None:
        """保有中の判断。損切り・利確・時間切れは :mod:`wbjp.risk.stops` に委ねる。

        ここで判断するのは、日足の指標だけでは分からない決算リスクだけ。
        """
        if in_blackout and self.exit_before_earnings:
            return Signal(self.name, symbol, direction=-1.0, reason="決算前のため手仕舞い")
        return Signal(
            self.name,
            symbol,
            direction=0.5,
            reason="保有継続（損切り・利確はストップ管理に委ねる）",
        )

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
        atr_value = latest[f"atr_{self.atr_period}"]
        high = latest[self._high_col]
        dollar_volume = latest[self._dollar_volume_col]
        dryup_ratio = latest[self._dryup_col]
        breakout_high = latest[self._breakout_col]
        rs_value = latest[self._rs_col]

        older_mid = (
            frame[f"sma_{self.sma_mid}"][-1 - self.slope_lookback]
            if frame.height > self.slope_lookback
            else None
        )
        older_long = (
            frame[f"sma_{self.sma_long}"][-1 - self.slope_lookback]
            if frame.height > self.slope_lookback
            else None
        )

        values = (
            close,
            sma_long,
            sma_mid,
            atr_value,
            high,
            dollar_volume,
            dryup_ratio,
            breakout_high,
        )
        if any(v is None for v in values):
            return ScreenResult(failed=["指標が未計算"], close=close or 0.0)

        drawdown = 1.0 - close / high if high else 1.0
        atr_ratio = atr_value / close if close else 0.0
        rs_margin = (
            rs_value - self._benchmark_return
            if rs_value is not None and self._benchmark_return is not None
            else None
        )

        failed = []
        if not (close > sma_long and sma_mid > sma_long):
            failed.append(f"トレンド: 終値 > SMA{self.sma_long} > … を満たさない")
        if older_long is not None and sma_long <= older_long:
            failed.append(f"SMA{self.sma_long} が下向き")
        if older_mid is not None and sma_mid <= older_mid:
            failed.append(f"SMA{self.sma_mid} が下向き")
        if self.benchmark and (
            self._benchmark_return is None or rs_margin is None or rs_margin <= 0
        ):
            failed.append(f"RS: {self.benchmark} に対する超過リターンが無い/マイナス")
        if drawdown > self.max_drawdown_from_high:
            failed.append(f"高値から {drawdown:.1%} 下落（上限 {self.max_drawdown_from_high:.0%}）")
        if dryup_ratio > self.volume_dryup_max:
            failed.append(
                f"出来高比 {dryup_ratio:.0%} が枯れていない（上限 {self.volume_dryup_max:.0%}）"
            )
        if close <= breakout_high:
            failed.append(f"直近{self.breakout_lookback}日高値 {breakout_high:.2f} を未達")
        if not (self.min_atr_ratio <= atr_ratio <= self.max_atr_ratio):
            failed.append(f"ATR比 {atr_ratio:.1%} が範囲外")
        if dollar_volume < self.min_dollar_volume:
            failed.append(f"売買代金 {dollar_volume:,.0f} が下限未満")
        if not self._market_ok:
            failed.append(f"地合い: {self.benchmark} が SMA{self.benchmark_sma} 割れ")

        return ScreenResult(
            failed=failed,
            close=close,
            dryup_ratio=dryup_ratio,
            drawdown=drawdown,
            atr_ratio=atr_ratio,
            dollar_volume=dollar_volume,
            trend_distance=close / sma_long - 1.0,
            rs_margin=rs_margin or 0.0,
        )

    def _score(self, checks: ScreenResult) -> tuple[float, dict[str, float]]:
        dryup = _clamp((self.volume_dryup_max - checks.dryup_ratio) / self.volume_dryup_max)
        rs = _clamp(checks.rs_margin / 20.0)
        trend = _clamp(checks.trend_distance / 0.20)
        liquid = _clamp(
            (math.log10(max(checks.dollar_volume, 1.0)) - math.log10(self.min_dollar_volume)) / 2.0
        )
        parts = {"dryup": dryup, "rs": rs, "trend": trend, "liquid": liquid}
        score = 0.35 * dryup + 0.30 * rs + 0.20 * trend + 0.15 * liquid
        return round(score, 4), parts

    # -- フィルタ -----------------------------------------------------------

    _market_ok: bool = True
    _benchmark_return: float | None = None

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

    def _benchmark_rs_return(self, ctx: StrategyContext) -> float | None:
        """ベンチマークの過去 ``rs_lookback`` 日騰落率。RS 比較の基準にする。"""
        if not self.benchmark:
            return None
        if not ctx.has_bars(self.benchmark, self.rs_lookback + 1):
            return None
        frame = ctx.bars(self.benchmark)
        col = self._rs_col
        if col not in frame.columns:
            frame = frame.with_columns(roc(self.rs_lookback))
        value = frame.row(-1, named=True)[col]
        return float(value) if value is not None else None

    def _in_blackout(self, symbol: str, as_of: dt.date) -> bool:
        for earnings in self.blackout.get(symbol, ()):
            if earnings - dt.timedelta(days=self.blackout_days_before) <= as_of <= earnings:
                return True
        return False

    def describe(self) -> str:
        return (
            f"{self.name}(sma={self.sma_short}/{self.sma_mid}/{self.sma_long}, "
            f"breakout={self.breakout_lookback}日高値, benchmark={self.benchmark})"
        )


class ScreenResult:
    """1銘柄のスクリーニング結果。"""

    __slots__ = (
        "atr_ratio",
        "close",
        "dollar_volume",
        "drawdown",
        "dryup_ratio",
        "failed",
        "rs_margin",
        "trend_distance",
    )

    def __init__(
        self,
        *,
        failed: list[str],
        close: float,
        dryup_ratio: float = 0.0,
        drawdown: float = 0.0,
        atr_ratio: float = 0.0,
        dollar_volume: float = 0.0,
        trend_distance: float = 0.0,
        rs_margin: float = 0.0,
    ) -> None:
        self.failed = failed
        self.close = close
        self.dryup_ratio = dryup_ratio
        self.drawdown = drawdown
        self.atr_ratio = atr_ratio
        self.dollar_volume = dollar_volume
        self.trend_distance = trend_distance
        self.rs_margin = rs_margin

    @property
    def passed(self) -> bool:
        return not self.failed

    def as_meta(self) -> dict[str, float]:
        return {
            "dryup_ratio": self.dryup_ratio,
            "drawdown": self.drawdown,
            "atr_ratio": self.atr_ratio,
            "dollar_volume": self.dollar_volume,
            "trend_distance": self.trend_distance,
            "rs_margin": self.rs_margin,
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
