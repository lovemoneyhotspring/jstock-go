"""ログの置き場は 1 箇所。既定はデータ置き場に追従し、WBJP_LOG_DIR で丸ごと動かせる。"""

from __future__ import annotations

from pathlib import Path

import pytest

from wbcore.settings import AppSettings


@pytest.fixture(autouse=True)
def _no_dotenv(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.chdir(tmp_path)  # リポジトリの .env を読まない
    for name in ("WBJP_LOG_DIR", "WBJP_DATA_DIR", "WBJP_ENV"):
        monkeypatch.delenv(name, raising=False)


def test_log_dir_follows_data_dir_by_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("WBJP_DATA_DIR", "/var/lib/wbjp")
    settings = AppSettings()
    assert settings.resolved_log_dir == Path("/var/lib/wbjp/logs")
    assert settings.log_file("accum") == Path("/var/lib/wbjp/logs/accum-uat.jsonl")


def test_log_dir_override_wins(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("WBJP_DATA_DIR", "/var/lib/wbjp")
    monkeypatch.setenv("WBJP_LOG_DIR", "/srv/logs")
    monkeypatch.setenv("WBJP_ENV", "prod")
    assert AppSettings().log_file("wbjp") == Path("/srv/logs/wbjp-prod.jsonl")
