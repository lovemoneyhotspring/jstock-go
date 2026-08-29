"""CLI のテスト。

とくに **cron から回したときに黙って止まらないこと** を確認する。
"""

from __future__ import annotations

from pathlib import Path

import pytest
from typer.testing import CliRunner

from wbjp.cli import app

runner = CliRunner()

CONFIG_DIR = Path(__file__).resolve().parent.parent / "config"


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    import os

    for key in list(os.environ):
        if key.startswith("WBJP_"):
            monkeypatch.delenv(key, raising=False)
    monkeypatch.setattr("wbcore.credentials.DEFAULT_ENV_FILE", tmp_path / "absent.env")


def _run(*args: str, env: dict[str, str] | None = None):  # type: ignore[no-untyped-def]
    return runner.invoke(app, ["run", "--config-dir", str(CONFIG_DIR), *args], env=env)


def test_prod_live_without_yes_fails_loudly_when_not_interactive() -> None:
    """cron には stdin が無い。

    確認プロンプトで黙って Abort するのではなく、--yes が要ることを
    ログに残して落ちる。
    """
    result = _run("--live", env={"WBJP_ENV": "prod"})

    assert result.exit_code == 1
    assert "--yes" in result.output


def _capture_build_broker(monkeypatch: pytest.MonkeyPatch) -> list[bool]:
    """``_build_broker`` の呼び出しを記録し、そこで実行を止める。

    ここまで到達していれば確認プロンプトは通過している。dry-run でも
    ブローカーには接続する（残高と建玉が無いと差分が出せない）ため、
    到達したこと自体は発注を意味しない。``live`` 引数の方を見る。
    """
    calls: list[bool] = []

    def _fake(config, live):  # type: ignore[no-untyped-def]
        calls.append(live)
        raise RuntimeError("ここで止める")

    monkeypatch.setattr("wbjp.cli._build_broker", _fake)
    return calls


def test_dry_run_does_not_require_yes(monkeypatch: pytest.MonkeyPatch) -> None:
    """--live が無ければ確認は不要。発注もしない。"""
    calls = _capture_build_broker(monkeypatch)

    result = _run(env={"WBJP_ENV": "prod"})

    assert calls == [False], "dry-run なのに発注が許可されている"
    assert "--yes" not in result.output


def test_yes_flag_skips_the_confirmation(monkeypatch: pytest.MonkeyPatch) -> None:
    """--yes を付ければ非対話でも確認で止まらず、発注が許可される。"""
    calls = _capture_build_broker(monkeypatch)

    _run("--live", "--yes", env={"WBJP_ENV": "prod"})

    assert calls == [True], "--yes を付けても発注が許可されていない"


# --------------------------------------------------------------------------
# 積立
# --------------------------------------------------------------------------
