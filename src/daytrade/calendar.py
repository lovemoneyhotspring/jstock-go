"""東証の営業日。J-Quants の取引カレンダー（``HolDiv``: 1=営業日, 2=半日, 0/3=休場）を使う。"""

from __future__ import annotations

import datetime as dt

import polars as pl

from wbcore.data.jquants_archive import Archive, endpoint

#: 営業日とみなす区分。
TRADING_DIVISIONS = ("1", "2")


class TradingCalendar:
    """営業日の並び。カレンダーが無い期間は平日で代用する（ログは呼び出し側で）。"""

    def __init__(self, days: list[dt.date]) -> None:
        self._days = sorted(set(days))
        self._set = set(self._days)

    @classmethod
    def from_archive(cls, archive: Archive) -> TradingCalendar:
        frame = archive.scan(endpoint("markets_calendar")).collect()
        if frame.height == 0 or "HolDiv" not in frame.columns:
            return cls([])
        days = (
            frame.filter(pl.col("HolDiv").is_in(TRADING_DIVISIONS))
            .select("Date")
            .to_series()
            .to_list()
        )
        return cls([d for d in days if isinstance(d, dt.date)])

    @property
    def empty(self) -> bool:
        return not self._days

    def is_trading_day(self, day: dt.date) -> bool:
        if self.empty or day < self._days[0] or day > self._days[-1]:
            return day.weekday() < 5
        return day in self._set

    def next_trading_day(self, after: dt.date, *, inclusive: bool = False) -> dt.date:
        day = after if inclusive else after + dt.timedelta(days=1)
        for _ in range(60):
            if self.is_trading_day(day):
                return day
            day += dt.timedelta(days=1)
        raise ValueError(f"{after} 以降 60 日に営業日がありません")

    def previous_trading_day(self, before: dt.date) -> dt.date:
        day = before - dt.timedelta(days=1)
        for _ in range(60):
            if self.is_trading_day(day):
                return day
            day -= dt.timedelta(days=1)
        raise ValueError(f"{before} 以前 60 日に営業日がありません")
