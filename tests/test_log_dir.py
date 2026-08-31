"""ログの置き場は 1 箇所。既定はデータ置き場に追従し、WBJP_LOG_DIR で丸ごと動かせる。"""

from __future__ import annotations

from pathlib import Path

import pytest

from wbcore.settings import AppSettings


@pytest.fixture(autouse=True)
def _no_dotenv(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.chdir(tmp_path)  # リポジトリの .env を読まない
    for name in ("WBJP_LOG_DIR", "WBJP_DATA_DIR", "WBJP_STATE_DIR", "WBJP_ENV"):
        monkeypatch.delenv(name, raising=False)


def test_log_dir_follows_state_dir_by_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("WBJP_STATE_DIR", "/var/lib/wbjp/state")
    settings = AppSettings()
    assert settings.resolved_log_dir == Path("/var/lib/wbjp/state/logs")
    assert settings.log_file("accum") == Path("/var/lib/wbjp/state/logs/accum-uat.jsonl")


def test_log_dir_override_wins(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("WBJP_LOG_DIR", "/srv/logs")
    monkeypatch.setenv("WBJP_ENV", "prod")
    assert AppSettings().log_file("wbjp") == Path("/srv/logs/wbjp-prod.jsonl")


def test_db_paths_live_in_state_dir(tmp_path: Path) -> None:
    settings = AppSettings(data_dir=tmp_path / "data", state_dir=tmp_path / "state")
    assert settings.db_path == tmp_path / "state" / "wbjp-uat.db"
    assert settings.accum_db_path == tmp_path / "state" / "accum-uat.db"
    assert settings.backup_dir == tmp_path / "state" / "backup"


def test_state_files_migrate_from_old_data_dir_layout(tmp_path: Path) -> None:
    """旧配置（data/ 直下の台帳）は初回アクセスで state/ へ移る。

    pull 直後の cron が空の台帳で走って当月を買い直す事故を防ぐ。
    """
    data = tmp_path / "data"
    data.mkdir()
    (data / "accum-uat.db").write_bytes(b"ledger")
    (data / "backup").mkdir()
    (data / "backup" / "accum-uat-20260801.db").write_bytes(b"old")

    settings = AppSettings(data_dir=data, state_dir=tmp_path / "state")
    assert settings.accum_db_path.read_bytes() == b"ledger"
    assert not (data / "accum-uat.db").exists()
    assert (settings.backup_dir / "accum-uat-20260801.db").read_bytes() == b"old"
    assert not (data / "backup").exists()
    # 2 回目のアクセスは何も起きない
    assert settings.accum_db_path.read_bytes() == b"ledger"
