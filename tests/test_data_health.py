"""足の蓄積の健全性チェックと通知。"""

from __future__ import annotations

import datetime as dt
import json
from pathlib import Path

import polars as pl
import pytest

from wbcore import notify
from wbcore.data.health import check
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


def test_fresh_daily_bars_are_healthy(tmp_path: Path) -> None:
    BarStore(tmp_path).write("X", _daily(TRADING))
    (d1,) = check(tmp_path, ["X"], today=TODAY)
    assert d1.healthy and d1.describe() == "正常"
    assert (d1.first, d1.last, d1.bars) == (TRADING[0], TRADING[-1], 5)


def test_daily_is_stale_after_a_few_calendar_days(tmp_path: Path) -> None:
    BarStore(tmp_path).write("X", _daily(TRADING[:2]))  # 最終 8/25、今日は 8/29 → 4 日
    (d1,) = check(tmp_path, ["X"], today=TODAY)
    assert d1.healthy
    (d1_old,) = check(tmp_path, ["X"], today=TODAY + dt.timedelta(days=1))
    assert d1_old.stale and "止まっている" in d1_old.describe()


def test_missing_symbol_is_reported(tmp_path: Path) -> None:
    (d1,) = check(tmp_path, ["NOPE"], today=TODAY)
    assert not d1.healthy and d1.describe() == "足が無い"


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
        '[universe]\nmarket = "JP"\nsymbols = ["X"]\n',
        encoding="utf-8",
    )
    monkeypatch.setenv("WBJP_DATA_DIR", str(tmp_path / "data"))
    result = CliRunner().invoke(app, ["data", "check", "--config-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "足が無い" in result.stdout
