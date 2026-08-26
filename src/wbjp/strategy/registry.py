"""戦略の登録簿。

設定ファイルの名前から戦略クラスを引く。新しい戦略を足すときは
:func:`register` するか、``samples`` に置いて ``_BUILTIN`` に追記する。
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from wbjp.config import StrategyEntry
from wbjp.strategy.base import Strategy
from wbjp.strategy.samples.atr_breakout import AtrBreakoutStrategy
from wbjp.strategy.samples.momentum_rank import MomentumRankStrategy
from wbjp.strategy.samples.rsi_reversion import RsiReversionStrategy
from wbjp.strategy.samples.sma_cross import SmaCrossStrategy
from wbjp.strategy.samples.trend_pullback import TrendPullbackStrategy

_REGISTRY: dict[str, type[Strategy]] = {}


def register(strategy_cls: type[Strategy]) -> type[Strategy]:
    """戦略クラスを登録する。デコレータとしても使える。

    Raises:
        ValueError: 名前が未設定、または既に登録済みのとき。
    """
    if not strategy_cls.name:
        raise ValueError(f"{strategy_cls.__name__} に name が設定されていません")
    existing = _REGISTRY.get(strategy_cls.name)
    if existing is not None and existing is not strategy_cls:
        raise ValueError(
            f"戦略名 {strategy_cls.name!r} は既に {existing.__name__} が使用しています"
        )
    _REGISTRY[strategy_cls.name] = strategy_cls
    return strategy_cls


def available() -> list[str]:
    """登録済みの戦略名。"""
    return sorted(_REGISTRY)


def get(name: str) -> type[Strategy]:
    """名前から戦略クラスを引く。

    Raises:
        ValueError: 未知の名前のとき。
    """
    try:
        return _REGISTRY[name]
    except KeyError:
        raise ValueError(f"未知の戦略 {name!r}。利用可能: {available()}") from None


def create(name: str, params: dict[str, Any] | None = None) -> Strategy:
    """名前とパラメータから戦略を生成する。

    Raises:
        ValueError: 未知の戦略、またはパラメータが受け付けられないとき。
    """
    strategy_cls = get(name)
    try:
        return strategy_cls(**(params or {}))
    except TypeError as exc:
        raise ValueError(f"戦略 {name!r} のパラメータが不正です: {exc}") from exc


def build_all(entries: Iterable[StrategyEntry]) -> list[Strategy]:
    """設定ファイルの記述から戦略を組み立てる。"""
    return [create(entry.name, entry.params) for entry in entries]


_BUILTIN: tuple[type[Strategy], ...] = (
    SmaCrossStrategy,
    RsiReversionStrategy,
    AtrBreakoutStrategy,
    TrendPullbackStrategy,
    MomentumRankStrategy,
)

for _cls in _BUILTIN:
    register(_cls)
