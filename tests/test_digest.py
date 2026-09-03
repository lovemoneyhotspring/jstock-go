"""日次ダイジェスト——AI が最初に読む 1 ファイル。"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from wbcore import digest


@pytest.fixture(autouse=True)
def _clean() -> None:
    digest.reset()


def _read(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]


def test_run_is_summarised_to_one_line(tmp_path: Path) -> None:
    digest.start_run(
        app="daytrade",
        env="prod",
        command="plan",
        run_id="abc123",
        state_dir=tmp_path,
        day="2026-09-03",
    )
    digest.note(candidates=3682, eligible=952)
    digest.flush()

    path = digest.digest_path(tmp_path, "prod", "2026-09-03")
    (record,) = _read(path)
    assert record["app"] == "daytrade"
    assert record["command"] == "plan"
    assert record["run_id"] == "abc123"
    assert record["outcome"] == "ok"
    assert record["candidates"] == 3682 and record["eligible"] == 952
    assert "anomalies" not in record  # 正常な実行には付かない
    assert isinstance(record["dur_ms"], int)


def test_anomalies_are_the_only_thing_worth_drilling_into(tmp_path: Path) -> None:
    """異常のある実行だけが ``anomalies`` を持つ。AI の絞り込みはこれ 1 つで済む。"""
    for command, broken in (("plan", False), ("open", True)):
        digest.reset()
        digest.start_run(
            app="daytrade",
            env="prod",
            command=command,
            run_id=command,
            state_dir=tmp_path,
            day="2026-09-03",
        )
        if broken:
            digest.anomaly("daytrade.carry", "2 銘柄が未返済のまま")
        digest.flush()

    records = _read(digest.digest_path(tmp_path, "prod", "2026-09-03"))
    assert len(records) == 2
    flagged = [r for r in records if r.get("anomalies")]
    assert [r["command"] for r in flagged] == ["open"]
    assert flagged[0]["anomalies"] == ["daytrade.carry: 2 銘柄が未返済のまま"]


def test_failure_marks_outcome_error(tmp_path: Path) -> None:
    digest.start_run(
        app="accum", env="uat", command="run", run_id="x", state_dir=tmp_path, day="2026-09-03"
    )
    digest.fail("accum.crash", "ValueError: boom")
    digest.flush()

    (record,) = _read(digest.digest_path(tmp_path, "uat", "2026-09-03"))
    assert record["outcome"] == "error"
    assert record["anomalies"] == ["accum.crash: ValueError: boom"]


def test_second_flush_does_not_duplicate(tmp_path: Path) -> None:
    """``atexit`` と明示の呼び出しが重なっても 1 行きり。"""
    digest.start_run(
        app="wbjp", env="uat", command="run", run_id="y", state_dir=tmp_path, day="2026-09-03"
    )
    digest.flush()
    digest.flush()
    assert len(_read(digest.digest_path(tmp_path, "uat", "2026-09-03"))) == 1


def test_runs_of_different_apps_share_one_file(tmp_path: Path) -> None:
    """アプリを分けない。「今日どう動いたか」を 1 ファイルで読めるようにするため。"""
    for app in ("jquants", "daytrade", "accum"):
        digest.reset()
        digest.start_run(
            app=app, env="prod", command="run", run_id=app, state_dir=tmp_path, day="2026-09-03"
        )
        digest.flush()

    records = _read(digest.digest_path(tmp_path, "prod", "2026-09-03"))
    assert [r["app"] for r in records] == ["jquants", "daytrade", "accum"]


def test_note_without_a_run_is_harmless() -> None:
    """CLI の外（テストやライブラリ利用）で呼ばれても落ちない。"""
    digest.note(x=1)
    digest.anomaly("nope")
    digest.flush()


def test_cli_writes_a_digest_line(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """実際の CLI 実行が 1 行残す（配線の確認）。"""
    from daytrade.cli import app

    state = tmp_path / "state"
    monkeypatch.setenv("WBJP_STATE_DIR", str(state))
    monkeypatch.setenv("WBJP_LOG_DIR", str(state / "logs"))
    monkeypatch.setenv("WBJP_ENV", "uat")

    CliRunner().invoke(app, ["status", "--date", "2026-09-03"])
    digest.flush()

    files = list((state / "digest").glob("*.jsonl"))
    assert len(files) == 1
    (record,) = _read(files[0])
    assert record["app"] == "daytrade" and record["command"] == "status"
