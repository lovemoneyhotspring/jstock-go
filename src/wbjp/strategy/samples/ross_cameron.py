"""ロス・キャメロン流モメンタム（Gap & Go / マイクロプルバック）の日足版。

Warrior Trading のロス・キャメロンが提唱する手法を、このシステムの制約
（日足・終値判断・大型株ユニバース）に翻訳したもの。**本家は分足のデイトレード**
なので、そのままは動かない。翻訳の対応表:

    本家（分足・小型株）                        この戦略（日足・スイング）
    ------------------------------------------  ------------------------------------------
    寄付前ギャップ ≧ 10%（材料株）               当日の始値が前日終値比 ≧ ``gap_min``
    相対出来高（RVOL）≧ 2〜5 倍                  当日出来高 ÷ 20日平均出来高 ≧ ``rvol_min``
    寄付前高値のブレイク（Gap & Go）             終値が直近 ``high_lookback`` 日高値を上抜け
    9EMA / 20EMA の上で推移                      終値 > EMA9 かつ EMA9 > EMA20
    1〜3本の押し目のあと直前高値抜け              「材料日」の後 1〜3 日の押し目 → 前日高値抜け
    （マイクロプルバック / ブルフラッグ）          （``allow_pullback_entry``）
    損切りは押し目安値・利確は 2R（2:1）           初期ストップと 2R 部分利確は ``[stops]`` に委ねる
    9EMA を割ったら降りる（トレーリング）          終値 < EMA9 で手仕舞い（``exit_on_ema``）
    株価 $2〜$20・浮動株 < 2,000 万株              ``min_price`` / ``max_price``（既定は無効）。浮動株数は
                                                  日足データに無いため未実装

本家の「勝率 60〜70%・損益比 2:1 以上」という骨格はそのまま残す。**出口の非対称性**
（負けは小さく・勝ちは 2R で半分＋EMA9 で追従）が主眼で、入口の条件は
「動意づいた瞬間に乗る」ためのものに過ぎない。

エントリー（2 種類。どちらも全条件 AND）:

    A. Gap & Go（材料日そのものに乗る）
        1. ギャップ: 始値 ≧ 前日終値 × (1 + gap_min)
        2. RVOL: 出来高 ≧ 平均出来高 × rvol_min
        3. 強い陽線: 終値 > 始値 かつ 終値がレンジの上位 ``close_strength`` 以上で引ける
        4. ブレイク: 終値 > 直近 high_lookback 日高値（当日を除く）
        5. 短期トレンド: 終値 > EMA9 > EMA20

    B. マイクロプルバック（材料日のあと、押し目からの再上昇に乗る）
        1. 直近 ``catalyst_lookback`` 日以内に A-1〜A-3 を満たした「材料日」がある
        2. 材料日以降、1〜``max_pullback_days`` 日の押し目（終値が前日を下回る日）がある
        3. 押し目の安値が EMA9 の上で止まった（トレンドが崩れていない）
        4. 当日: 終値 > 前日高値 かつ 終値 > 始値 かつ 出来高 > 前日出来高
        5. 短期トレンド: 終値 > EMA9 > EMA20

    共通: ATR/終値 が範囲内、20日平均売買代金 ≧ 下限、価格帯（任意）、
          地合い（任意、ベンチマーク > SMA）、決算ブラックアウト外（任意）。

    決算ブラックアウトは既定で **使わない**。本家は決算ギャップこそを材料として
    取りに行くため。ギャップの後追いが怖ければ ``blackout_file`` を指定する。

順位付け:
    条件を満たした銘柄に 0〜1 のスコアを付け、``direction``（0.3〜1.0）に写す。
    サイジングは direction の高い順に枠を埋めるので、スコアがそのまま採用順位。

    スコアの内訳（meta に残す）:
        - rvol     … 相対出来高。本家が最重視する「みんなが見ている株」の度合い
        - gap      … ギャップの大きさ（材料の強さ）
        - strength … 終値がレンジのどこで引けたか（ギャップを守り切ったか）
        - breakout … ブレイク幅（ATR 単位）

手仕舞い（保有中に direction = -1 を出す）:
    - 終値 < EMA9（``exit_on_ema``、本家の 9EMA トレーリング）
    - 終値 < 前日安値（``exit_on_prev_low``、既定は無効。EMA9 より早い）
    - 決算ブラックアウト入り（``blackout_file`` 指定時のみ）
    損切り・2R 部分利確・時間切れは :mod:`wbjp.risk.stops` が担当する。
"""

from __future__ import annotations

import datetime as dt
import math
from pathlib import Path
from typing import Any, ClassVar

import polars as pl

from wbjp.domain.models import Signal
from wbjp.indicators.ohlcv import atr, ema, sma
from wbjp.strategy.base import IndicatorStrategy, StrategyContext
from wbjp.strategy.samples.rsi_pullback import load_blackout

#: 意見を出すときの direction の下限。entry_threshold（既定 0.3）以上。
_DIRECTION_FLOOR = 0.3


class RossCameronStrategy(IndicatorStrategy):
    name: ClassVar[str] = "ross_cameron"

    def __init__(
        self,
        gap_min: float = 0.03,
        rvol_min: float = 2.0,
        close_strength: float = 0.7,
        high_lookback: int = 20,
        ema_fast: int = 9,
        ema_slow: int = 20,
        volume_lookback: int = 20,
        allow_gap_entry: bool = True,
        allow_pullback_entry: bool = True,
        catalyst_lookback: int = 5,
        max_pullback_days: int = 3,
        atr_period: int = 14,
        min_atr_ratio: float = 0.015,
        max_atr_ratio: float = 0.10,
        min_dollar_volume: float = 5_000_000.0,
        min_price: float | None = None,
        max_price: float | None = None,
        benchmark: str | None = "SPY",
        benchmark_sma: int = 50,
        blackout_file: str | None = None,
        blackout_days_before: int = 3,
        exit_before_earnings: bool = True,
        exit_on_ema: bool = True,
        exit_on_prev_low: bool = False,
    ) -> None:
        if gap_min <= 0:
            raise ValueError("gap_min は正の比率（例: 0.03）")
        if rvol_min <= 0:
            raise ValueError("rvol_min は正の倍率（例: 2.0）")
        if not 0.0 <= close_strength <= 1.0:
            raise ValueError("close_strength は 0〜1")
        if not ema_fast < ema_slow:
            raise ValueError("ema_fast < ema_slow を満たすこと")
        if not 0 < min_atr_ratio < max_atr_ratio:
            raise ValueError("0 < min_atr_ratio < max_atr_ratio を満たすこと")
        if min_price is not None and max_price is not None and not min_price < max_price:
            raise ValueError("min_price < max_price を満たすこと")
        if not (allow_gap_entry or allow_pullback_entry):
            raise ValueError("allow_gap_entry / allow_pullback_entry のどちらかは有効にすること")
        if max_pullback_days < 1 or catalyst_lookback <= max_pullback_days:
            raise ValueError("1 ≦ max_pullback_days < catalyst_lookback を満たすこと")

        self.gap_min = gap_min
        self.rvol_min = rvol_min
        self.close_strength = close_strength
        self.high_lookback = high_lookback
        self.ema_fast = ema_fast
        self.ema_slow = ema_slow
        self.volume_lookback = volume_lookback
        self.allow_gap_entry = allow_gap_entry
        self.allow_pullback_entry = allow_pullback_entry
        self.catalyst_lookback = catalyst_lookback
        self.max_pullback_days = max_pullback_days
        self.atr_period = atr_period
        self.min_atr_ratio = min_atr_ratio
        self.max_atr_ratio = max_atr_ratio
        self.min_dollar_volume = min_dollar_volume
        self.min_price = min_price
        self.max_price = max_price
        self.benchmark = benchmark
        self.benchmark_sma = benchmark_sma
        self.blackout_days_before = blackout_days_before
        self.exit_before_earnings = exit_before_earnings
        self.exit_on_ema = exit_on_ema
        self.exit_on_prev_low = exit_on_prev_low
        self.blackout: dict[str, list[dt.date]] = (
            load_blackout(Path(blackout_file)) if blackout_file else {}
        )
        # EMA は種の平均から収束するまで数周期かかるため、遅い方の 3 倍を見込む
        self.warmup_bars = (
            max(3 * ema_slow, high_lookback, volume_lookback, atr_period, benchmark_sma)
            + catalyst_lookback
            + 2
        )

    # -- 指標 ---------------------------------------------------------------

    def indicators(self) -> list[pl.Expr]:
        prev_close = pl.col("close").shift(1)
        avg_volume = pl.col("volume").shift(1).rolling_mean(self.volume_lookback)
        return [
            ema(self.ema_fast),
            ema(self.ema_slow),
            atr(self.atr_period),
            prev_close.alias("prev_close"),
            pl.col("high").shift(1).alias("prev_high"),
            pl.col("low").shift(1).alias("prev_low"),
            pl.col("volume").shift(1).alias("prev_volume"),
            # 当日を除いた直近高値。当日を含めると「終値 > 当日高値」が成り立たない
            pl.col("high").shift(1).rolling_max(self.high_lookback).alias(self._high_col),
            # 当日を除いた平均出来高。当日の急増を分母に入れると RVOL が薄まる
            avg_volume.alias(self._avg_volume_col),
            (pl.col("open") / prev_close - 1.0).alias("gap"),
            (pl.col("volume") / avg_volume).alias("rvol"),
            (pl.col("close") * pl.col("volume"))
            .rolling_mean(self.volume_lookback)
            .alias(self._dollar_volume_col),
        ]

    @property
    def _high_col(self) -> str:
        return f"prior_high_{self.high_lookback}"

    @property
    def _avg_volume_col(self) -> str:
        return f"avg_volume_{self.volume_lookback}"

    @property
    def _dollar_volume_col(self) -> str:
        return f"dollar_volume_{self.volume_lookback}"

    # -- 判断 ---------------------------------------------------------------

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        self._market_ok = self._benchmark_allows_entry(ctx)
        return super().on_bars(ctx)

    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        if symbol == self.benchmark:
            return None

        latest = frame.row(-1, named=True)
        if None in (latest["close"], latest[f"ema_{self.ema_fast}"], latest["prev_low"]):
            return None

        in_blackout = self._in_blackout(symbol, ctx.as_of)
        position = ctx.position(symbol)
        if position is not None and position.quantity > 0:
            return self._evaluate_exit(symbol, latest, in_blackout)

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
                f"{checks.setup}: ギャップ {checks.gap:+.1%}, RVOL {checks.rvol:.1f}x, "
                f"引け強度 {checks.strength:.0%}, スコア {score:.2f}"
            ),
            meta={"score": score, **parts, **checks.as_meta()},
        )

    def _evaluate_exit(
        self, symbol: str, latest: dict[str, Any], in_blackout: bool
    ) -> Signal | None:
        close = latest["close"]
        ema_fast = latest[f"ema_{self.ema_fast}"]
        prev_low = latest["prev_low"]

        reason = None
        if in_blackout and self.exit_before_earnings:
            reason = "決算前のため手仕舞い"
        elif self.exit_on_ema and close < ema_fast:
            reason = f"終値 {close:.2f} が EMA{self.ema_fast} {ema_fast:.2f} を割った"
        elif self.exit_on_prev_low and close < prev_low:
            reason = f"終値 {close:.2f} が前日安値 {prev_low:.2f} を割った"

        if reason is None:
            # 保有継続。意見が無いと sizer が「シグナル消滅」で手仕舞うため、
            # 明示的に弱い買いを返す。
            return Signal(
                self.name, symbol, direction=0.5, reason=f"保有継続（EMA{self.ema_fast} の上）"
            )
        return Signal(self.name, symbol, direction=-1.0, reason=reason, meta={"ema_fast": ema_fast})

    # -- スクリーニング -----------------------------------------------------

    def screen(self, frame: pl.DataFrame) -> ScreenResult:
        """1銘柄の足に対して、エントリー条件を1つずつ評価する。

        ``passed`` が False でも各条件の値を返すので、「なぜ落ちたか」を表示できる。
        Gap & Go とマイクロプルバックの両方を試し、通った方を ``setup`` に残す。
        """
        latest = frame.row(-1, named=True)
        close = latest["close"]
        values = (
            close,
            latest["open"],
            latest["high"],
            latest["low"],
            latest[f"ema_{self.ema_fast}"],
            latest[f"ema_{self.ema_slow}"],
            latest[f"atr_{self.atr_period}"],
            latest["gap"],
            latest["rvol"],
            latest[self._high_col],
            latest[self._dollar_volume_col],
        )
        if any(v is None for v in values):
            return ScreenResult(failed=["指標が未計算"], close=close or 0.0)

        atr_value = latest[f"atr_{self.atr_period}"]
        atr_ratio = atr_value / close if close else 0.0
        strength = _close_strength(latest)
        breakout = (close - latest[self._high_col]) / atr_value if atr_value else 0.0

        # 共通フィルタ（どちらのセットアップでも必須）
        failed: list[str] = []
        if not (close > latest[f"ema_{self.ema_fast}"] > latest[f"ema_{self.ema_slow}"]):
            failed.append(f"短期トレンド: 終値 > EMA{self.ema_fast} > EMA{self.ema_slow} でない")
        if not (self.min_atr_ratio <= atr_ratio <= self.max_atr_ratio):
            failed.append(f"ATR比 {atr_ratio:.1%} が範囲外")
        if latest[self._dollar_volume_col] < self.min_dollar_volume:
            failed.append(f"売買代金 {latest[self._dollar_volume_col]:,.0f} が下限未満")
        if self.min_price is not None and close < self.min_price:
            failed.append(f"株価 {close:.2f} < {self.min_price:g}")
        if self.max_price is not None and close > self.max_price:
            failed.append(f"株価 {close:.2f} > {self.max_price:g}")
        if not self._market_ok:
            failed.append(f"地合い: {self.benchmark} が SMA{self.benchmark_sma} 割れ")

        # セットアップ判定。通ったものがあればそれを採用する
        setup = None
        setup_failed: list[str] = []
        gap, rvol = latest["gap"], latest["rvol"]
        if self.allow_gap_entry:
            reasons = self._gap_and_go_failures(latest, strength)
            if not reasons:
                setup = "Gap&Go"
            else:
                setup_failed.extend(f"Gap&Go: {r}" for r in reasons)
        if setup is None and self.allow_pullback_entry:
            reasons, catalyst = self._pullback_failures(frame)
            if not reasons and catalyst is not None:
                setup = "マイクロプルバック"
                gap, rvol = catalyst["gap"], catalyst["rvol"]
            else:
                setup_failed.extend(f"押し目: {r}" for r in reasons)
        if setup is None:
            failed.extend(setup_failed)

        return ScreenResult(
            failed=failed,
            close=close,
            setup=setup or "",
            gap=gap,
            rvol=rvol,
            strength=strength,
            breakout=breakout,
            atr_ratio=atr_ratio,
            dollar_volume=latest[self._dollar_volume_col],
        )

    def _gap_and_go_failures(self, bar: dict[str, Any], strength: float) -> list[str]:
        """Gap & Go の条件（材料日そのものの判定）。空なら合格。"""
        failed = []
        if bar["gap"] < self.gap_min:
            failed.append(f"ギャップ {bar['gap']:+.1%} < {self.gap_min:.1%}")
        if bar["rvol"] < self.rvol_min:
            failed.append(f"RVOL {bar['rvol']:.1f}x < {self.rvol_min:g}x")
        if not (bar["close"] > bar["open"] and strength >= self.close_strength):
            failed.append(f"陽線で強く引けていない（引け強度 {strength:.0%}）")
        if bar["close"] <= bar[self._high_col]:
            failed.append(f"直近 {self.high_lookback} 日高値を抜けていない")
        return failed

    def _is_catalyst_day(self, bar: dict[str, Any]) -> bool:
        """材料日: ギャップ ＋ RVOL ＋ 強い陽線（ブレイクは問わない）。"""
        if None in (bar["gap"], bar["rvol"], bar["close"], bar["open"]):
            return False
        return (
            bar["gap"] >= self.gap_min
            and bar["rvol"] >= self.rvol_min
            and bar["close"] > bar["open"]
            and _close_strength(bar) >= self.close_strength
        )

    def _pullback_failures(self, frame: pl.DataFrame) -> tuple[list[str], dict[str, Any] | None]:
        """マイクロプルバックの条件。(不合格理由, 材料日の足) を返す。"""
        latest = frame.row(-1, named=True)
        # 直近 catalyst_lookback 日（当日を除く）から材料日を探す。新しい方を優先
        window = frame.tail(self.catalyst_lookback + 1)
        rows = [window.row(i, named=True) for i in range(window.height)]
        catalyst_index = next(
            (i for i in range(len(rows) - 2, -1, -1) if self._is_catalyst_day(rows[i])), None
        )
        if catalyst_index is None:
            return [f"直近 {self.catalyst_lookback} 日に材料日がない"], None

        between = rows[catalyst_index + 1 : -1]
        pullback = [b for b in between if b["close"] < b["prev_close"]]
        failed = []
        if not between or not pullback:
            failed.append("材料日のあとに押し目がない")
        elif len(between) > self.max_pullback_days:
            failed.append(f"押し目が {len(between)} 日と長すぎる（上限 {self.max_pullback_days}）")
        elif any(b["low"] < b[f"ema_{self.ema_fast}"] for b in pullback):
            failed.append(f"押し目の安値が EMA{self.ema_fast} を割った")
        if not (latest["close"] > latest["prev_high"] and latest["close"] > latest["open"]):
            failed.append("前日高値を陽線で抜けていない")
        if latest["volume"] <= latest["prev_volume"]:
            failed.append("出来高が前日を上回っていない")
        return failed, rows[catalyst_index]

    def _score(self, checks: ScreenResult) -> tuple[float, dict[str, float]]:
        rvol = _clamp((checks.rvol - self.rvol_min) / (3.0 * self.rvol_min))
        gap = _clamp((checks.gap - self.gap_min) / (3.0 * self.gap_min))
        strength = _clamp(
            (checks.strength - self.close_strength) / max(1.0 - self.close_strength, 1e-9)
        )
        breakout = _clamp(checks.breakout / 2.0)
        parts = {"rvol": rvol, "gap": gap, "strength": strength, "breakout": breakout}
        score = 0.35 * rvol + 0.30 * gap + 0.20 * strength + 0.15 * breakout
        return round(score, 4), parts

    # -- フィルタ -----------------------------------------------------------

    _market_ok: bool = True

    def _benchmark_allows_entry(self, ctx: StrategyContext) -> bool:
        if not self.benchmark:
            return True
        if not ctx.has_bars(self.benchmark, self.benchmark_sma + 1):
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
            f"{self.name}(gap≥{self.gap_min:.0%}, rvol≥{self.rvol_min:g}x, "
            f"ema={self.ema_fast}/{self.ema_slow}, benchmark={self.benchmark})"
        )


class ScreenResult:
    """1銘柄のスクリーニング結果。"""

    __slots__ = (
        "atr_ratio",
        "breakout",
        "close",
        "dollar_volume",
        "failed",
        "gap",
        "rvol",
        "setup",
        "strength",
    )

    def __init__(
        self,
        *,
        failed: list[str],
        close: float,
        setup: str = "",
        gap: float = 0.0,
        rvol: float = 0.0,
        strength: float = 0.0,
        breakout: float = 0.0,
        atr_ratio: float = 0.0,
        dollar_volume: float = 0.0,
    ) -> None:
        self.failed = failed
        self.close = close
        self.setup = setup
        self.gap = gap
        self.rvol = rvol
        self.strength = strength
        self.breakout = breakout
        self.atr_ratio = atr_ratio
        self.dollar_volume = dollar_volume

    @property
    def passed(self) -> bool:
        return not self.failed

    def as_meta(self) -> dict[str, Any]:
        return {
            "setup": self.setup,
            "close": self.close,
            "gap_pct": self.gap,
            "rvol_x": self.rvol,
            "close_strength": self.strength,
            "breakout_atr": self.breakout,
            "atr_ratio": self.atr_ratio,
            "dollar_volume": self.dollar_volume,
        }


def _close_strength(bar: dict[str, Any]) -> float:
    """終値がその日のレンジのどこで引けたか（0 = 安値引け、1 = 高値引け）。"""
    span = bar["high"] - bar["low"]
    if not span or span <= 0:
        return 1.0 if bar["close"] >= bar["open"] else 0.0
    return _clamp((bar["close"] - bar["low"]) / span)


def _clamp(value: float, low: float = 0.0, high: float = 1.0) -> float:
    if math.isnan(value):
        return low
    return max(low, min(high, value))
