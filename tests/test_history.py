"""追記専用の履歴（wbcore.history）のテスト。"""

from __future__ import annotations

import datetime as dt
from pathlib import Path

import polars as pl
from rich.console import Console

from wbcore.history import KEY_COLUMNS, HistoryStore, show_history

DAY = dt.date(2026, 9, 3)
AT = dt.datetime(2026, 9, 3, 0, 1, 0, tzinfo=dt.UTC)


def _frame(symbols: list[str]) -> pl.DataFrame:
    return pl.DataFrame({"symbol": symbols, "gap": [-0.01 * i for i in range(len(symbols))]})


def test_append_never_overwrites_and_keys_come_first(tmp_path: Path) -> None:
    store = HistoryStore(tmp_path / "history")
    first = store.append("ranking", _frame(["A", "B"]), day=DAY, run_id="r1", at=AT)
    second = store.append("ranking", _frame(["C"]), day=DAY, run_id="r1", at=AT)  # 同名 → 枝番
    third = store.append("ranking", _frame(["D"]), day=DAY, run_id="r2", at=AT)

    assert first != second and second != third
    assert first.name == "2026-09-03T000100Z-r1.parquet"
    assert second.name == "2026-09-03T000100Z-r1_2.parquet"
    assert [p.name for p in store.files("ranking")] == [first.name, second.name, third.name]
    assert store.read("ranking")["symbol"].to_list() == ["A", "B", "C", "D"]  # 追記した順
    assert len(store.files("ranking")) == 3
    frame = pl.read_parquet(first)
    assert frame.columns[:3] == list(KEY_COLUMNS)
    assert frame["day"].to_list() == [DAY, DAY]
    assert frame["run_id"].to_list() == ["r1", "r1"]
    assert frame["recorded_at"].dtype == pl.Datetime("us", "UTC")


def test_read_concats_by_period_and_tolerates_new_columns(tmp_path: Path) -> None:
    store = HistoryStore(tmp_path / "history")
    store.append("ranking", _frame(["A"]), day=dt.date(2026, 9, 1), run_id="a", at=AT)
    store.append("ranking", _frame(["B"]), day=dt.date(2026, 9, 2), run_id="b", at=AT)
    # 後から列が増えても古いファイルは読める
    newer = _frame(["C"]).with_columns(pl.lit(True).alias("picked"))
    store.append("ranking", newer, day=dt.date(2026, 9, 3), run_id="c", at=AT)

    everything = store.read("ranking")
    assert everything["symbol"].to_list() == ["A", "B", "C"]
    assert everything["picked"].to_list() == [None, None, True]
    middle = store.read("ranking", start=dt.date(2026, 9, 2), end=dt.date(2026, 9, 2))
    assert middle["symbol"].to_list() == ["B"]
    assert store.days("ranking") == [dt.date(2026, 9, 1), dt.date(2026, 9, 2), dt.date(2026, 9, 3)]
    assert store.read("unknown").height == 0 and store.read("unknown").width == 0


def test_latest_keeps_only_the_last_run_of_the_day(tmp_path: Path) -> None:
    store = HistoryStore(tmp_path / "history")
    store.append("ranking", _frame(["A", "B"]), day=DAY, run_id="0901", at=AT)
    later = AT + dt.timedelta(minutes=3)
    store.append("ranking", _frame(["C"]), day=DAY, run_id="0904", at=later)

    latest = store.latest("ranking", DAY)
    assert latest["run_id"].to_list() == ["0904"]
    assert store.read("ranking").height == 3  # 前の実行も消えていない


def test_empty_frame_is_still_recorded(tmp_path: Path) -> None:
    store = HistoryStore(tmp_path / "history")
    path = store.append("ranking", _frame([]), day=DAY, run_id="r", at=AT)
    frame = pl.read_parquet(path)
    assert frame.height == 0 and frame.columns[:3] == list(KEY_COLUMNS)
    summary = store.summary()
    assert [(s.kind, s.files, s.first_day) for s in summary] == [("ranking", 1, DAY)]


def test_show_history_lists_kinds_and_exports_csv(tmp_path: Path) -> None:
    store = HistoryStore(tmp_path / "history")
    store.append("ranking", _frame(["A", "B"]), day=DAY, run_id="r", at=AT)
    console = Console(record=True, width=200)

    show_history(console, store, None)
    assert "ranking" in console.export_text()

    out = tmp_path / "out" / "ranking.csv"
    show_history(console, store, "ranking", csv_path=out, limit=1)
    lines = out.read_text(encoding="utf-8").splitlines()
    assert lines[0].startswith("day,run_id,recorded_at,symbol,gap")
    assert len(lines) == 3
    assert "表示 1" in console.export_text()
