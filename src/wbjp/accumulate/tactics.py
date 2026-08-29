"""積立型の戦略。「その日の購入倍率をいくつにするか」だけを決める。

種別は ``accumulate``。売買型（:class:`wbjp.strategy.base.Strategy`）と
共通の親 :class:`~wbjp.strategy.base.Playbook` を持ち、同じ登録簿・同じ
設定ファイル（``strategies.toml`` の ``[[tactics]]``）から引ける。
違うのは判断の出力だけ——意見ではなく倍率を返し、売却しない。

**名前はメカニズム、強さはパラメータ**

``bear_stack_4x`` のような名前は作らない。倍率を変えた瞬間に名前が
嘘になるためで、これは売買型の ``sma_cross`` が期間を名前に含めて
いないのと同じ方針。

**倍率は 1 未満にしない**

上昇局面で倍率を下げると、投じられなかった資金が現金として滞留する。
検証では滞留日数と平均取得単価の悪化がほぼ完全な比例関係にあり
（約 0.068%/日）、倍率を 0.5 に落とすと平均取得単価が +5〜+30% 悪化した。
増額の原資は取り置きではなく新規資金を前提にする。
"""

from __future__ import annotations

from abc import abstractmethod
from collections.abc import Mapping, Sequence
from typing import ClassVar

import polars as pl

from wbjp.accumulate.stack import FAST, MID, SLOW, bear_stack, stack_score
from wbjp.accumulate.window import TradingWindow
from wbjp.indicators.ohlcv import sma
from wbjp.strategy.base import Playbook, PlaybookKind

#: 倍率列の名前。
MULTIPLIER = "multiplier"


class Tactic(Playbook, abstract=True):
    """購入倍率を決める積立型戦略の基底（種別 ``accumulate``）。

    実装するのは :meth:`multiplier` と :attr:`warmup_bars` だけ。
    足の取得も約定も知らない純粋な式なので、単体でテストできる。
    """

    kind: ClassVar[PlaybookKind] = PlaybookKind.ACCUMULATE

    def __init__(self, *, window: object = None) -> None:
        # 発注時間帯は倍率の決め方とは独立なので、全戦略で共通に持つ。
        # 既定は 14:00〜15:00（理由は wbjp.accumulate.window の説明）。
        self.window = TradingWindow.parse(window)

    @abstractmethod
    def multiplier(self) -> pl.Expr:
        """倍率列（float, 1.0 以上）を作る式。

        指標が確定しない期間は 1.0 を返すこと。null にすると
        ウォームアップ中に基本分の積立まで止まってしまう。
        """

    @property
    @abstractmethod
    def warmup_bars(self) -> int:
        """倍率が意味を持つまでに必要な足の本数。"""

    def allows_order(self, moment: object = None) -> bool:
        """その時刻に発注してよいか。投下額そのものには影響しない。"""
        return self.window.allows(moment)  # type: ignore[arg-type]

    def __repr__(self) -> str:
        return f"<{type(self).__name__} {self.describe()} 発注 {self.window.describe()}>"


class Constant(Tactic):
    """定額。常に 1 倍。純粋なドル平均法で、すべての比較の基準になる。"""

    name: ClassVar[str] = "constant"

    def multiplier(self) -> pl.Expr:
        return pl.lit(1.0).alias(MULTIPLIER)

    @property
    def warmup_bars(self) -> int:
        return 1


class BearStack(Tactic):
    """完全下降配列（``終値 < MA20 < MA50 < MA200``）のとき増額する。

    出現率は指数で 5〜13% と稀。追加投入1円あたりの単価改善が
    検証した中で最良で、必要な追加資金も最も少ない。
    """

    name: ClassVar[str] = "bear_stack"

    def __init__(
        self,
        multiplier: float = 4.0,
        fast: int = FAST,
        mid: int = MID,
        slow: int = SLOW,
        *,
        window: object = None,
    ) -> None:
        super().__init__(window=window)
        _check_multiplier(multiplier)
        self.value = float(multiplier)
        self.fast, self.mid, self.slow = fast, mid, slow

    def multiplier(self) -> pl.Expr:
        hit = bear_stack(self.fast, self.mid, self.slow).fill_null(value=False)
        return pl.when(hit).then(self.value).otherwise(1.0).alias(MULTIPLIER)

    @property
    def warmup_bars(self) -> int:
        return self.slow

    def describe(self) -> str:
        return f"{self.name}(×{self.value:g}, {self.fast}/{self.mid}/{self.slow})"


class StackLadder(Tactic):
    """弱気スコア（0〜6）の崩れ具合に応じて段階的に増額する。

    :class:`BearStack` より効果は大きいが、必要な追加資金も増え、
    1円あたりの効率は落ちる。``{6: 4.0}`` は BearStack と等価。
    """

    name: ClassVar[str] = "stack_ladder"

    #: 既定は検証した段階型。スコア3以上で1.5倍、5で2倍、6で4倍。
    DEFAULT: ClassVar[Mapping[int, float]] = {3: 1.5, 5: 2.0, 6: 4.0}

    def __init__(
        self,
        multipliers: Mapping[int | str, float] | None = None,
        fast: int = FAST,
        mid: int = MID,
        slow: int = SLOW,
        *,
        window: object = None,
    ) -> None:
        super().__init__(window=window)
        raw = self.DEFAULT if multipliers is None else multipliers
        # TOML のキーは文字列になるため int へ寄せる
        table = {int(k): float(v) for k, v in raw.items()}
        if not table:
            raise ValueError("multipliers が空です")
        for score, value in table.items():
            if not 0 <= score <= 6:
                raise ValueError(f"弱気スコアは 0〜6: {score}")
            _check_multiplier(value)
        self.table = dict(sorted(table.items()))
        _check_monotone(list(self.table.values()), "弱気スコアが大きいほど倍率も大きく")
        self.fast, self.mid, self.slow = fast, mid, slow

    def multiplier(self) -> pl.Expr:
        # スコアが大きいほど倍率も大きいと保証してあるので、段ごとの式の
        # 最大を取れば「最も深い段」を選んだのと同じになる。when/then を
        # 連鎖させるより、段が増えても読みやすい。
        score = stack_score(self.fast, self.mid, self.slow).fill_null(0)
        rungs = [
            pl.when(score >= threshold).then(value).otherwise(1.0)
            for threshold, value in self.table.items()
        ]
        return pl.max_horizontal(rungs).cast(pl.Float64).alias(MULTIPLIER)

    @property
    def warmup_bars(self) -> int:
        return self.slow

    def describe(self) -> str:
        rungs = ", ".join(f"{k}→×{v:g}" for k, v in sorted(self.table.items()))
        return f"{self.name}({rungs})"


class DrawdownLadder(Tactic):
    """過去最高値からの下落率に応じて段階的に増額する。

    米国指数では強い一方、**新高値を作らない市場では破綻する**。
    日経225 の 1971-2026 全期間では常時発動に近くなり、平均取得単価が
    +13% 悪化した。``require_downtrend`` で 200日線割れを条件に加えると
    発動が絞られ、資金効率が概ね 1.5〜2 倍に改善する。
    """

    name: ClassVar[str] = "drawdown_ladder"

    def __init__(
        self,
        levels: Sequence[float] = (0.10, 0.20, 0.30),
        multipliers: Sequence[float] = (2.0, 3.0, 4.0),
        *,
        require_downtrend: bool = False,
        slow: int = SLOW,
        window: object = None,
    ) -> None:
        super().__init__(window=window)
        if len(levels) != len(multipliers):
            raise ValueError(
                f"levels と multipliers の長さが違います: {len(levels)}, {len(multipliers)}"
            )
        if not levels:
            raise ValueError("levels が空です")
        if list(levels) != sorted(levels):
            raise ValueError(f"levels は浅い順に並べてください: {list(levels)}")
        for level in levels:
            if not 0 < level < 1:
                raise ValueError(f"下落率は 0〜1 の割合で指定します: {level}")
        for value in multipliers:
            _check_multiplier(value)
        self.levels = [float(x) for x in levels]
        self.values = [float(x) for x in multipliers]
        _check_monotone(self.values, "下落が深いほど倍率も大きく")
        self.require_downtrend = require_downtrend
        self.slow = slow

    def multiplier(self) -> pl.Expr:
        close = pl.col("close")
        drawdown = close / close.cum_max() - 1.0
        gate = (close < sma(self.slow)).fill_null(value=False) if self.require_downtrend else None

        rungs = []
        for level, value in zip(self.levels, self.values, strict=True):
            branch = drawdown <= -level
            if gate is not None:
                branch = branch & gate
            rungs.append(pl.when(branch).then(value).otherwise(1.0))
        return pl.max_horizontal(rungs).cast(pl.Float64).alias(MULTIPLIER)

    @property
    def warmup_bars(self) -> int:
        return self.slow if self.require_downtrend else 1

    def describe(self) -> str:
        pairs = zip(self.levels, self.values, strict=True)
        rungs = ", ".join(f"-{level:.0%}→×{value:g}" for level, value in pairs)
        gate = "・200日線割れ時のみ" if self.require_downtrend else ""
        return f"{self.name}({rungs}{gate})"


def _check_monotone(values: list[float], requirement: str) -> None:
    """段が深くなるほど倍率が上がることを保証する。

    逆順の指定（浅い下落で大きく買い、深い下落で小さく買う）は設定ミスの
    可能性が高く、許すと段の選び方が直感と食い違う。
    """
    if values != sorted(values):
        raise ValueError(f"{requirement}指定してください: {values}")


def _check_multiplier(value: float) -> None:
    if value < 1.0:
        raise ValueError(
            f"倍率は 1.0 以上にしてください。1未満にすると上昇局面で現金が滞留し、"
            f"検証では平均取得単価が +5〜+30% 悪化しました: {value}"
        )
