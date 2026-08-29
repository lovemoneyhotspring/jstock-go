"""戦略の登録簿。売買型と積立型を**同じ名前空間**で扱う。

設定ファイルの名前から戦略クラスを引く。新しい戦略を足すときは
:func:`register` するか、``samples`` / :mod:`wbjp.accumulate.tactics` に
置いて ``_BUILTIN`` に追記する。

**なぜ1つの登録簿なのか**

売買型（:class:`~wbjp.strategy.base.Strategy`）と積立型
（:class:`~wbjp.accumulate.tactics.Tactic`）は判断の出力が違うので実行
経路は分かれているが、どちらも「取引に使う戦略」であることに変わりはない。
登録簿を分けると ``wbjp strategies`` のような一覧から片方が丸ごと漏れ、
存在を忘れる。名前空間を1つにすることで、同名の衝突も定義時に弾ける。

種別の取り違えは :func:`create_strategy` / :func:`create_tactic` が
「``[[tactics]]`` に書いてください」と具体的に指摘する。
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import TYPE_CHECKING, Any, Protocol

from wbjp.accumulate.tactics import BearStack, Constant, DrawdownLadder, StackLadder, Tactic
from wbjp.strategy.base import Playbook, PlaybookKind, Strategy
from wbjp.strategy.samples.atr_breakout import AtrBreakoutStrategy
from wbjp.strategy.samples.momentum_rank import MomentumRankStrategy
from wbjp.strategy.samples.ross_cameron import RossCameronStrategy
from wbjp.strategy.samples.rsi_pullback import RsiPullbackStrategy
from wbjp.strategy.samples.rsi_reversion import RsiReversionStrategy
from wbjp.strategy.samples.sma_cross import SmaCrossStrategy
from wbjp.strategy.samples.trend_pullback import TrendPullbackStrategy

if TYPE_CHECKING:
    from wbjp.config import StrategyEntry

_REGISTRY: dict[str, type[Playbook]] = {}


class _Entry(Protocol):
    """設定ファイルの1行。登録簿が必要とするのは名前とパラメータだけ。"""

    name: str

    @property
    def params(self) -> dict[str, Any]: ...


def register(playbook_cls: type[Playbook]) -> type[Playbook]:
    """戦略クラスを登録する。デコレータとしても使える。

    Raises:
        ValueError: 名前が未設定、または既に登録済みのとき。
    """
    if not playbook_cls.name:
        raise ValueError(f"{playbook_cls.__name__} に name が設定されていません")
    existing = _REGISTRY.get(playbook_cls.name)
    if existing is not None and existing is not playbook_cls:
        raise ValueError(
            f"戦略名 {playbook_cls.name!r} は既に {existing.__name__} が使用しています"
        )
    _REGISTRY[playbook_cls.name] = playbook_cls
    return playbook_cls


def available(kind: PlaybookKind | str | None = None) -> list[str]:
    """登録済みの戦略名。``kind`` を渡すとその種別だけに絞る。"""
    if kind is None:
        return sorted(_REGISTRY)
    wanted = PlaybookKind(kind)
    return sorted(name for name, cls in _REGISTRY.items() if cls.kind is wanted)


def catalog() -> list[type[Playbook]]:
    """登録済みの戦略クラス。種別ごとに名前順。一覧表示用。"""
    return sorted(_REGISTRY.values(), key=lambda cls: (cls.kind.value, cls.name))


def get(name: str) -> type[Playbook]:
    """名前から戦略クラスを引く。

    Raises:
        ValueError: 未知の名前のとき。
    """
    try:
        return _REGISTRY[name]
    except KeyError:
        raise ValueError(f"未知の戦略 {name!r}。利用可能: {available()}") from None


def create(name: str, params: dict[str, Any] | None = None) -> Playbook:
    """名前とパラメータから戦略を生成する。

    Raises:
        ValueError: 未知の戦略、またはパラメータが受け付けられないとき。
    """
    playbook_cls = get(name)
    try:
        return playbook_cls(**(params or {}))
    except TypeError as exc:
        raise ValueError(f"戦略 {name!r} のパラメータが不正です: {exc}") from exc


def _create_as(name: str, params: dict[str, Any] | None, kind: PlaybookKind) -> Playbook:
    """種別を検証しつつ生成する。取り違えは書くべき場所を添えて弾く。"""
    found = get(name)
    if found.kind is not kind:
        section = "[[tactics]]" if found.kind is PlaybookKind.ACCUMULATE else "[[strategies]]"
        raise ValueError(
            f"{name!r} は{found.kind.label}型の戦略です。{section} に書いてください"
            f"（{kind.label}型として使える戦略: {available(kind)}）"
        )
    return create(name, params)


def create_strategy(name: str, params: dict[str, Any] | None = None) -> Strategy:
    """売買型（``signal``）として生成する。種別が違えば ValueError。"""
    return _create_as(name, params, PlaybookKind.SIGNAL)  # type: ignore[return-value]


def create_tactic(name: str, params: dict[str, Any] | None = None) -> Tactic:
    """積立型（``accumulate``）として生成する。種別が違えば ValueError。"""
    return _create_as(name, params, PlaybookKind.ACCUMULATE)  # type: ignore[return-value]


def build_all(entries: Iterable[StrategyEntry | _Entry]) -> list[Strategy]:
    """設定ファイルの ``[[strategies]]`` から売買型戦略を組み立てる。"""
    return [create_strategy(entry.name, entry.params) for entry in entries]


_BUILTIN: tuple[type[Playbook], ...] = (
    # 売買型（signal）
    SmaCrossStrategy,
    RsiReversionStrategy,
    AtrBreakoutStrategy,
    TrendPullbackStrategy,
    MomentumRankStrategy,
    RsiPullbackStrategy,
    RossCameronStrategy,
    # 積立型（accumulate）
    Constant,
    BearStack,
    StackLadder,
    DrawdownLadder,
)

for _cls in _BUILTIN:
    register(_cls)
