"""足の蓄積が健全かを調べる。cron が黙って止まっているのを見つけるため。

分足は遡れる期間が短い（yfinance の 1 分足は 7 日）。取り込みが止まって
いることに 7 日以上気づかなければ、その穴は永久に残る。だから
「最後にいつ取れたか」と「取れているべき日に穴が無いか」を機械的に見る。

取れているべき日（取引日）は**その銘柄の日足があった日**で決める。
祝日の一覧を持たなくても、日足が存在する日は市場が開いていた日なので、
その日に日中足が無ければ穴だと言える。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field
from pathlib import Path

from wbcore.data.provider import Interval
from wbcore.data.store import BarStore

#: 日足がこの日数より古ければ「止まっている」とみなす（週末＋祝日の余裕）。
DAILY_STALE_AFTER_DAYS = 4


@dataclass(frozen=True, slots=True)
class Coverage:
    """1銘柄・1間隔の蓄積状況。"""

    symbol: str
    interval: Interval
    bars: int
    first: dt.date | None
    last: dt.date | None
    #: 日足はあるのに、この間隔の足が無い取引日（蓄積を始めた日以降）。
    missing_days: list[dt.date] = field(default_factory=list)
    #: 最後の足が、あるべき日より古い。
    stale: bool = False

    @property
    def healthy(self) -> bool:
        return self.bars > 0 and not self.stale and not self.missing_days

    def describe(self) -> str:
        if self.bars == 0:
            return "足が無い"
        problems = []
        if self.stale:
            problems.append(f"最終 {self.last} で止まっている")
        if self.missing_days:
            shown = ", ".join(str(d) for d in self.missing_days[:3])
            more = f" ほか{len(self.missing_days) - 3}日" if len(self.missing_days) > 3 else ""
            problems.append(f"穴 {shown}{more}")
        return "、".join(problems) if problems else "正常"


def check(
    root: Path,
    symbols: list[str],
    intervals: list[Interval],
    *,
    today: dt.date | None = None,
    lookback_days: int = 30,
) -> list[Coverage]:
    """銘柄×間隔ごとの蓄積状況。日足を「取引日の定義」として使う。

    Args:
        lookback_days: 穴を探す範囲（暦日）。古い穴は今さら埋まらないので見ない。
    """
    today = today or dt.date.today()
    since = today - dt.timedelta(days=lookback_days)
    daily_store = BarStore(root, Interval.D1)
    out: list[Coverage] = []

    for symbol in symbols:
        daily = daily_store.read(symbol, start=since)
        trading_days = sorted(d for d in daily["date"].to_list() if d < today)
        last_trading_day = trading_days[-1] if trading_days else None

        for interval in intervals:
            store = daily_store if interval is Interval.D1 else BarStore(root, interval)
            frame = store.read(symbol)
            if frame.height == 0:
                out.append(Coverage(symbol, interval, 0, None, None))
                continue
            first, last = frame["date"].min(), frame["date"].max()
            assert isinstance(first, dt.date) and isinstance(last, dt.date)

            if interval is Interval.D1:
                stale = (today - last).days > DAILY_STALE_AFTER_DAYS
                missing: list[dt.date] = []
            else:
                have = set(frame.filter(frame["date"] >= since)["date"].to_list())
                # 蓄積を始めた日より前は「まだ無い」だけで穴ではない
                missing = [d for d in trading_days if d >= first and d not in have]
                stale = last_trading_day is not None and last < last_trading_day
            out.append(Coverage(symbol, interval, frame.height, first, last, missing, stale))
    return out
