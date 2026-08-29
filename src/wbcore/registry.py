"""名前でクラスを引く登録簿。

設定ファイルに書いた名前（``"sma_cross"`` / ``"bear_stack"``）から
クラスを引き、パラメータを渡して生成する。戦略の種類が違っても
この部分は同じなので、共通の部品にしてある。

使い方::

    class Strategy:
        name: ClassVar[str] = ""

    STRATEGIES = Registry[Strategy]("戦略")

    @STRATEGIES.register
    class SmaCross(Strategy):
        name = "sma_cross"

    STRATEGIES.create("sma_cross", {"fast": 25, "slow": 75})
"""

from __future__ import annotations

import sys
from typing import Any, ClassVar, Protocol


class Named(Protocol):
    """登録できるクラスの条件。クラス変数 ``name`` を持つこと。"""

    name: ClassVar[str]


class Registry[T: Named]:
    """``name`` → クラス の対応表。

    Args:
        label: エラーメッセージで対象を指す語（「戦略」など）。
    """

    def __init__(self, label: str) -> None:
        self.label = label
        self._items: dict[str, type[T]] = {}

    def register(self, cls: type[T]) -> type[T]:
        """クラスを登録する。デコレータとしても使える。

        Raises:
            ValueError: 名前が未設定、または別のクラスが同名で登録済みのとき。
        """
        if not cls.name:
            raise ValueError(f"{cls.__name__} に name が設定されていません")
        existing = self._items.get(cls.name)
        if existing is not None and existing is not cls:
            raise ValueError(
                f"{self.label}名 {cls.name!r} は既に {existing.__name__} が使用しています"
            )
        self._items[cls.name] = cls
        return cls

    def available(self) -> list[str]:
        """登録済みの名前。"""
        return sorted(self._items)

    def classes(self) -> list[type[T]]:
        """登録済みのクラス。名前順。"""
        return [self._items[name] for name in self.available()]

    def get(self, name: str) -> type[T]:
        """名前からクラスを引く。

        Raises:
            ValueError: 未知の名前のとき。候補を添える。
        """
        try:
            return self._items[name]
        except KeyError:
            raise ValueError(f"未知の{self.label} {name!r}。利用可能: {self.available()}") from None

    def create(self, name: str, params: dict[str, Any] | None = None) -> T:
        """名前とパラメータからインスタンスを作る。

        Raises:
            ValueError: 未知の名前、またはパラメータが受け付けられないとき。
        """
        cls = self.get(name)
        try:
            return cls(**(params or {}))
        except TypeError as exc:
            raise ValueError(f"{self.label} {name!r} のパラメータが不正です: {exc}") from exc

    def __contains__(self, name: object) -> bool:
        return name in self._items

    def __len__(self) -> int:
        return len(self._items)


def summary_of(cls: type[Any]) -> str:
    """クラスの1行説明。一覧表示に使う。

    クラスに docstring が無ければモジュールのものを使う。「1モジュール1戦略」
    で説明をモジュール冒頭に置く慣習のため。ReST の装飾記号は落とす。
    """
    doc = (cls.__doc__ or "").strip()
    if not doc:
        module = sys.modules.get(cls.__module__)
        doc = (getattr(module, "__doc__", None) or "").strip()
    if not doc:
        return ""
    return doc.split("\n", 1)[0].replace("``", "").replace("**", "")
