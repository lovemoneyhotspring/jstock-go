"""ATR フィルタ付きドンチャン・ブレイクアウト戦略。

過去 N 日の高値を上抜けたら買い、安値を下抜けたら売り。
ボラティリティが極端に小さい（＝だましになりやすい）局面は見送る。

性格: 大きな相場の初動を捉えるが、勝率は低く、少数の大きな勝ちで稼ぐ。
移動平均クロスとは似た方向を向きやすいが、反応の速さが異なる。

注意:
    ドンチャンの高値・安値は :func:`~wbcore.indicators.ohlcv.donchian_high`
    が **当日を除いて** 計算している。当日を含めると当日高値が必ず
    上限に等しくなり、ブレイクが常に成立する嘘のバックテスト結果になる。
"""

from __future__ import annotations

from typing import ClassVar

import polars as pl

from wbcore.domain.models import Signal
from wbcore.indicators.ohlcv import atr, donchian_high, donchian_low
from wbjp.strategy.base import IndicatorStrategy, StrategyContext


class AtrBreakoutStrategy(IndicatorStrategy):
    name: ClassVar[str] = "atr_breakout"

    def __init__(
        self,
        channel: int = 20,
        atr_period: int = 14,
        min_atr_ratio: float = 0.005,
    ) -> None:
        """
        Args:
            channel: ブレイク判定に使う期間。
            atr_period: ATR の期間。
            min_atr_ratio: ATR/終値 がこれ未満なら値動きが小さすぎると
                みなして見送る。だましを減らすためのフィルタ。
        """
        self.channel = channel
        self.atr_period = atr_period
        self.min_atr_ratio = min_atr_ratio
        self.warmup_bars = max(channel, atr_period) + 2

    def indicators(self) -> list[pl.Expr]:
        return [
            donchian_high(self.channel),
            donchian_low(self.channel),
            atr(self.atr_period),
        ]

    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        latest = frame.row(-1, named=True)
        upper = latest[f"donchian_high_{self.channel}"]
        lower = latest[f"donchian_low_{self.channel}"]
        atr_value = latest[f"atr_{self.atr_period}"]
        close = latest["close"]

        if None in (upper, lower, atr_value) or close <= 0:
            return None

        atr_ratio = atr_value / close
        if atr_ratio < self.min_atr_ratio:
            return None  # 値動きが乏しく、だましになりやすい

        # ボラティリティが大きいほどブレイクの信頼度を上げる（上限あり）
        confidence = min(1.0, 0.4 + atr_ratio * 20.0)

        if latest["high"] > upper:
            return Signal(
                self.name,
                symbol,
                direction=1.0,
                confidence=confidence,
                reason=f"{self.channel}日高値 {upper:.1f} を上抜け",
                meta={"upper": upper, "atr": atr_value, "atr_ratio": atr_ratio},
            )

        if latest["low"] < lower:
            return Signal(
                self.name,
                symbol,
                direction=-1.0,
                confidence=confidence,
                reason=f"{self.channel}日安値 {lower:.1f} を下抜け",
                meta={"lower": lower, "atr": atr_value, "atr_ratio": atr_ratio},
            )

        return None

    def describe(self) -> str:
        return f"{self.name}(channel={self.channel}, atr_period={self.atr_period})"
