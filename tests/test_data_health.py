"""足の蓄積の健全性チェックと通知。"""

from __future__ import annotations

import datetime as dt
import json
from pathlib import Path

import polars as pl
import pytest

from wbcore import notify
from wbcore.data.health import check
from wbcore.data.provider import Interval
from wbcore.data.store import BarStore
from wbjp.cli import app

TODAY = dt.date(2026, 8, 29)  # 土曜
# 直近の取引日（月〜金）
TRADING = [
    dt.date(2026, 8, 24),
    dt.date(2026, 8, 25),
    dt.date(2026, 8, 26),
    dt.date(2026, 8, 27),
    dt.date(2026, 8, 28),
]


def _daily(dates: list[dt.date]) -> pl.DataFrame:
    n = len(dates)
    return pl.DataFrame(
        {
            "date": dates,
            "open": [1.0] * n,
            "high": [1.0] * n,
            "low": [1.0] * n,
            "close": [1.0] * n,
            "volume": [1.0] * n,
        }
    )


def _minutes(dates: list[dt.date]) -> pl.DataFrame:
    stamps = [dt.datetime.combine(d, dt.time(0, 0), tzinfo=dt.UTC) for d in dates]
    n = len(stamps)
    return pl.DataFrame(
        {
            "ts": stamps,
            "open": [1.0] * n,
            "high": [1.0] * n,
            "low": [1.0] * n,
            "close": [1.0] * n,
            "volume": [1.0] * n,
        }
    ).with_columns(pl.col("ts").dt.date().alias("date"))


def test_complete_coverage_is_healthy(tmp_path: Path) -> None:
    BarStore(tmp_path).write("X", _daily(TRADING))
    BarStore(tmp_path, Interval.M1).write("X", _minutes(TRADING))
    (m1, d1) = check(tmp_path, ["X"], [Interval.M1, Interval.D1], today=TODAY)
    assert m1.healthy and d1.healthy
    assert m1.describe() == "正常"


def test_gap_is_a_trading_day_with_daily_but_no_intraday(tmp_path: Path) -> None:
    BarStore(tmp_path).write("X", _daily(TRADING))
    BarStore(tmp_path, Interval.M1).write(
        "X", _minutes([TRADING[0], TRADING[1], TRADING[3], TRADING[4]])
    )
    (m1,) = check(tmp_path, ["X"], [Interval.M1], today=TODAY)
    assert m1.missing_days == [dt.date(2026, 8, 26)]
    assert not m1.stale
    assert "穴 2026-08-26" in m1.describe()


def test_days_before_accumulation_started_are_not_gaps(tmp_path: Path) -> None:
    """蓄積を始めた日より前に分足が無いのは当然で、穴ではない。"""
    BarStore(tmp_path).write("X", _daily(TRADING))
    BarStore(tmp_path, Interval.M1).write("X", _minutes(TRADING[3:]))
    (m1,) = check(tmp_path, ["X"], [Interval.M1], today=TODAY)
    assert m1.healthy


def test_stale_when_intraday_stops_before_the_last_trading_day(tmp_path: Path) -> None:
    BarStore(tmp_path).write("X", _daily(TRADING))
    BarStore(tmp_path, Interval.M1).write("X", _minutes(TRADING[:3]))
    (m1,) = check(tmp_path, ["X"], [Interval.M1], today=TODAY)
    assert m1.stale and "止まっている" in m1.describe()


def test_daily_is_stale_after_a_few_calendar_days(tmp_path: Path) -> None:
    BarStore(tmp_path).write("X", _daily(TRADING[:2]))  # 最終 8/25、今日は 8/29 → 4 日
    (d1,) = check(tmp_path, ["X"], [Interval.D1], today=TODAY)
    assert d1.healthy
    (d1_old,) = check(tmp_path, ["X"], [Interval.D1], today=TODAY + dt.timedelta(days=1))
    assert d1_old.stale


def test_missing_symbol_is_reported(tmp_path: Path) -> None:
    (m1,) = check(tmp_path, ["NOPE"], [Interval.M1], today=TODAY)
    assert not m1.healthy and m1.describe() == "足が無い"


# --- 通知 --------------------------------------------------------------


def test_alert_without_webhook_only_logs(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(notify.WEBHOOK_ENV, raising=False)
    assert notify.alert("t", "b") is False


def test_alert_posts_json_to_the_webhook(monkeypatch: pytest.MonkeyPatch) -> None:
    sent: dict[str, object] = {}

    class _Response:
        status = 200

        def __enter__(self) -> _Response:
            return self

        def __exit__(self, *exc: object) -> None:
            return None

    def fake_urlopen(request, timeout):  # type: ignore[no-untyped-def]
        sent["url"] = request.full_url
        sent["body"] = json.loads(request.data)
        return _Response()

    monkeypatch.setenv(notify.WEBHOOK_ENV, "https://hooks.example/abc")
    monkeypatch.setattr(notify.urllib.request, "urlopen", fake_urlopen)
    assert notify.alert("止まった", "詳細") is True
    assert sent["url"] == "https://hooks.example/abc"
    assert sent["body"]["text"] == "[wbjp] 止まった\n詳細"  # type: ignore[index]


# --- CLI --------------------------------------------------------------


def test_data_check_exits_nonzero_on_problems(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from typer.testing import CliRunner

    (tmp_path / "settings.toml").write_text(
        '[universe]\nmarket = "JP"\ninterval = "5m"\nbase_interval = "1m"\nsymbols = ["X"]\n',
        encoding="utf-8",
    )
    monkeypatch.setenv("WBJP_DATA_DIR", str(tmp_path / "data"))
    result = CliRunner().invoke(app, ["data", "check", "--config-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "足が無い" in result.stdout
