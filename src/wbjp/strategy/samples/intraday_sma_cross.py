"""日中足の移動平均クロス（順張り）。窓を時間で持ち、取引時間帯と引け前を扱う。

:class:`~wbjp.strategy.samples.sma_cross.SmaCrossStrategy` の日中版。
違いは3つで、どれも分足で運用するときに必ず要るもの:

1. **窓を時間で指定する。** ``fast = "15m"`` / ``slow = "1h"``。足の間隔が
   5 分なら 3 本と 12 本、1 分なら 15 本と 60 本に :meth:`bind` で直す。
   本数で書くと、間隔を変えた瞬間に意味が変わる。
2. **取引時間帯の外では建てない。** ``session = { start = "09:30", end = "14:30" }``
   （取引所の現地時刻）。寄付直後の荒れた足と、引け間際を避ける。
   保有中の銘柄には時間帯の外でも意見を出す（手仕舞いの判断が止まらないように）。
3. **引け前に手仕舞う。** ``flat_before = "15:00"`` 以降は保有をすべて売る。
   翌日に持ち越すなら空文字にする。

指標の意味（本数 → 時間）を戦略が自分で管理するので、同じ設定を
5 分足でも 1 分足でも使える。
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any, ClassVar

import polars as pl

from wbcore.data.provider import Interval, parse_duration
from wbcore.domain.models import Market, Signal
from wbcore.domain.session import SessionWindow, parse_time
from wbcore.indicators.ohlcv import sma
from wbjp.strategy.base import IndicatorStrategy, StrategyContext


class IntradaySmaCrossStrategy(IndicatorStrategy):
    name: ClassVar[str] = "intraday_sma_cross"
    intervals: ClassVar[frozenset[Interval]] = frozenset(i for i in Interval if i.is_intraday)

    def __init__(
        self,
        fast: str = "15m",
        slow: str = "1h",
        market: str | Market = Market.JP,
        session: Mapping[str, Any] | None = None,
        flat_before: str = "15:00",
    ) -> None:
        self.fast_span = parse_duration(fast)
        self.slow_span = parse_duration(slow)
        if self.fast_span >= self.slow_span:
            raise ValueError(f"fast は slow より短く: fast={fast}, slow={slow}")
        self.market = Market(market)
        self.session = SessionWindow.parse(session, self.market)
        self.flat_before = parse_time(flat_before, "flat_before") if flat_before else None
        # 本数は足の間隔が決まるまで確定しない
        self.fast: int | None = None
        self.slow: int | None = None

    def bind(self, interval: Interval) -> None:
        self.fast = interval.bars_in(self.fast_span)
        self.slow = interval.bars_in(self.slow_span)
        if self.fast >= self.slow:
            raise ValueError(
                f"{interval.value} 足では fast={self.fast} 本と slow={self.slow} 本が逆転または同じです"
            )
        self.warmup_bars = self.slow + 1

    def indicators(self) -> list[pl.Expr]:
        if self.fast is None or self.slow is None:
            raise RuntimeError(f"{self.name}: bind() が呼ばれていません（足の間隔が未確定）")
        return [sma(self.fast), sma(self.slow)]

    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        assert self.fast is not None and self.slow is not None
        if ctx.at is None:
            return None  # 日足で呼ばれた（intervals で弾かれるはずだが念のため）
        holding = ctx.has_position(symbol)
        local = ctx.at.astimezone(self.market.timezone)

        # 引け前: 持っていれば手仕舞い、持っていなければ何もしない
        if self.flat_before is not None and local.time() >= self.flat_before:
            if holding:
                return Signal(
                    self.name,
                    symbol,
                    direction=-1.0,
                    reason=f"{self.flat_before:%H:%M} 以降は持ち越さない",
                )
            return None

        fast_col, slow_col = f"sma_{self.fast}", f"sma_{self.slow}"
        recent = frame.tail(2)
        if recent.height < 2:
            return None
        prev, current = recent.row(0, named=True), recent.row(1, named=True)
        if any(row[col] is None for row in (prev, current) for col in (fast_col, slow_col)):
            return None

        prev_diff = prev[fast_col] - prev[slow_col]
        current_diff = current[fast_col] - current[slow_col]
        confidence = min(1.0, abs(current_diff) / current[slow_col] * 50.0)
        label = f"{self.fast_span.seconds // 60}分線/{self.slow_span.seconds // 60}分線"

        # 取引時間帯の外では新規に建てない。保有中なら意見は出し続ける。
        in_session = self.session is None or self.session.allows(ctx.at)
        if not in_session and not holding:
            return None

        if prev_diff <= 0 < current_diff:
            return Signal(
                self.name, symbol, 1.0, max(confidence, 0.3), f"{label} 上抜け（{local:%H:%M}）"
            )
        if prev_diff >= 0 > current_diff:
            return Signal(
                self.name, symbol, -1.0, max(confidence, 0.3), f"{label} 下抜け（{local:%H:%M}）"
            )
        direction = 1.0 if current_diff > 0 else -1.0
        return Signal(
            self.name,
            symbol,
            direction,
            confidence * 0.5,
            f"{label} の{'上' if direction > 0 else '下'}で推移",
        )

    def describe(self) -> str:
        bars = f" = {self.fast}/{self.slow} 本" if self.fast is not None else ""
        session = f", {self.session.describe()}" if self.session else ""
        flat = f", {self.flat_before:%H:%M} で手仕舞い" if self.flat_before else ""
        return f"{self.name}(fast={self.fast_span}, slow={self.slow_span}{bars}{session}{flat})"
