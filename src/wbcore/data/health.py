"""足の蓄積が健全かを調べる。cron が黙って止まっているのを見つけるため。

日足の「最後にいつ取れたか」を機械的に見る。止まっていることに気づくのが
遅れるほど、判断は古い足のまま続く。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from pathlib import Path

from wbcore.clock import today_utc
from wbcore.data.store import BarStore

#: 日足がこの日数より古ければ「止まっている」とみなす（週末＋祝日の余裕）。
DAILY_STALE_AFTER_DAYS = 4


@dataclass(frozen=True, slots=True)
class Coverage:
    """1銘柄の蓄積状況。"""

    symbol: str
    bars: int
    first: dt.date | None
    last: dt.date | None
    #: 最後の足が、あるべき日より古い。
    stale: bool = False

    @property
    def healthy(self) -> bool:
        return self.bars > 0 and not self.stale

    def describe(self) -> str:
        if self.bars == 0:
            return "足が無い"
        if self.stale:
            return f"最終 {self.last} で止まっている"
        return "正常"


def check(root: Path, symbols: list[str], *, today: dt.date | None = None) -> list[Coverage]:
    """銘柄ごとの蓄積状況。"""
    today = today or today_utc()
    store = BarStore(root)
    out: list[Coverage] = []
    for symbol in symbols:
        frame = store.read(symbol)
        if frame.height == 0:
            out.append(Coverage(symbol, 0, None, None))
            continue
        first, last = frame["date"].min(), frame["date"].max()
        assert isinstance(first, dt.date) and isinstance(last, dt.date)
        stale = (today - last).days > DAILY_STALE_AFTER_DAYS
        out.append(Coverage(symbol, frame.height, first, last, stale))
    return out
