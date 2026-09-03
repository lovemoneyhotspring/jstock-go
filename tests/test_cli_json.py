"""``--json``——読み手が AI のときの出力。

Rich の表は罫線・余白・色の制御文字でトークンを食う。``--json`` を付けたときは
**表を一切出さず**、標準出力に JSON を 1 個だけ書く（そのまま ``jq`` に渡せる）。

履歴がまだ 1 件も無い状態で落ちないことも合わせて確かめる。運用を始めた直後は
必ずこの状態を通るのに、``pl.col`` は列の無いフレームに当てると例外になる。
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

runner = CliRunner()


@pytest.fixture(autouse=True)
def _state(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("WBJP_ENV", "uat")
    monkeypatch.setenv("WBJP_STATE_DIR", str(tmp_path / "state"))
    monkeypatch.setenv("WBJP_LOG_DIR", str(tmp_path / "state" / "logs"))
    monkeypatch.setenv("WBJP_DATA_DIR", str(tmp_path / "data"))


def _json_of(result: object) -> dict:
    """出力全体がちょうど 1 個の JSON であることまで含めて確かめる。"""
    text = result.stdout.strip()  # type: ignore[attr-defined]
    assert text, "出力が空"
    payload = json.loads(text)
    assert isinstance(payload, dict)
    return payload


class TestDaytrade:
    def test_history_lists_kinds(self) -> None:
        from daytrade.cli import app

        payload = _json_of(runner.invoke(app, ["history", "--json"]))
        assert payload["ok"] is True
        assert payload["kinds"] == []

    def test_review_on_empty_history(self) -> None:
        from daytrade.cli import app

        payload = _json_of(runner.invoke(app, ["review", "--json"]))
        assert payload == {"ok": True, "rows": [], "totals": []}


class TestWbjp:
    def test_review_on_empty_history(self) -> None:
        """列すら無い空フレームで落ちない（運用開始直後に必ず通る道）。"""
        from wbjp.cli import app

        payload = _json_of(runner.invoke(app, ["review", "--json"]))
        assert payload == {"ok": True, "rows": [], "totals": []}

    def test_evaluate_without_screens(self) -> None:
        from wbjp.cli import app

        payload = _json_of(runner.invoke(app, ["evaluate", "--json"]))
        assert payload["ok"] is True
        assert payload["days"] == []

    def test_history_lists_kinds(self) -> None:
        from wbjp.cli import app

        payload = _json_of(runner.invoke(app, ["history", "--json"]))
        assert payload["kinds"] == []


class TestAccum:
    def test_history_lists_kinds(self) -> None:
        from accum.cli import app

        payload = _json_of(runner.invoke(app, ["history", "--json"]))
        assert payload["ok"] is True
        assert payload["kinds"] == []

    def test_evaluate_without_decisions(self) -> None:
        from accum.cli import app

        payload = _json_of(runner.invoke(app, ["evaluate", "--json"]))
        assert payload["ok"] is True
        assert payload["rows"] == [] and payload["summary"] == []


def test_json_output_has_no_table_drawing_characters() -> None:
    """``--json`` のときに Rich の表が混ざると ``jq`` に渡せなくなる。"""
    from daytrade.cli import app

    result = runner.invoke(app, ["history", "--json"])
    assert not set("┏┓┗┛━┃│─╭╮╰╯") & set(result.stdout)
