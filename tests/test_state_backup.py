"""state/ バックアップのテスト。"""

from __future__ import annotations

import datetime as dt
import sqlite3
from pathlib import Path

from wbcore.backup import backup_state
from wbcore.settings import AppSettings


def _make_db(path: Path, value: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with sqlite3.connect(path) as conn:
        conn.execute("CREATE TABLE t (v TEXT)")
        conn.execute("INSERT INTO t VALUES (?)", (value,))


def _read(path: Path) -> str:
    with sqlite3.connect(path) as conn:
        return str(conn.execute("SELECT v FROM t").fetchone()[0])


def _settings(tmp_path: Path) -> AppSettings:
    return AppSettings(data_dir=tmp_path / "data", state_dir=tmp_path / "state")


def test_backs_up_every_sqlite_in_state(tmp_path: Path) -> None:
    settings = _settings(tmp_path)
    _make_db(settings.state_dir / "accum-prod.db", "accum")
    _make_db(settings.state_dir / "wbjp-prod.db", "wbjp")
    (settings.state_dir / "logs").mkdir()
    (settings.state_dir / "logs" / "accum-prod.jsonl").write_text("{}\n")

    result = backup_state(settings, today=dt.date(2026, 8, 31))

    names = sorted(p.name for p in result.copied)
    assert names == ["accum-prod-20260831.db", "wbjp-prod-20260831.db"]
    assert _read(settings.backup_dir / "accum-prod-20260831.db") == "accum"
    assert _read(settings.backup_dir / "wbjp-prod-20260831.db") == "wbjp"
    # logs はバックアップしない
    assert not list(settings.backup_dir.glob("*.jsonl"))


def test_prunes_old_generations_per_stem(tmp_path: Path) -> None:
    settings = _settings(tmp_path)
    _make_db(settings.state_dir / "accum-prod.db", "x")
    settings.backup_dir.mkdir(parents=True)
    for day in (1, 2, 3):
        (settings.backup_dir / f"accum-prod-2026080{day}.db").write_bytes(b"old")
    # 他の系列（wbjp）の世代は accum の削減に巻き込まれない
    (settings.backup_dir / "wbjp-prod-20260801.db").write_bytes(b"keep")

    result = backup_state(settings, keep=2, today=dt.date(2026, 8, 31))

    assert sorted(p.name for p in result.removed) == [
        "accum-prod-20260801.db",
        "accum-prod-20260802.db",
    ]
    remaining = sorted(p.name for p in settings.backup_dir.glob("accum-prod-*.db"))
    assert remaining == ["accum-prod-20260803.db", "accum-prod-20260831.db"]
    assert (settings.backup_dir / "wbjp-prod-20260801.db").exists()


def test_same_day_rerun_overwrites_not_duplicates(tmp_path: Path) -> None:
    settings = _settings(tmp_path)
    _make_db(settings.state_dir / "accum-prod.db", "v1")
    backup_state(settings, today=dt.date(2026, 8, 31))
    with sqlite3.connect(settings.state_dir / "accum-prod.db") as conn:
        conn.execute("UPDATE t SET v = 'v2'")
    result = backup_state(settings, today=dt.date(2026, 8, 31))
    assert len(result.copied) == 1
    assert _read(settings.backup_dir / "accum-prod-20260831.db") == "v2"


def test_empty_state_dir_is_fine(tmp_path: Path) -> None:
    settings = _settings(tmp_path)
    settings.state_dir.mkdir(parents=True)
    result = backup_state(settings)
    assert result.copied == [] and result.removed == []
