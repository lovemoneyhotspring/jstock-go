"""移動平均の配列（並び順）から相場の位置を判定する。

**完全下降配列** ``終値 < MA20 < MA50 < MA200`` は、4本すべてが
下向きに整列した状態を指す。出現率は指数で概ね5〜13%と稀で、
そのとき価格は過去最高値から平均30%超下にある。

**予測力があるわけではない。** 検証では、この状態の1年後の平均価格は
上昇配列のときより*低い*（＝将来リターンはむしろ悪い）。それでも
平均取得単価が下がるのは、単純にその瞬間の価格が安いからで、
効果は「その後この市場が回復するか」に全面的に依存する。
回復しなかった市場（ビットコイン）では逆効果になる。
"""

from __future__ import annotations

import polars as pl

from wbjp.indicators.ohlcv import sma

#: 既定の移動平均の組。20/50/200 は感度検証で広い高原になっており、
#: 前後にずらしても結論が変わらないことを確認済み。
FAST, MID, SLOW = 20, 50, 200


def bear_stack(
    fast: int = FAST, mid: int = MID, slow: int = SLOW, column: str = "close"
) -> pl.Expr:
    """完全下降配列 ``価格 < MA(fast) < MA(mid) < MA(slow)`` を判定する。

    Returns:
        ``bear_stack`` という名前の Boolean 列を作る式。移動平均が
        確定するまでは null になる（False ではない）。
    """
    _check(fast, mid, slow)
    price = pl.col(column)
    f, m, s = sma(fast, column), sma(mid, column), sma(slow, column)
    return ((price < f) & (f < m) & (m < s)).alias("bear_stack")


def stack_score(
    fast: int = FAST, mid: int = MID, slow: int = SLOW, column: str = "close"
) -> pl.Expr:
    """弱気スコア（0〜6）。6つの大小関係のうち成立している数。

    ``0`` = 完全上昇配列、``6`` = 完全下降配列。倍率を段階的に
    変えたい場合の材料。:func:`bear_stack` は ``stack_score() == 6``
    と同じ条件を、より安く判定するもの。
    """
    _check(fast, mid, slow)
    price = pl.col(column)
    f, m, s = sma(fast, column), sma(mid, column), sma(slow, column)
    pairs = [price < f, price < m, price < s, f < m, f < s, m < s]
    total = pairs[0].cast(pl.Int8)
    for pair in pairs[1:]:
        total = total + pair.cast(pl.Int8)
    return total.alias("stack_score")


def _check(fast: int, mid: int, slow: int) -> None:
    if not 0 < fast < mid < slow:
        raise ValueError(f"fast < mid < slow である必要があります: {fast}, {mid}, {slow}")
