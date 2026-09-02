"""取引所の現地時刻で見た時間帯。日中足の戦略が「いつ建ててよいか」に使う。

足の時刻は UTC で持っている（:data:`wbcore.data.provider.INTRADAY_BAR_SCHEMA`）。
「寄付直後は建てない」「引け前に手仕舞う」といった判断は取引所の現地時刻で
書くのが自然なので、ここで変換して比べる。
"""

from __future__ import annotations

import datetime as dt
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any
from zoneinfo import ZoneInfo

from wbcore.clock import today_utc
from wbcore.domain.models import Market

#: 通常取引の寄付と引け（現地時刻）。足の区切りを寄付に揃えるのに使う。
_SESSIONS: dict[Market, tuple[dt.time, dt.time]] = {
    Market.JP: (dt.time(9, 0), dt.time(15, 30)),
    Market.US: (dt.time(9, 30), dt.time(16, 0)),
}


def session_open(market: Market) -> dt.time:
    return _SESSIONS[market][0]


def session_close(market: Market) -> dt.time:
    return _SESSIONS[market][1]


def close_utc(market: Market, on: dt.date | None = None) -> dt.datetime:
    """その日の引けを UTC で。夏時間の有無は日付で決まるので ``on`` を受ける。"""
    day = on or today_utc()
    local = dt.datetime.combine(day, session_close(market), tzinfo=market.timezone)
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
    except ValueError, AttributeError:
        raise ValueError(f"{label} は HH:MM 形式で指定します: {value!r}") from None


@dataclass(frozen=True, slots=True)
class SessionWindow:
    """現地時刻の ``start`` 以上 ``end`` 未満。"""

    start: dt.time
    end: dt.time
    tz: ZoneInfo

    def __post_init__(self) -> None:
        if self.start >= self.end:
            raise ValueError(f"start は end より前にしてください: {self.start} >= {self.end}")

    def local(self, at: dt.datetime) -> dt.datetime:
        """UTC の時刻を現地時刻にする。tz 無しは UTC とみなす。"""
        if at.tzinfo is None:
            at = at.replace(tzinfo=dt.UTC)
        return at.astimezone(self.tz)

    def allows(self, at: dt.datetime) -> bool:
        return self.start <= self.local(at).time() < self.end

    def describe(self) -> str:
        return f"{self.start:%H:%M}〜{self.end:%H:%M} {self.tz.key}"

    @classmethod
    def parse(cls, value: Mapping[str, Any] | bool | None, market: Market) -> SessionWindow | None:
        """設定の ``{ start = "09:30", end = "14:30" }`` から作る。None / false なら制限なし。"""
        if not value:
            return None
        if not isinstance(value, Mapping) or "start" not in value or "end" not in value:
            raise ValueError(
                f'session は {{ start = "HH:MM", end = "HH:MM" }} で指定します: {value!r}'
            )
        return cls(
            parse_time(value["start"], "start"), parse_time(value["end"], "end"), market.timezone
        )
