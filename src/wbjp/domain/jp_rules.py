"""日本株の取引ルール。

呼値・値幅制限・単元株・取引時間・差金決済といった、東証固有の制約を
1か所に集める。これらを間違えると注文が取引所に静かに弾かれるため、
テーブルは必ず一次情報に合わせ、変更時はここだけを直す。

出典:
    呼値の単位 https://www.jpx.co.jp/equities/trading/domestic/07.html
    制限値幅   https://www.jpx.co.jp/equities/trading/domestic/06.html
    （JPX が直接取得を拒否するため、表の値は JPX 準拠の
      https://www.matsui.co.jp/stock/domestic/rule/pub/ で確認した）
"""

from __future__ import annotations

import datetime as dt
from bisect import bisect_left, bisect_right
from decimal import ROUND_DOWN, ROUND_HALF_UP, ROUND_UP, Decimal
from enum import Enum, auto

from wbjp.domain.models import Side

JST = dt.timezone(dt.timedelta(hours=9), "JST")

#: 標準的な売買単位。ETF/REIT など一部に 1株・10株単位の銘柄がある。
DEFAULT_LOT_SIZE = Decimal(100)


# --------------------------------------------------------------------------
# 呼値（tick size）
# --------------------------------------------------------------------------

#: 通常銘柄の呼値。(価格の上限（この値以下）, 呼値の単位)
_TICK_STANDARD: list[tuple[Decimal, Decimal]] = [
    (Decimal("1000"), Decimal("1")),
    (Decimal("3000"), Decimal("1")),
    (Decimal("5000"), Decimal("5")),
    (Decimal("10000"), Decimal("10")),
    (Decimal("30000"), Decimal("10")),
    (Decimal("50000"), Decimal("50")),
    (Decimal("100000"), Decimal("100")),
    (Decimal("300000"), Decimal("100")),
    (Decimal("500000"), Decimal("500")),
    (Decimal("1000000"), Decimal("1000")),
    (Decimal("3000000"), Decimal("1000")),
    (Decimal("5000000"), Decimal("5000")),
    (Decimal("10000000"), Decimal("10000")),
    (Decimal("30000000"), Decimal("10000")),
    (Decimal("50000000"), Decimal("50000")),
]
#: 上記の上限を超える価格帯の呼値。
_TICK_STANDARD_ABOVE = Decimal("100000")

#: TOPIX500 構成銘柄の呼値（通常銘柄より細かい）。
_TICK_TOPIX500: list[tuple[Decimal, Decimal]] = [
    (Decimal("1000"), Decimal("0.1")),
    (Decimal("3000"), Decimal("0.5")),
    (Decimal("5000"), Decimal("1")),
    (Decimal("10000"), Decimal("1")),
    (Decimal("30000"), Decimal("5")),
    (Decimal("50000"), Decimal("10")),
    (Decimal("100000"), Decimal("10")),
    (Decimal("300000"), Decimal("50")),
    (Decimal("500000"), Decimal("100")),
    (Decimal("1000000"), Decimal("100")),
    (Decimal("3000000"), Decimal("500")),
    (Decimal("5000000"), Decimal("1000")),
    (Decimal("10000000"), Decimal("1000")),
    (Decimal("30000000"), Decimal("5000")),
    (Decimal("50000000"), Decimal("10000")),
]
_TICK_TOPIX500_ABOVE = Decimal("10000")

_TICK_STANDARD_BOUNDS = [b for b, _ in _TICK_STANDARD]
_TICK_TOPIX500_BOUNDS = [b for b, _ in _TICK_TOPIX500]


def tick_size(price: Decimal, *, topix500: bool = False) -> Decimal:
    """``price`` の価格帯における呼値の単位を返す。

    Args:
        price: 判定対象の値段。
        topix500: TOPIX500 構成銘柄なら True。呼値が細かくなる。

    Raises:
        ValueError: price が 0 以下のとき。
    """
    if price <= 0:
        raise ValueError(f"price は正の数: {price}")

    if topix500:
        bounds, table, above = _TICK_TOPIX500_BOUNDS, _TICK_TOPIX500, _TICK_TOPIX500_ABOVE
    else:
        bounds, table, above = _TICK_STANDARD_BOUNDS, _TICK_STANDARD, _TICK_STANDARD_ABOVE

    idx = bisect_left(bounds, price)
    if idx >= len(table):
        return above
    return table[idx][1]


class PriceRounding(Enum):
    """呼値へ丸めるときの方針。"""

    #: 不利にならない方向へ丸める（買いは切り下げ、売りは切り上げ）。既定。
    CONSERVATIVE = auto()
    #: 約定しやすい方向へ丸める（買いは切り上げ、売りは切り下げ）。損切りなど。
    AGGRESSIVE = auto()
    #: 最も近い呼値へ丸める。
    NEAREST = auto()


def snap_to_tick(
    price: Decimal,
    side: Side,
    *,
    topix500: bool = False,
    rounding: PriceRounding = PriceRounding.CONSERVATIVE,
) -> Decimal:
    """指値を有効な呼値に乗せる。

    呼値に乗っていない指値は取引所に受け付けられない。指値注文を出す前に
    必ずこれを通すこと。

    丸めによって価格帯をまたぐ場合があるため、呼値が安定するまで
    最大数回だけ再計算する。
    """
    if price <= 0:
        raise ValueError(f"price は正の数: {price}")

    match rounding:
        case PriceRounding.NEAREST:
            mode = ROUND_HALF_UP
        case PriceRounding.CONSERVATIVE:
            mode = ROUND_DOWN if side is Side.BUY else ROUND_UP
        case PriceRounding.AGGRESSIVE:
            mode = ROUND_UP if side is Side.BUY else ROUND_DOWN

    current = price
    for _ in range(4):
        tick = tick_size(current, topix500=topix500)
        snapped = (current / tick).quantize(Decimal(1), rounding=mode) * tick
        # 切り下げで 0 になるのは最小呼値未満のときだけ。1ティックに引き上げる。
        if snapped <= 0:
            snapped = tick
        if snapped == current or tick_size(snapped, topix500=topix500) == tick:
            return _tidy(snapped)
        current = snapped
    return _tidy(current)


def _tidy(value: Decimal) -> Decimal:
    """余分な末尾のゼロを落としつつ、指数表記になるのを防ぐ。

    ``Decimal("1000.0").normalize()`` は ``Decimal("1E+3")`` を返す。
    これをそのまま文字列化して API に渡すと ``"1E+3"`` となり注文が
    弾かれるため、正の指数は必ず展開し直す。
    """
    normalized = value.normalize()
    _, _, exponent = normalized.as_tuple()
    if isinstance(exponent, int) and exponent > 0:
        return normalized.quantize(Decimal(1))
    return normalized


# --------------------------------------------------------------------------
# 制限値幅（ストップ高・ストップ安）
# --------------------------------------------------------------------------

#: (基準値段の上限（この値「未満」）, 制限値幅)
#: 呼値のテーブルが「以下」区分なのに対し、こちらは「未満」区分。
#: 引き方が違う（bisect_right を使う）ので混同しないこと。
_PRICE_LIMITS: list[tuple[Decimal, Decimal]] = [
    (Decimal("100"), Decimal("30")),
    (Decimal("200"), Decimal("50")),
    (Decimal("500"), Decimal("80")),
    (Decimal("700"), Decimal("100")),
    (Decimal("1000"), Decimal("150")),
    (Decimal("1500"), Decimal("300")),
    (Decimal("2000"), Decimal("400")),
    (Decimal("3000"), Decimal("500")),
    (Decimal("5000"), Decimal("700")),
    (Decimal("7000"), Decimal("1000")),
    (Decimal("10000"), Decimal("1500")),
    (Decimal("15000"), Decimal("3000")),
    (Decimal("20000"), Decimal("4000")),
    (Decimal("30000"), Decimal("5000")),
    (Decimal("50000"), Decimal("7000")),
    (Decimal("70000"), Decimal("10000")),
    (Decimal("100000"), Decimal("15000")),
    (Decimal("150000"), Decimal("30000")),
    (Decimal("200000"), Decimal("40000")),
    (Decimal("300000"), Decimal("50000")),
    (Decimal("500000"), Decimal("70000")),
    (Decimal("700000"), Decimal("100000")),
    (Decimal("1000000"), Decimal("150000")),
    (Decimal("1500000"), Decimal("300000")),
    (Decimal("2000000"), Decimal("400000")),
    (Decimal("3000000"), Decimal("500000")),
    (Decimal("5000000"), Decimal("700000")),
    (Decimal("7000000"), Decimal("1000000")),
    (Decimal("10000000"), Decimal("1500000")),
    (Decimal("15000000"), Decimal("3000000")),
    (Decimal("20000000"), Decimal("4000000")),
    (Decimal("30000000"), Decimal("5000000")),
    (Decimal("50000000"), Decimal("7000000")),
]
_PRICE_LIMIT_ABOVE = Decimal("10000000")
_PRICE_LIMIT_BOUNDS = [b for b, _ in _PRICE_LIMITS]


def price_limit_width(base_price: Decimal) -> Decimal:
    """基準値段に対する制限値幅（上下の片側）を返す。"""
    if base_price <= 0:
        raise ValueError(f"base_price は正の数: {base_price}")
    # 「未満」区分なので、境界値ちょうどは上の区分に入る → bisect_right
    idx = bisect_right(_PRICE_LIMIT_BOUNDS, base_price)
    if idx >= len(_PRICE_LIMITS):
        return _PRICE_LIMIT_ABOVE
    return _PRICE_LIMITS[idx][1]


def price_limit_range(base_price: Decimal) -> tuple[Decimal, Decimal]:
    """(ストップ安, ストップ高) を返す。

    基準値段は通常、前営業日の終値。
    """
    width = price_limit_width(base_price)
    low = base_price - width
    return (max(low, Decimal(1)), base_price + width)


def is_within_price_limit(price: Decimal, base_price: Decimal) -> bool:
    """``price`` が制限値幅の内側か。外側の指値は受け付けられない。"""
    low, high = price_limit_range(base_price)
    return low <= price <= high


# --------------------------------------------------------------------------
# 単元株
# --------------------------------------------------------------------------


def round_to_lot(quantity: Decimal, lot_size: Decimal = DEFAULT_LOT_SIZE) -> Decimal:
    """売買単位に切り捨てる。

    切り上げないのは、資金を超過する注文を作らないため。
    単元未満は発注できないので、端数は必ず捨てる。
    """
    if lot_size <= 0:
        raise ValueError(f"lot_size は正の数: {lot_size}")
    if quantity < 0:
        return -((-quantity // lot_size) * lot_size)
    return (quantity // lot_size) * lot_size


# --------------------------------------------------------------------------
# 取引時間
# --------------------------------------------------------------------------

#: 前場（2024年11月5日以降も変更なし）
MORNING_OPEN = dt.time(9, 0)
MORNING_CLOSE = dt.time(11, 30)
#: 後場（2024年11月5日にクロージング・オークション導入で 15:30 まで延長）
AFTERNOON_OPEN = dt.time(12, 30)
AFTERNOON_CLOSE = dt.time(15, 30)


def is_trading_hours(when: dt.datetime) -> bool:
    """東証のザラ場中か。

    祝日・年末年始は判定しない（取引所カレンダーが別途必要）。
    ここは時刻のみを見る。
    """
    jst = when.astimezone(JST)
    if jst.weekday() >= 5:  # 土日
        return False
    t = jst.time()
    return (MORNING_OPEN <= t <= MORNING_CLOSE) or (AFTERNOON_OPEN <= t <= AFTERNOON_CLOSE)


# --------------------------------------------------------------------------
# 差金決済の防止
# --------------------------------------------------------------------------


def violates_same_day_settlement(
    side: Side,
    symbol: str,
    bought_today: set[str],
) -> bool:
    """差金決済（同一資金・同一銘柄の日計り）に該当しうるかを判定する。

    現物取引で、同じ日に同じ銘柄を「買い→売り→買い」すると、同一資金の
    二重利用となり法令違反になる。受渡が T+2 のため、資金が実際に戻るのは
    2営業日後。

    ここでは保守的に「当日買い付けた銘柄は当日売らない」で止める。
    日足スイング運用ではそもそも起きないが、ストップ発動時などに
    誤って発生しうるため、安全側に倒しておく。

    Args:
        side: 発注しようとしている売買方向。
        symbol: 銘柄コード。
        bought_today: 当日すでに買い付けた銘柄コードの集合。
    """
    return side is Side.SELL and symbol in bought_today
