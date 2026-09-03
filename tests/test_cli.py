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


def _write_bars(data_dir: Path, symbols: list[str], days: int = 120) -> None:
    """右肩上がりの日足を書く（sma_cross が意見を出す長さ）。"""
    import datetime as dt

    import polars as pl

    from wbcore.data.store import BarStore

    store = BarStore(data_dir / "bars")
    dates: list[dt.date] = []
    cursor = dt.date(2026, 9, 2)
    while len(dates) < days:
        if cursor.weekday() < 5:
            dates.append(cursor)
        cursor -= dt.timedelta(days=1)
    dates.reverse()
    for symbol in symbols:
        close = [1000.0 + 2.0 * i for i in range(days)]
        frame = pl.DataFrame(
            {
                "date": dates,
                "open": [c - 1 for c in close],
                "high": [c + 5 for c in close],
                "low": [c - 5 for c in close],
                "close": close,
                "volume": [1_000_000.0] * days,
            }
        )
        store.write(symbol, frame)


def test_screen_appends_history_and_history_command_reads_it(tmp_path: Path) -> None:
    import polars as pl

    _write_bars(tmp_path / "data", ["7203", "6758", "9984", "8306", "6861"])
    env = {
        "WBJP_DATA_DIR": str(tmp_path / "data"),
        "WBJP_STATE_DIR": str(tmp_path / "state"),
        "WBJP_LOG_DIR": str(tmp_path / "state" / "logs"),  # .env の絶対パスに引きずられない
    }

    result = runner.invoke(app, ["screen", "--config-dir", str(CONFIG_DIR)], env=env)
    assert result.exit_code == 0, result.output
    files = sorted((tmp_path / "state" / "wbjp" / "history" / "screen").glob("*.parquet"))
    assert len(files) == 1
    frame = pl.read_parquet(files[0])
    assert frame.columns[:3] == ["day", "run_id", "recorded_at"]
    assert {"rank", "symbol", "score", "passed", "adopted", "close"} <= set(frame.columns)
    assert frame.height >= 1 and frame["passed"].dtype == pl.Boolean

    result = runner.invoke(app, ["screen", "--config-dir", str(CONFIG_DIR), "--no-save"], env=env)
    assert result.exit_code == 0, result.output
    assert len(list((tmp_path / "state" / "wbjp" / "history" / "screen").glob("*.parquet"))) == 1

    listing = runner.invoke(app, ["history", "--config-dir", str(CONFIG_DIR)], env=env)
    assert listing.exit_code == 0 and "screen" in listing.output
    out = tmp_path / "screen.csv"
    detail = runner.invoke(
        app, ["history", "screen", "--config-dir", str(CONFIG_DIR), "--csv", str(out)], env=env
    )
    assert detail.exit_code == 0, detail.output
    lines = out.read_text(encoding="utf-8").splitlines()
    assert lines[0].startswith("day,run_id,recorded_at,rank,symbol,score,passed,adopted")
    assert len(lines) == 1 + frame.height and any(",7203," in line for line in lines[1:])
