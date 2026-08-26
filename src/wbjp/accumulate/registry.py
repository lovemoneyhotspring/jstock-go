"""積立戦術の登録簿。設定ファイルの名前から戦術を引く。"""

from __future__ import annotations

from typing import Any

from wbjp.accumulate.tactics import (
    BearStack,
    Constant,
    DrawdownLadder,
    StackLadder,
    Tactic,
)

_REGISTRY: dict[str, type[Tactic]] = {}


def register(tactic_cls: type[Tactic]) -> type[Tactic]:
    """戦術クラスを登録する。デコレータとしても使える。"""
    if not tactic_cls.name:
        raise ValueError(f"{tactic_cls.__name__} に name が設定されていません")
    existing = _REGISTRY.get(tactic_cls.name)
    if existing is not None and existing is not tactic_cls:
        raise ValueError(f"戦術名 {tactic_cls.name!r} は既に {existing.__name__} が使用しています")
    _REGISTRY[tactic_cls.name] = tactic_cls
    return tactic_cls


def available() -> list[str]:
    """登録済みの戦術名。"""
    return sorted(_REGISTRY)


def get(name: str) -> type[Tactic]:
    """名前から戦術クラスを引く。"""
    try:
        return _REGISTRY[name]
    except KeyError:
        raise ValueError(f"未知の戦術 {name!r}。利用可能: {available()}") from None


def create(name: str, params: dict[str, Any] | None = None) -> Tactic:
    """名前とパラメータから戦術を生成する。"""
    tactic_cls = get(name)
    try:
        return tactic_cls(**(params or {}))
    except TypeError as exc:
        raise ValueError(f"戦術 {name!r} のパラメータが不正です: {exc}") from exc


for _cls in (Constant, BearStack, StackLadder, DrawdownLadder):
    register(_cls)
