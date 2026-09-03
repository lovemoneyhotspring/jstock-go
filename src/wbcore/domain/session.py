"""取引所の引け時刻と、市場をまたぐ判定の前後関係。

東証の銘柄を米国の指数で判定するとき（積立の ``signal_symbol``）に、
同じ日付の指数の足が判断時点で存在するかを :func:`closes_after` で決める。
"""

from __future__ import annotations

import datetime as dt
from typing import Any

from wbcore.clock import today_utc
from wbcore.domain.models import Market

#: 通常取引の引け（現地時刻）。
_CLOSES: dict[Market, dt.time] = {
    Market.JP: dt.time(15, 30),
    Market.US: dt.time(16, 0),
}


def close_utc(market: Market, on: dt.date | None = None) -> dt.datetime:
    """その日の引けを UTC で。夏時間の有無は日付で決まるので ``on`` を受ける。"""
    day = on or today_utc()
    local = dt.datetime.combine(day, _CLOSES[market], tzinfo=market.timezone)
    return local.astimezone(dt.UTC)


def closes_after(market: Market, other: Market, on: dt.date | None = None) -> bool:
    """``market`` の引けが、同じ暦日の ``other`` の引けより後か。

    「日付が同じ足」でも、引けが後の市場の足はその日の判断時点にまだ無い。
    東証の銘柄を NASDAQ の指数で判定するなら、同じ日付の指数の足は使えず
    前日の足を使う（米国の引け 16:00 ET は翌日 05〜06:00 JST）。
    """
    return close_utc(market, on).time() > close_utc(other, on).time()


def parse_time(value: Any, label: str = "time") -> dt.time:
    """``"09:30"`` のような表記を time にする。"""
    if isinstance(value, dt.time):
        return value
    try:
        hour, minute = str(value).split(":")
        return dt.time(int(hour), int(minute))
    except (ValueError, AttributeError):
        raise ValueError(f"{label} は HH:MM 形式で指定します: {value!r}") from None
