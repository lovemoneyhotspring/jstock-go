"""発注を許す時間帯。

**これは投下額を変えない**

積立の計画は日足で決まる（:mod:`accum.plan`）。この時間帯は
「その日ぶんの注文を、いつ出してよいか」だけを制御する。したがって
:func:`~accum.simulate.simulate` の結果は時間帯を変えても
変わらない。日足では時間内の値動きを再現できないため、変わったように
見せる方が嘘になる。

**なぜ既定を 14:00〜15:00 にしているか**

60分足3年ぶんの実測では、時間帯による平均取得単価の差は ±0.03% しか
なく、どの時間帯が安いという傾向は無かった（大引けより安く買えた日の
割合はどの時間帯も50〜54%）。一方で**大引けに対するぶれ**は時間とともに
単調に小さくなり、寄り付き直後は14時台の約3倍ある。

    09:00 の標準偏差 0.687% ／ 平均値幅 0.778%
    14:00 の標準偏差 0.236% ／ 平均値幅 0.329%

つまり14時台を選ぶ理由は「安く買えるから」ではなく「想定と大きく違う
値段を掴みにくいから」。期待値の改善は無い。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from typing import Any
from zoneinfo import ZoneInfo

#: 東証の時計。
JST = ZoneInfo("Asia/Tokyo")

#: 東証の立会時間（2024年11月5日の延長後。大引けは15:30）。
SESSIONS: tuple[tuple[dt.time, dt.time], ...] = (
    (dt.time(9, 0), dt.time(11, 30)),
    (dt.time(12, 30), dt.time(15, 30)),
)

#: 既定の発注時間帯。
DEFAULT_START = dt.time(14, 0)
DEFAULT_END = dt.time(15, 0)


@dataclass(frozen=True, slots=True)
class TradingWindow:
    """発注を許す時間帯（日本時間）。

    Attributes:
        start: 開始時刻（含む）。
        end: 終了時刻（含まない）。
        enabled: False なら時間帯を制限しない。
    """

    start: dt.time = DEFAULT_START
    end: dt.time = DEFAULT_END
    enabled: bool = True

    def __post_init__(self) -> None:
        if not self.enabled:
            return
        if self.start >= self.end:
            raise ValueError(f"開始が終了以降になっています: {self.start}〜{self.end}")
        if not any(s <= self.start < e for s, e in SESSIONS):
            raise ValueError(
                f"開始 {self.start:%H:%M} が立会時間外です（前場 09:00〜11:30 / 後場 12:30〜15:30）"
            )
        if not any(s < self.end <= e for s, e in SESSIONS):
            raise ValueError(
                f"終了 {self.end:%H:%M} が立会時間外です（前場 09:00〜11:30 / 後場 12:30〜15:30）"
            )

    def allows(self, moment: dt.datetime | None = None) -> bool:
        """その時刻に発注してよいか。

        Args:
            moment: 省略時は現在時刻。naive な値は日本時間とみなす。
        """
        if not self.enabled:
            return True
        return self.start <= self._jst(moment).time() < self.end

    def next_open(self, moment: dt.datetime | None = None) -> dt.datetime:
        """次に発注できるようになる日時。今が時間内ならその時刻自身。

        土日と祝日は考慮しない。休場日かどうかは足データが持っている
        情報であり、時計だけでは判断できないため。
        """
        now = self._jst(moment)
        if not self.enabled or self.allows(now):
            return now
        today = now.replace(hour=self.start.hour, minute=self.start.minute, second=0, microsecond=0)
        return today if now < today else today + dt.timedelta(days=1)

    def describe(self) -> str:
        if not self.enabled:
            return "制限なし"
        return f"{self.start:%H:%M}〜{self.end:%H:%M}"

    @staticmethod
    def _jst(moment: dt.datetime | None) -> dt.datetime:
        if moment is None:
            return dt.datetime.now(JST)
        if moment.tzinfo is None:
            return moment.replace(tzinfo=JST)
        return moment.astimezone(JST)

    @classmethod
    def parse(cls, value: Any) -> TradingWindow:
        """設定ファイルの記述から組み立てる。

        受け付ける形::

            （省略）                              → 既定の 14:00〜15:00
            window = false                        → 制限なし
            window = { start = "14:00", end = "15:00" }

        Raises:
            ValueError: 形式が不正なとき。
        """
        if value is None:
            return cls()
        if isinstance(value, cls):
            return value
        if isinstance(value, bool):
            return cls(enabled=value)
        if not isinstance(value, dict):
            raise ValueError(
                f'window は false か {{ start = "14:00", end = "15:00" }} の形で'
                f"指定してください: {value!r}"
            )
        unknown = set(value) - {"start", "end", "enabled"}
        if unknown:
            raise ValueError(f"window に未知のキーがあります: {sorted(unknown)}")
        return cls(
            start=_time(value.get("start", DEFAULT_START), "start"),
            end=_time(value.get("end", DEFAULT_END), "end"),
            enabled=bool(value.get("enabled", True)),
        )


def _time(value: Any, field: str) -> dt.time:
    if isinstance(value, dt.time):
        return value
    if isinstance(value, str):
        try:
            return dt.time.fromisoformat(value)
        except ValueError:
            raise ValueError(f"window.{field} は HH:MM 形式で指定してください: {value!r}") from None
    raise ValueError(f"window.{field} は文字列で指定してください: {value!r}")
