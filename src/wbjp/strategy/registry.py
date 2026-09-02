"""戦略の登録簿。

設定ファイルの名前から戦略クラスを引く。新しい戦略を足すときは
:func:`register` するか、``samples`` に置いて ``_BUILTIN`` に追記する。
登録簿の仕組みそのものは :class:`wbcore.registry.Registry`（積立の
:mod:`accum.registry` も同じ部品を使う）。
"""

from __future__ import annotations

from collections.abc import Iterable

from wbcore.registry import Registry
from wbjp.config import StrategyEntry
from wbjp.strategy.base import Strategy
from wbjp.strategy.samples.atr_breakout import AtrBreakoutStrategy
from wbjp.strategy.samples.momentum_rank import MomentumRankStrategy
from wbjp.strategy.samples.ross_cameron import RossCameronStrategy
from wbjp.strategy.samples.rsi_pullback import RsiPullbackStrategy
from wbjp.strategy.samples.rsi_reversion import RsiReversionStrategy
from wbjp.strategy.samples.sma_cross import SmaCrossStrategy
from wbjp.strategy.samples.trend_pullback import TrendPullbackStrategy

STRATEGIES = Registry[Strategy]("戦略")

register = STRATEGIES.register
available = STRATEGIES.available
get = STRATEGIES.get
create = STRATEGIES.create


def build_all(entries: Iterable[StrategyEntry]) -> list[Strategy]:
    """設定ファイルの記述から戦略を組み立てる。"""
    return [create(entry.name, entry.params) for entry in entries]


_BUILTIN: tuple[type[Strategy], ...] = (
    SmaCrossStrategy,
    RsiReversionStrategy,
    AtrBreakoutStrategy,
    TrendPullbackStrategy,
    MomentumRankStrategy,
    RsiPullbackStrategy,
    RossCameronStrategy,
)

for _cls in _BUILTIN:
    register(_cls)
