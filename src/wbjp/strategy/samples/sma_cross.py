"""移動平均クロス戦略（順張り）。

短期移動平均が長期移動平均を上抜けたら買い、下抜けたら売り。
最も古典的なトレンドフォロー。

性格: トレンドが続く相場で強く、横ばいでは往復ビンタを食らう。
逆張り戦略と組み合わせると相互に弱点を補える。
"""

from __future__ import annotations

from typing import ClassVar

import polars as pl

from wbcore.domain.models import Signal
from wbcore.indicators.ohlcv import sma
from wbjp.strategy.base import IndicatorStrategy, StrategyContext


class SmaCrossStrategy(IndicatorStrategy):
    name: ClassVar[str] = "sma_cross"

    def __init__(self, fast: int = 25, slow: int = 75) -> None:
        if fast >= slow:
            raise ValueError(f"fast は slow より小さく: fast={fast}, slow={slow}")
        self.fast = fast
        self.slow = slow
        # 長期線が確定した翌日から前日比較ができるので +1 本
        self.warmup_bars = slow + 1

    def indicators(self) -> list[pl.Expr]:
        return [sma(self.fast), sma(self.slow)]

    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        fast_col, slow_col = f"sma_{self.fast}", f"sma_{self.slow}"
        recent = frame.tail(2)
        if recent.height < 2:
            return None

        prev, current = recent.row(0, named=True), recent.row(1, named=True)
        if any(row[col] is None for row in (prev, current) for col in (fast_col, slow_col)):
            return None

        prev_diff = prev[fast_col] - prev[slow_col]
        current_diff = current[fast_col] - current[slow_col]

        # 乖離率を確信度に使う。離れているほどトレンドが明確とみなす。
        confidence = min(1.0, abs(current_diff) / current[slow_col] * 20.0)

        if prev_diff <= 0 < current_diff:
            return Signal(
                self.name,
                symbol,
                direction=1.0,
                confidence=max(confidence, 0.3),
                reason=f"{self.fast}日線が{self.slow}日線を上抜け",
                meta={"fast": current[fast_col], "slow": current[slow_col]},
            )
        if prev_diff >= 0 > current_diff:
            return Signal(
                self.name,
                symbol,
                direction=-1.0,
                confidence=max(confidence, 0.3),
                reason=f"{self.fast}日線が{self.slow}日線を下抜け",
                meta={"fast": current[fast_col], "slow": current[slow_col]},
            )

        # クロスしていない間も、どちら側にいるかは意見として出す。
        # これが無いと、クロスした当日しか保有を維持できない。
        direction = 1.0 if current_diff > 0 else -1.0
        return Signal(
            self.name,
            symbol,
            direction=direction,
            confidence=confidence * 0.5,
            reason=f"{self.fast}日線が{self.slow}日線の{'上' if direction > 0 else '下'}で推移",
            meta={"fast": current[fast_col], "slow": current[slow_col]},
        )

    def describe(self) -> str:
        return f"{self.name}(fast={self.fast}, slow={self.slow})"
