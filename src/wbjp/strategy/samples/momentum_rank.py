"""クロスセクショナル・モメンタム（順位付き、月次入れ替え）。

**なぜこれか**

「過去 6〜12 ヶ月に強かった銘柄は、その後も数ヶ月は強い」という
モメンタム効果は、1990 年代の発見以降も市場・期間をまたいで生き残って
いる数少ない優位性の1つ。勝率は 5 割前後だが、勝ちが伸びて負けが小さい
（損益比型）。短期の押し目買いが「タイトな損切りに勝率の優位を食われる」
のと対照的に、こちらは損切りを広く取り、**手仕舞いはトレンドの崩れと
順位の脱落**で決める。

最大の弱点は急反転局面（モメンタムクラッシュ）。ベンチマークが長期
移動平均を割ったら新規建てを止め、建玉も全部降りることで軽減する。

母集団（スクリーニング）:
    1. 20日平均売買代金 ≧ 下限、株価 ≧ 下限   … 滑らない
    2. ATR/終値 ≦ 上限                          … 崩れかけの荒い銘柄を除く
    3. 終値 > SMA（``trend_sma``）              … 自身のトレンドが生きている
    4. 6ヶ月リターン（直近1ヶ月除く）> 0、12ヶ月リターン > 0
       直近1ヶ月を除くのは、短期の平均回帰（先月上げた株は今月下げやすい）
       を順位から取り除くため。
    5. 6ヶ月リターンがベンチマークを上回る       … 市場平均に勝っている
    6. 地合い: ベンチマーク > その SMA          … 市場全体の下げ局面では建てない

順位:
    リスク調整後モメンタム = 6ヶ月リターン ÷ 年率ボラティリティ。
    同じ上昇率なら、静かに上げた銘柄を上に置く。順位を direction
    （0.3〜1.0）に写し、サイジングが上位から枠を埋める。

売買の間隔:
    新規建ては ``rebalance``（monthly / weekly / daily）の区切りの日だけ。
    毎日入れ替えると売買代金がかさみ、モメンタムの「ゆっくり効く」性質と
    合わない。手仕舞いのうち「トレンド崩れ」「地合いオフ」は毎日判定し、
    「順位の脱落」は区切りの日だけ判定する。

手仕舞い:
    - 終値 < SMA（``trend_sma``）              … トレンド崩れ（毎日）
    - ベンチマークが SMA 割れ                   … 地合いオフ（毎日、全建玉）
    - 区切りの日に順位が上位 ``keep_multiple × top_n`` から脱落
    損切りは :mod:`wbjp.risk.stops` の広い ATR トレーリングに任せる。
    時間切れ・建値移動・利確は使わない（勝ちを伸ばすため）。
"""

from __future__ import annotations

import math
from typing import Any, ClassVar

import polars as pl

from wbjp.domain.models import Signal
from wbjp.indicators.ohlcv import atr, sma
from wbjp.strategy.base import IndicatorStrategy, StrategyContext

_DIRECTION_FLOOR = 0.3
_TRADING_DAYS = 252


class MomentumRankStrategy(IndicatorStrategy):
    name: ClassVar[str] = "momentum_rank"

    def __init__(
        self,
        lookback: int = 126,
        skip: int = 21,
        long_lookback: int = 252,
        vol_lookback: int = 63,
        trend_sma: int = 100,
        atr_period: int = 14,
        max_atr_ratio: float = 0.06,
        min_price: float = 5.0,
        min_dollar_volume: float = 20_000_000.0,
        volume_lookback: int = 20,
        benchmark: str | None = "SPY",
        benchmark_sma: int = 200,
        top_n: int = 5,
        keep_multiple: int = 2,
        rebalance: str = "monthly",
    ) -> None:
        if lookback <= 0 or skip < 0 or long_lookback <= lookback + skip:
            raise ValueError("long_lookback > lookback + skip > 0 を満たすこと")
        if rebalance not in {"monthly", "weekly", "daily"}:
            raise ValueError(f"rebalance は monthly / weekly / daily: {rebalance}")
        if top_n <= 0 or keep_multiple < 1:
            raise ValueError("top_n は正、keep_multiple は 1 以上")

        self.lookback = lookback
        self.skip = skip
        self.long_lookback = long_lookback
        self.vol_lookback = vol_lookback
        self.trend_sma = trend_sma
        self.atr_period = atr_period
        self.max_atr_ratio = max_atr_ratio
        self.min_price = min_price
        self.min_dollar_volume = min_dollar_volume
        self.volume_lookback = volume_lookback
        self.benchmark = benchmark
        self.benchmark_sma = benchmark_sma
        self.top_n = top_n
        self.keep_multiple = keep_multiple
        self.rebalance = rebalance
        self.warmup_bars = max(long_lookback, benchmark_sma, trend_sma) + skip + 2

    # -- 指標 ---------------------------------------------------------------

    def indicators(self) -> list[pl.Expr]:
        close = pl.col("close")
        return [
            sma(self.trend_sma),
            atr(self.atr_period),
            (close.shift(self.skip) / close.shift(self.skip + self.lookback) - 1.0).alias(
                "mom_mid"
            ),
            (close / close.shift(self.long_lookback) - 1.0).alias("mom_long"),
            (
                (close / close.shift(1)).log().rolling_std(self.vol_lookback)
                * math.sqrt(_TRADING_DAYS)
            ).alias("vol_ann"),
            (close * pl.col("volume")).rolling_mean(self.volume_lookback).alias("dollar_volume"),
        ]

    # -- 判断 ---------------------------------------------------------------

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        expressions = self.indicators()
        names = self.indicator_names()

        frames: dict[str, pl.DataFrame] = {}
        for symbol in ctx.symbols:
            if not ctx.has_bars(symbol, self.warmup_bars):
                continue
            frame = ctx.bars(symbol)
            if not set(names).issubset(frame.columns):
                frame = frame.with_columns(expressions)
            frames[symbol] = frame

        market_ok, benchmark_mom = self._benchmark_state(ctx, frames)
        rebalance_day = self._is_rebalance_day(frames)

        # 1) 母集団を絞り、順位付けの材料を集める
        ranked: list[tuple[str, float, dict[str, float]]] = []
        for symbol, frame in frames.items():
            if symbol == self.benchmark:
                continue
            result = self.screen(frame, benchmark_mom, market_ok)
            if result.passed:
                ranked.append((symbol, result.score, result.as_meta()))
        ranked.sort(key=lambda item: -item[1])
        rank_of = {symbol: index + 1 for index, (symbol, _, _) in enumerate(ranked)}
        keep_rank = self.top_n * self.keep_multiple

        signals: list[Signal] = []

        # 2) 保有中の判断（毎日）
        for symbol in ctx.held_symbols:
            held_frame = frames.get(symbol)
            if held_frame is None or symbol == self.benchmark:
                continue
            latest = held_frame.row(-1, named=True)
            reason = None
            if not market_ok:
                reason = f"地合いオフ: {self.benchmark} が SMA{self.benchmark_sma} 割れ"
            elif latest[f"sma_{self.trend_sma}"] is not None and (
                latest["close"] < latest[f"sma_{self.trend_sma}"]
            ):
                reason = f"トレンド崩れ: 終値が SMA{self.trend_sma} を割った"
            elif rebalance_day and rank_of.get(symbol, math.inf) > keep_rank:
                reason = (
                    f"順位脱落: {rank_of.get(symbol, '圏外')} 位（上位 {keep_rank} 位まで保持）"
                )

            if reason:
                signals.append(Signal(self.name, symbol, direction=-1.0, reason=reason))
            else:
                signals.append(
                    Signal(
                        self.name,
                        symbol,
                        direction=0.5,
                        reason=f"保有継続（順位 {rank_of.get(symbol, '圏外')}）",
                    )
                )

        # 3) 新規建ては区切りの日だけ
        if rebalance_day and market_ok and ranked:
            top_score = ranked[0][1]
            for symbol, score, meta in ranked:
                if symbol in ctx.held_symbols:
                    continue
                rank = rank_of[symbol]
                # 1位を 1.0、順位が下がるほど下限に近づける
                relative = score / top_score if top_score > 0 else 0.0
                direction = _DIRECTION_FLOOR + (1.0 - _DIRECTION_FLOOR) * relative
                signals.append(
                    Signal(
                        self.name,
                        symbol,
                        direction=round(direction, 4),
                        reason=(
                            f"モメンタム {rank} 位: 6M {meta['mom_mid']:+.1%} / "
                            f"12M {meta['mom_long']:+.1%} / ボラ {meta['vol_ann']:.0%}"
                        ),
                        meta={"rank": rank, "score": score, **meta},
                    )
                )
        return signals

    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        raise NotImplementedError("順位付けは on_bars で銘柄横断に行う")

    # -- スクリーニング（screen コマンドからも使う） ------------------------

    def screen(
        self, frame: pl.DataFrame, benchmark_mom: float | None, market_ok: bool
    ) -> RankResult:
        latest = frame.row(-1, named=True)
        close = latest["close"]
        mom_mid = latest["mom_mid"]
        mom_long = latest["mom_long"]
        vol = latest["vol_ann"]
        trend = latest[f"sma_{self.trend_sma}"]
        atr_value = latest[f"atr_{self.atr_period}"]
        dollar_volume = latest["dollar_volume"]

        if any(v is None for v in (close, mom_mid, mom_long, vol, trend, atr_value, dollar_volume)):
            return RankResult(failed=["指標が未計算"], close=close or 0.0)

        failed = []
        if close < self.min_price:
            failed.append(f"株価 {close:.2f} が下限 {self.min_price:g} 未満")
        if dollar_volume < self.min_dollar_volume:
            failed.append(f"売買代金 {dollar_volume:,.0f} が下限未満")
        atr_ratio = atr_value / close
        if atr_ratio > self.max_atr_ratio:
            failed.append(f"ATR比 {atr_ratio:.1%} が上限 {self.max_atr_ratio:.0%} 超")
        if close <= trend:
            failed.append(f"終値が SMA{self.trend_sma} 以下")
        if mom_mid <= 0:
            failed.append(f"6M リターン {mom_mid:+.1%} がマイナス")
        if mom_long <= 0:
            failed.append(f"12M リターン {mom_long:+.1%} がマイナス")
        if self.benchmark and (benchmark_mom is None or mom_mid <= benchmark_mom):
            failed.append(f"6M リターンが {self.benchmark} を下回る")
        if not market_ok:
            failed.append(f"地合い: {self.benchmark} が SMA{self.benchmark_sma} 割れ")

        score = mom_mid / vol if vol and vol > 0 else 0.0
        return RankResult(
            failed=failed,
            close=close,
            score=score,
            mom_mid=mom_mid,
            mom_long=mom_long,
            vol_ann=vol,
            atr_ratio=atr_ratio,
            dollar_volume=dollar_volume,
        )

    # -- 内部 ---------------------------------------------------------------

    def _benchmark_state(
        self, ctx: StrategyContext, frames: dict[str, pl.DataFrame]
    ) -> tuple[bool, float | None]:
        """(地合いが良いか, ベンチマークの 6M リターン)。"""
        if not self.benchmark:
            return True, None
        benchmark = frames.get(self.benchmark)
        if benchmark is None:
            # 地合いを判断できないなら建てない（黙って通さない）
            return False, None
        col = f"sma_{self.benchmark_sma}"
        if col not in benchmark.columns:
            benchmark = benchmark.with_columns(sma(self.benchmark_sma))
        latest = benchmark.row(-1, named=True)
        value = latest[col]
        ok = value is not None and latest["close"] > value
        return bool(ok), latest.get("mom_mid")

    def _is_rebalance_day(self, frames: dict[str, pl.DataFrame]) -> bool:
        if self.rebalance == "daily":
            return True
        frame = next(iter(frames.values()), None)
        if frame is None or frame.height < 2:
            return False
        today, prev = frame["date"][-1], frame["date"][-2]
        if self.rebalance == "monthly":
            return bool((today.year, today.month) != (prev.year, prev.month))
        return bool(today.isocalendar()[1] != prev.isocalendar()[1])  # weekly

    def describe(self) -> str:
        return (
            f"{self.name}(mom={self.lookback}-{self.skip}, sma={self.trend_sma}, "
            f"rebalance={self.rebalance}, top={self.top_n}, benchmark={self.benchmark})"
        )


class RankResult:
    """1銘柄のスクリーニングと順位付けの材料。"""

    __slots__ = (
        "atr_ratio",
        "close",
        "dollar_volume",
        "failed",
        "mom_long",
        "mom_mid",
        "score",
        "vol_ann",
    )

    def __init__(
        self,
        *,
        failed: list[str],
        close: float,
        score: float = 0.0,
        mom_mid: float = 0.0,
        mom_long: float = 0.0,
        vol_ann: float = 0.0,
        atr_ratio: float = 0.0,
        dollar_volume: float = 0.0,
    ) -> None:
        self.failed = failed
        self.close = close
        self.score = score
        self.mom_mid = mom_mid
        self.mom_long = mom_long
        self.vol_ann = vol_ann
        self.atr_ratio = atr_ratio
        self.dollar_volume = dollar_volume

    @property
    def passed(self) -> bool:
        return not self.failed

    def as_meta(self) -> dict[str, Any]:
        return {
            "close": self.close,
            "mom_mid": self.mom_mid,
            "mom_long": self.mom_long,
            "vol_ann": self.vol_ann,
            "atr_ratio": self.atr_ratio,
            "dollar_volume": self.dollar_volume,
        }
