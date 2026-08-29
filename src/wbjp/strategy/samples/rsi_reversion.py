"""RSI 逆張り戦略。

RSI が売られすぎ圏に沈んだら買い、買われすぎ圏に浮いたら売り。

性格: レンジ相場で機能し、強いトレンドでは逆行し続けて損を膨らませる。
そこで ADX でトレンドの強さを測り、**強トレンド時は発言を控える**
フィルタを入れてある。順張り戦略との組み合わせを前提とした設計。
"""

from __future__ import annotations

from typing import ClassVar

import polars as pl

from wbcore.domain.models import Signal
from wbcore.indicators.ohlcv import adx, rsi
from wbjp.strategy.base import IndicatorStrategy, StrategyContext


class RsiReversionStrategy(IndicatorStrategy):
    name: ClassVar[str] = "rsi_reversion"

    def __init__(
        self,
        period: int = 14,
        oversold: float = 30.0,
        overbought: float = 70.0,
        adx_period: int = 14,
        max_adx: float = 40.0,
    ) -> None:
        if not 0 < oversold < overbought < 100:
            raise ValueError(
                f"0 < oversold < overbought < 100 を満たすこと: {oversold}, {overbought}"
            )
        self.period = period
        self.oversold = oversold
        self.overbought = overbought
        self.adx_period = adx_period
        self.max_adx = max_adx
        # ADX は内部で2段の Wilder 平滑化を行うため、その分の助走が要る
        self.warmup_bars = max(period, adx_period * 3) + 1

    def indicators(self) -> list[pl.Expr]:
        return [rsi(self.period), *adx(self.adx_period)]

    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        latest = frame.row(-1, named=True)
        rsi_value = latest[f"rsi_{self.period}"]
        adx_value = latest[f"adx_{self.adx_period}"]

        if rsi_value is None:
            return None

        # 強いトレンドが出ている間は逆張りしない。
        # 逆張り戦略が最も損をするのがこの局面なので、黙る判断を明示する。
        if adx_value is not None and adx_value > self.max_adx:
            return None

        if rsi_value <= self.oversold:
            depth = (self.oversold - rsi_value) / self.oversold
            return Signal(
                self.name,
                symbol,
                direction=1.0,
                confidence=min(1.0, 0.4 + depth),
                reason=f"RSI {rsi_value:.1f} が売られすぎ圏（≦{self.oversold:g}）",
                meta={"rsi": rsi_value, "adx": adx_value},
            )

        if rsi_value >= self.overbought:
            depth = (rsi_value - self.overbought) / (100 - self.overbought)
            return Signal(
                self.name,
                symbol,
                direction=-1.0,
                confidence=min(1.0, 0.4 + depth),
                reason=f"RSI {rsi_value:.1f} が買われすぎ圏（≧{self.overbought:g}）",
                meta={"rsi": rsi_value, "adx": adx_value},
            )

        return None

    def describe(self) -> str:
        return (
            f"{self.name}(period={self.period}, "
            f"oversold={self.oversold:g}, overbought={self.overbought:g})"
        )
