"""戦略の基底。

設計方針:
    戦略は **注文を出さない**。売買の意見（:class:`~wbjp.domain.models.Signal`）を
    返すだけにする。注文への変換、数量の決定、リスク判定はすべてエンジン側の
    責務とする。

    こうすると戦略は I/O もブローカーも時計も知らない純粋な関数に近づき、
    モックなしで単体テストできる。バックテストと本番で同じコードが動く
    のも、この分離があってこそ。

先読みバイアスについて:
    :class:`StrategyContext` が渡す足は、判断時点までに**切り詰め済み**。
    戦略が未来の足を見る手段がそもそも存在しない。「規律で気をつける」
    のではなく、構造で不可能にしている。

戦略の2つの種別:
    どちらも「取引に使う戦略」だが、判断の出力が違う。共通の親は
    :class:`Playbook` で、登録簿（:mod:`wbjp.strategy.registry`）と
    設定ファイル（``strategies.toml``）はこの親を単位に一本化してある。

    ``signal``
        :class:`Strategy`。銘柄ごとに -1.0〜+1.0 の意見を返す。合成器が
        1本にまとめ、サイジングが目標株数へ変換する。売りも損切りもする。
    ``accumulate``
        :class:`~wbjp.accumulate.tactics.Tactic`。その日の購入倍率を返す。
        目標建玉を持たず、予算 × 倍率で積み増すだけで売却しない。

    出力の意味が違うので実行経路は分けてある（倍率を意見として合成しても
    意味を成さない）。共有するのは名前・登録・設定・一覧の面。
"""

from __future__ import annotations

import datetime as dt
import sys
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from decimal import Decimal
from enum import StrEnum
from typing import Any, ClassVar

import polars as pl

from wbjp.domain.models import Position, Signal


class InsufficientBarsError(RuntimeError):
    """ウォームアップに必要な本数の足が無い。"""


class PlaybookKind(StrEnum):
    """戦略の種別。判断の出力が何かを表す。"""

    #: 銘柄ごとの売買意見（-1.0〜+1.0）を返す。合成 → サイジング → 発注。
    SIGNAL = "signal"
    #: その日の購入倍率を返す。予算 × 倍率で積み増す。
    ACCUMULATE = "accumulate"

    @property
    def label(self) -> str:
        return "売買" if self is PlaybookKind.SIGNAL else "積立"


class Playbook(ABC):
    """取引に使う戦略すべての共通の親。

    共通なのは「設定ファイルから名前で引ける」ことだけ。判断の中身は
    :class:`Strategy`（意見）と :class:`~wbjp.accumulate.tactics.Tactic`
    （倍率）でまったく違う。それでも共通の親を置くのは、登録簿・設定・
    一覧表示をひとつに保ち、``bear_stack`` のような戦略が
    「別モジュールにあるせいで一覧から漏れる」事故を防ぐため。

    中間の抽象クラスは ``class Foo(Playbook, abstract=True)`` と明示する。
    ``__init_subclass__`` は ABCMeta が ``__abstractmethods__`` を設定する
    **前**に呼ばれるため、抽象かどうかを自動判定できない。
    """

    #: 設定ファイルで指定する識別子。サブクラスで必ず上書きする。
    #: 種別をまたいで一意（``[[strategies]]`` と ``[[tactics]]`` で同名は不可）。
    name: ClassVar[str] = ""

    #: 種別。サブクラスの系統ごとに一度定義すればよい。
    kind: ClassVar[PlaybookKind]

    def __init_subclass__(cls, *, abstract: bool = False, **kwargs: Any) -> None:
        """``name`` と ``kind`` の付け忘れを定義時点で弾く。"""
        super().__init_subclass__(**kwargs)
        if abstract:
            return
        if not cls.name:
            raise TypeError(f"{cls.__name__} はクラス変数 name を定義してください")
        if getattr(cls, "kind", None) is None:
            raise TypeError(f"{cls.__name__} はクラス変数 kind を定義してください")

    def describe(self) -> str:
        """ログ・一覧用の説明。パラメータを含めると調査が楽になる。"""
        return self.name

    @classmethod
    def summary(cls) -> str:
        """1行の説明。戦略一覧の説明欄に使う。

        クラスに docstring が無ければモジュールのものを使う。売買型は
        「1モジュール1戦略」で説明をモジュール冒頭に置く慣習のため。
        """
        doc = (cls.__doc__ or "").strip()
        if not doc:
            module = sys.modules.get(cls.__module__)
            doc = (getattr(module, "__doc__", None) or "").strip()
        if not doc:
            return ""
        # docstring は ReST。一覧は素のテキストなので装飾記号を落とす。
        return doc.split("\n", 1)[0].replace("``", "").replace("**", "")


@dataclass(frozen=True, slots=True)
class StrategyContext:
    """戦略に渡す判断材料。

    ``bars`` には ``as_of`` 以前の足しか入っていない。エンジンが構築時に
    切り詰めているため、戦略側で日付を確認する必要はない。

    Attributes:
        as_of: 判断の基準日。この日の足まで（当日を含む）が見える。
        equity: 判断時点の総資産（円）。サイジングには使わないが、
            戦略が資産規模に応じた判断をしたい場合に参照できる。
    """

    as_of: dt.date
    _bars: dict[str, pl.DataFrame]
    _positions: dict[str, Position] = field(default_factory=dict)
    equity: Decimal = Decimal(0)

    @property
    def symbols(self) -> list[str]:
        """足が用意されている銘柄。"""
        return sorted(self._bars)

    def bars(self, symbol: str) -> pl.DataFrame:
        """``symbol`` の日足。``date`` 昇順で、``as_of`` までに限られる。

        列: ``date`` / ``open`` / ``high`` / ``low`` / ``close`` / ``volume``

        Raises:
            KeyError: その銘柄の足が無いとき。
        """
        try:
            return self._bars[symbol]
        except KeyError:
            raise KeyError(
                f"{symbol} の足がコンテキストにありません。利用可能: {self.symbols}"
            ) from None

    def has_bars(self, symbol: str, minimum: int = 1) -> bool:
        """``minimum`` 本以上の足があるか。"""
        return symbol in self._bars and self._bars[symbol].height >= minimum

    def latest(self, symbol: str) -> dict[str, Any]:
        """最新の足を辞書で返す。

        Raises:
            InsufficientBarsError: 足が1本も無いとき。
        """
        frame = self.bars(symbol)
        if frame.height == 0:
            raise InsufficientBarsError(f"{symbol}: 足が1本もありません")
        return frame.row(-1, named=True)

    def close(self, symbol: str) -> Decimal:
        """最新の終値。"""
        return Decimal(str(self.latest(symbol)["close"]))

    def position(self, symbol: str) -> Position | None:
        """現在の建玉。無ければ None。"""
        return self._positions.get(symbol)

    def has_position(self, symbol: str) -> bool:
        position = self._positions.get(symbol)
        return position is not None and position.quantity > 0

    @property
    def held_symbols(self) -> list[str]:
        return sorted(s for s, p in self._positions.items() if p.quantity > 0)


class Strategy(Playbook, abstract=True):
    """売買の意見を返す戦略の基底クラス（種別 ``signal``）。

    実装するのは :meth:`on_bars` だけ。``name`` は設定ファイルから
    参照する識別子になるので、クラス変数で必ず定義する。

    Example:
        >>> class AlwaysBuy(Strategy):
        ...     name = "always_buy"
        ...     warmup_bars = 1
        ...
        ...     def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        ...         return [
        ...             Signal(self.name, symbol, direction=1.0, reason="常に買い")
        ...             for symbol in ctx.symbols
        ...         ]
    """

    kind: ClassVar[PlaybookKind] = PlaybookKind.SIGNAL

    #: 判断に必要な過去の足の本数。エンジンがこの本数を用意してから呼ぶ。
    #: 足りない銘柄はスキップされる（例外にはしない）。
    #: パラメータ次第で変わるため、__init__ で上書きしてよい
    #: （ClassVar にすると期間を引数で受ける戦略が書けなくなる）。
    warmup_bars: int = 1

    @abstractmethod
    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        """判断を行い、売買の意見を返す。

        意見が無い銘柄については **Signal を返さない**。
        ``direction=0`` を返すのとは意味が違う: 前者は「何も言わない」、
        後者は「中立だと積極的に主張する」であり、合成時の扱いが変わる。

        Returns:
            意見のある銘柄ぶんの Signal。空リストでよい。
        """

    def __repr__(self) -> str:
        return f"<{type(self).__name__} name={self.name!r} warmup={self.warmup_bars}>"


class IndicatorStrategy(Strategy, abstract=True):
    """指標を計算してから銘柄ごとに判断する戦略のひな形。

    ウォームアップ不足の銘柄を飛ばす処理と、指標計算の当てはめを
    共通化する。個々の戦略は :meth:`indicators` と :meth:`evaluate`
    を書くだけでよい。
    """

    def indicators(self) -> list[pl.Expr]:
        """足に付与する指標の式。既定では何も足さない。"""
        return []

    def indicator_names(self) -> list[str]:
        """:meth:`indicators` が生成する列名。

        エンジンが指標を先回りして計算しておくために使う。
        """
        return [expr.meta.output_name() for expr in self.indicators()]

    @abstractmethod
    def evaluate(self, symbol: str, frame: pl.DataFrame, ctx: StrategyContext) -> Signal | None:
        """1銘柄ぶんの判断。意見が無ければ None を返す。

        Args:
            frame: :meth:`indicators` の列が付与済みの足。
        """

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        expressions = self.indicators()
        names = self.indicator_names()
        signals: list[Signal] = []

        for symbol in ctx.symbols:
            if not ctx.has_bars(symbol, self.warmup_bars):
                continue

            frame = ctx.bars(symbol)
            # エンジンが先回りして計算済みなら再計算しない。
            # バックテストでは日数ぶん繰り返し呼ばれるため、ここを毎回
            # 計算すると日数の二乗に比例して遅くなる。
            if expressions and not set(names).issubset(frame.columns):
                frame = frame.with_columns(expressions)

            signal = self.evaluate(symbol, frame, ctx)
            if signal is not None:
                signals.append(signal)

        return signals
