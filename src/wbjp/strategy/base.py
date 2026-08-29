"""戦略の基底。

設計方針:
    戦略は **注文を出さない**。売買の意見（:class:`~wbcore.domain.models.Signal`）を
    返すだけにする。注文への変換、数量の決定、リスク判定はすべてエンジン側の
    責務とする。

    こうすると戦略は I/O もブローカーも時計も知らない純粋な関数に近づき、
    モックなしで単体テストできる。バックテストと本番で同じコードが動く
    のも、この分離があってこそ。

先読みバイアスについて:
    :class:`StrategyContext` が渡す足は、判断時点までに**切り詰め済み**。
    戦略が未来の足を見る手段がそもそも存在しない。「規律で気をつける」
    のではなく、構造で不可能にしている。

積立との関係:
    積立（:mod:`accum`）は別プロジェクト。倍率を返す戦略は意見を返す
    戦略とは出力の意味が違い、合成器に混ぜても意味を成さないため、
    基底も登録簿も分けてある。共有するのは :mod:`wbcore` の部品
    （ブローカー・データ・指標・登録簿の仕組み）。
"""

from __future__ import annotations

import datetime as dt
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from decimal import Decimal
from typing import Any, ClassVar

import polars as pl

from wbcore.data.provider import Interval
from wbcore.domain.models import Market, Position, Signal


class InsufficientBarsError(RuntimeError):
    """ウォームアップに必要な本数の足が無い。"""


@dataclass(frozen=True, slots=True)
class StrategyContext:
    """戦略に渡す判断材料。

    ``bars`` には ``as_of`` 以前の足しか入っていない。エンジンが構築時に
    切り詰めているため、戦略側で日付を確認する必要はない。

    Attributes:
        as_of: 判断の基準日。この日の足まで（当日を含む）が見える。
        equity: 判断時点の総資産（円）。サイジングには使わないが、
            戦略が資産規模に応じた判断をしたい場合に参照できる。
        interval: 渡している足の間隔。日足なら ``Interval.D1``。
        at: 日中足のときの判断時刻（UTC、最後の足の時刻）。日足では None。
            取引所の現地時刻は ``at.astimezone(market.timezone)``。
        market: 取引所。:meth:`resample` の区切りを寄付に揃えるのに使う。
    """

    as_of: dt.date
    _bars: dict[str, pl.DataFrame]
    _positions: dict[str, Position] = field(default_factory=dict)
    equity: Decimal = Decimal(0)
    interval: Interval = Interval.D1
    at: dt.datetime | None = None
    market: Market = Market.JP

    def resample(
        self, symbol: str, interval: Interval, *, completed_only: bool = True
    ) -> pl.DataFrame:
        """見えている足から、より粗い間隔の足を合成して返す。

        5 分足で判断しながら 1 時間足や日足の指標を見たい（マルチタイム
        フレーム）ときに使う。合成は見えている足だけから行うので未来は
        混ざらないが、最後の足は形成中（区間の途中まで）になりうる。
        ``completed_only`` なら閉じた足だけを返す。

        Raises:
            ValueError: 今の足より細かい間隔を求めたとき。
        """
        from wbcore.data.resample import drop_forming, resample

        frame = resample(self.bars(symbol), self.interval, interval, market=self.market)
        if completed_only and interval is not self.interval:
            at = self.at or dt.datetime.combine(self.as_of, dt.time(23, 59), tzinfo=dt.UTC)
            frame = drop_forming(frame, interval, at, market=self.market)
        return frame

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


class Strategy(ABC):
    """売買戦略の基底クラス。

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

    #: 設定ファイルで指定する識別子。サブクラスで必ず上書きする。
    name: ClassVar[str] = ""

    #: 対応する足の間隔。既定はすべて（指標が「本数」で書かれていれば
    #: 間隔に依存しない）。日付の意味に依存する戦略（月次入れ替えなど）は
    #: ``frozenset({Interval.D1})`` に絞る。エンジンは設定の間隔に対応しない
    #: 戦略を起動時に弾く。
    intervals: ClassVar[frozenset[Interval]] = frozenset(Interval)

    #: 判断に必要な過去の足の本数。エンジンがこの本数を用意してから呼ぶ。
    #: 足りない銘柄はスキップされる（例外にはしない）。
    #: パラメータ次第で変わるため、__init__ で上書きしてよい
    #: （ClassVar にすると期間を引数で受ける戦略が書けなくなる）。
    warmup_bars: int = 1

    def supports(self, interval: Interval) -> bool:
        return interval in self.intervals

    def bind(self, interval: Interval) -> None:
        """足の間隔が決まったときにエンジンが呼ぶ。既定では何もしない。

        窓を時間で持つ戦略（「15分の移動平均」）はここで本数に直す
        （:meth:`Interval.bars_in`）。``indicators()`` より前に呼ばれる。
        """
        return None  # 抽象にはしない: 本数で書かれた戦略は上書き不要

    def __init_subclass__(cls, *, abstract: bool = False, **kwargs: Any) -> None:
        """``name`` の付け忘れを定義時点で弾く。

        ``__init_subclass__`` は ABCMeta が ``__abstractmethods__`` を
        設定する**前**に呼ばれるため、抽象クラスかどうかを自動判定できない。
        中間の抽象クラスは ``class Foo(Strategy, abstract=True)`` と
        明示的に申告する。
        """
        super().__init_subclass__(**kwargs)
        if not abstract and not cls.name:
            raise TypeError(f"{cls.__name__} はクラス変数 name を定義してください")

    def describe(self) -> str:
        """ログ用の説明。パラメータを含めると調査が楽になる。"""
        return self.name

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
