"""時刻の規約: 保存は tz 付き、演算は UTC、表示は時間帯を明記。"""

from __future__ import annotations

import datetime as dt
import re
from pathlib import Path

import pytest

from wbcore.clock import ensure_utc, fmt, fmt_time, now_utc, to_zone, today_utc, zone

SRC = Path(__file__).resolve().parent.parent / "src"
MOMENT = dt.datetime(2026, 8, 29, 6, 20, tzinfo=dt.UTC)  # 15:20 JST


def test_now_and_today_are_utc() -> None:
    assert now_utc().tzinfo is dt.UTC
    assert today_utc() == now_utc().date()


def test_naive_is_treated_as_utc_not_local() -> None:
    naive = dt.datetime(2026, 8, 29, 6, 20)
    assert ensure_utc(naive) == MOMENT
    assert to_zone(naive, "Asia/Tokyo").hour == 15


def test_display_always_carries_the_zone() -> None:
    assert fmt(MOMENT) == "2026-08-29 06:20 UTC"
    assert fmt(MOMENT, "Asia/Tokyo") == "2026-08-29 15:20 JST"
    assert fmt(MOMENT, "America/New_York") == "2026-08-29 02:20 EDT"
    assert fmt(MOMENT, seconds=True) == "2026-08-29 06:20:00 UTC"
    assert fmt_time(MOMENT, "Asia/Tokyo") == "15:20 JST"


def test_unknown_zone_is_rejected_with_examples() -> None:
    with pytest.raises(ValueError, match="未知の時間帯"):
        zone("Mars/Olympus")
    assert zone(None).key == "UTC"


# --- 規約の強制 --------------------------------------------------------


def test_no_local_clock_outside_the_clock_module() -> None:
    """ローカル時刻を取る呼び出しは wbcore.clock だけに閉じる。

    `dt.date.today()` / `datetime.now()`（tz 無し）は cron のサーバーと
    開発機で結果が変わる。見つけたら wbcore.clock の関数に置き換える。
    """
    forbidden = re.compile(r"\bdate\.today\(\)|\bdatetime\.now\(\)|\.utcnow\(\)")
    offenders = []
    for path in SRC.rglob("*.py"):
        if path.name == "clock.py":
            continue
        for lineno, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            code = line.split("#", 1)[0]
            if forbidden.search(code) and "now(dt.UTC)" not in code:
                offenders.append(f"{path.relative_to(SRC)}:{lineno}: {line.strip()}")
    assert not offenders, "ローカル時刻の取得が残っています:\n" + "\n".join(offenders)


def test_log_timestamps_follow_the_configured_zone_with_offset() -> None:
    from wbcore.logging import _timestamper

    stamped = _timestamper("Asia/Tokyo")(None, "info", {})["timestamp"]
    assert stamped.endswith("+09:00")
    assert _timestamper("UTC")(None, "info", {})["timestamp"].endswith("+00:00")
    with pytest.raises(ValueError, match="未知の時間帯"):
        _timestamper("Mars/Olympus")


def test_stored_iso_strings_are_shown_in_the_configured_zone() -> None:
    from wbcore.clock import fmt_iso

    assert fmt_iso("2026-08-29T06:36:37+00:00", "Asia/Tokyo") == "2026-08-29 15:36:37 JST"
    assert fmt_iso("2026-08-29T06:36:37Z") == "2026-08-29 06:36:37 UTC"
    assert fmt_iso("2026-08-29", "Asia/Tokyo") == "2026-08-29"  # 日付は時刻ではない
    assert fmt_iso("SUBMITTED", "Asia/Tokyo") == "SUBMITTED"
    assert fmt_iso(None) == "None"


def test_stored_intraday_bars_keep_their_zone(tmp_path: Path) -> None:
    import polars as pl

    from wbcore.data.provider import Interval
    from wbcore.data.store import BarStore

    store = BarStore(tmp_path, Interval.M5)
    store.write(
        "X",
        pl.DataFrame(
            {
                "ts": [MOMENT],
                "open": [1.0],
                "high": [1.0],
                "low": [1.0],
                "close": [1.0],
                "volume": [1.0],
            }
        ),
    )
    assert store.read("X")["ts"].dtype == pl.Datetime("us", "UTC")
    assert store.read("X")["ts"][0] == MOMENT


def test_trading_window_treats_naive_as_utc() -> None:
    from accum.window import TradingWindow

    window = TradingWindow.parse({"start": "14:00", "end": "15:00"})
    assert window.describe() == "14:00〜15:00 JST"
    # 05:30 UTC = 14:30 JST → 時間内。naive でも UTC とみなす
    assert window.allows(dt.datetime(2026, 8, 3, 5, 30))
    assert window.allows(dt.datetime(2026, 8, 3, 5, 30, tzinfo=dt.UTC))
    assert not window.allows(dt.datetime(2026, 8, 3, 14, 30, tzinfo=dt.UTC))
