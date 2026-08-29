"""積立 CLI（``accum``）のテスト。

とくに **cron から回したときに黙って止まらないこと** を確認する。
"""

from __future__ import annotations

from pathlib import Path

import pytest
from typer.testing import CliRunner

from accum.cli import app

runner = CliRunner()

CONFIG_DIR = Path(__file__).resolve().parent.parent / "config" / "accum"


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    import os

    for key in list(os.environ):
        if key.startswith("WBJP_"):
            monkeypatch.delenv(key, raising=False)
    monkeypatch.setattr("wbcore.credentials.DEFAULT_ENV_FILE", tmp_path / "absent.env")


def _accum(*args: str, config_dir: Path | None = None, env: dict[str, str] | None = None):  # type: ignore[no-untyped-def]
    directory = config_dir or CONFIG_DIR
    return runner.invoke(app, [*args, "--config-dir", str(directory)], env=env)


def test_list_shows_tactic_and_symbols() -> None:
    result = _accum("list")
    assert result.exit_code == 0
    assert "bear_stack" in result.stdout
    assert "1305.T" in result.stdout


def test_list_shows_disabled_entries() -> None:
    """止めてある戦略も一覧には出す。存在を忘れると設定を二重に足してしまう。"""
    result = _accum("list")
    assert result.exit_code == 0
    assert "—" in result.stdout


def test_strategies_lists_registered_tactics() -> None:
    result = _accum("strategies")
    assert result.exit_code == 0
    for name in ("bear_stack", "constant", "drawdown_ladder", "stack_ladder"):
        assert name in result.stdout


def test_missing_config_fails_with_a_reason(tmp_path: Path) -> None:
    result = _accum("list", config_dir=tmp_path)
    assert result.exit_code == 1
    assert "積立の設定が見つかりません" in result.stdout


def test_broken_config_fails_with_a_reason(tmp_path: Path) -> None:
    (tmp_path / "accum.toml").write_text(
        '[[tactics]]\nid = "x"\ntactic = "constant"\nsymbols = ["A"]\n'
        '[[tactics]]\nid = "y"\ntactic = "constant"\nsymbols = ["A"]\n',
        encoding="utf-8",
    )
    result = _accum("list", config_dir=tmp_path)
    assert result.exit_code == 1
    assert "二重買付" in result.stdout


def test_backtest_without_bars_exits_with_advice(tmp_path: Path) -> None:
    """足が無いときに黙って空表を出さない。"""
    (tmp_path / "accum.toml").write_text(
        '[[tactics]]\nid = "x"\ntactic = "constant"\nsymbols = ["NOSUCH"]\n', encoding="utf-8"
    )
    result = _accum("backtest", config_dir=tmp_path)
    assert result.exit_code == 1
    assert "accum sync" in result.stdout


def test_run_without_bars_does_not_touch_the_broker(tmp_path: Path) -> None:
    """足が無ければ投下額も決まらない。ブローカーに繋ぐ前に終わる。"""
    (tmp_path / "accum.toml").write_text(
        '[[tactics]]\nid = "x"\ntactic = "constant"\nsymbols = ["NOSUCH"]\n', encoding="utf-8"
    )
    result = _accum("run", "--no-sync", "--live", config_dir=tmp_path, env={"WBJP_ENV": "prod"})
    assert result.exit_code == 0
    assert "投下額のある銘柄はありません" in result.stdout


def test_commands_are_registered() -> None:
    result = runner.invoke(app, ["--help"])
    assert result.exit_code == 0
    for command in ("strategies", "list", "sync", "plan", "run", "backtest", "basket", "compare"):
        assert command in result.stdout
