"""データ層のテスト。

ネットワークを使うテストは ``network`` マークを付けてあり、既定では
実行しない（``uv run pytest -m network`` で実行）。
"""

from __future__ import annotations

import datetime as dt
from pathlib import Path

import polars as pl
import pytest

from wbcore.data.csv_replay import CsvReplayProvider, InMemoryProvider
from wbcore.data.provider import MarketDataError, empty_bars, normalize_bars
from wbcore.data.store import OVERLAP_DAYS, BarStore


def bars(closes: list[float], start: dt.date = dt.date(2025, 1, 6)) -> pl.DataFrame:
    """営業日っぽく平日だけ並べた足を作る。"""
    dates, current = [], start
    while len(dates) < len(closes):
        if current.weekday() < 5:
            dates.append(current)
        current += dt.timedelta(days=1)
    return pl.DataFrame(
        {
            "date": dates,
            "open": closes,
            "high": [c * 1.01 for c in closes],
            "low": [c * 0.99 for c in closes],
            "close": closes,
            "volume": [1_000_000.0] * len(closes),
        }
    )


# --------------------------------------------------------------------------
# 正規化
# --------------------------------------------------------------------------


def test_normalize_orders_by_date() -> None:
    frame = bars([100.0, 101.0, 102.0]).reverse()
    assert normalize_bars(frame)["date"].is_sorted()


def test_normalize_deduplicates_keeping_the_newest() -> None:
    """増分取得で境界日が二重になると指標の窓がずれるため、必ず1本に潰す。"""
    first = bars([100.0])
    second = first.with_columns(pl.lit(999.0).alias("close"))

    merged = normalize_bars(pl.concat([first, second]))

    assert merged.height == 1
    assert merged["close"][0] == 999.0


def test_normalize_drops_rows_without_prices() -> None:
    frame = bars([100.0, 101.0]).with_columns(
        pl.when(pl.col("close") == 101.0).then(None).otherwise(pl.col("close")).alias("close")
    )
    assert normalize_bars(frame).height == 1


def test_normalize_rejects_missing_columns() -> None:
    with pytest.raises(MarketDataError, match="列が不足"):
        normalize_bars(pl.DataFrame({"date": [dt.date(2025, 1, 6)], "close": [100.0]}))


def test_normalize_enforces_column_order() -> None:
    frame = bars([100.0]).select(["volume", "close", "low", "high", "open", "date"])
    assert normalize_bars(frame).columns == ["date", "open", "high", "low", "close", "volume"]


def test_empty_bars_has_the_canonical_schema() -> None:
    assert empty_bars().columns == ["date", "open", "high", "low", "close", "volume"]
    assert empty_bars().height == 0


# --------------------------------------------------------------------------
# BarStore
# --------------------------------------------------------------------------


def test_store_round_trips(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0, 101.0, 102.0]))

    read_back = store.read("7203")

    assert read_back.height == 3
    assert read_back.columns == ["date", "open", "high", "low", "close", "volume"]


def test_store_read_missing_symbol_returns_empty(tmp_path: Path) -> None:
    assert BarStore(tmp_path).read("9999").height == 0


def test_store_rejects_path_traversal(tmp_path: Path) -> None:
    """銘柄コードをそのままファイル名にするので、経路の脱出を防ぐ。"""
    with pytest.raises(ValueError, match="使えない文字"):
        BarStore(tmp_path).path_for("../../etc/passwd")


def test_store_upsert_appends_new_bars(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0, 101.0]))

    later = bars([102.0, 103.0], start=dt.date(2025, 1, 8))
    total = store.upsert("7203", later)

    assert total == 4
    assert store.read("7203")["close"].to_list() == [100.0, 101.0, 102.0, 103.0]


def test_store_upsert_overwrites_overlapping_dates(tmp_path: Path) -> None:
    """重ねて取り直した日は、新しい値で置き換わる（訂正の反映）。"""
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0, 101.0]))

    corrected = bars([100.0, 555.0])
    store.upsert("7203", corrected)

    read_back = store.read("7203")
    assert read_back.height == 2
    assert read_back["close"].to_list() == [100.0, 555.0]


def test_store_last_date(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    assert store.last_date("7203") is None

    store.write("7203", bars([100.0, 101.0, 102.0]))
    assert store.last_date("7203") == dt.date(2025, 1, 8)


def test_store_read_filters_by_date_range(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0, 101.0, 102.0, 103.0]))

    window = store.read("7203", start=dt.date(2025, 1, 7), end=dt.date(2025, 1, 8))

    assert window["date"].to_list() == [dt.date(2025, 1, 7), dt.date(2025, 1, 8)]


def test_store_read_many_skips_empty(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0]))

    result = store.read_many(["7203", "9999"])

    assert set(result) == {"7203"}


def test_store_lists_symbols(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0]))
    store.write("6758", bars([200.0]))
    assert store.symbols() == ["6758", "7203"]


# --------------------------------------------------------------------------
# 増分取得
# --------------------------------------------------------------------------


class RecordingProvider(InMemoryProvider):
    """どの範囲を要求されたか記録するプロバイダ。"""

    def __init__(self, data: dict[str, pl.DataFrame]) -> None:
        super().__init__(data)
        self.calls: list[tuple[list[str], dt.date, dt.date]] = []

    def fetch_bars(self, symbols, start, end):  # type: ignore[no-untyped-def]
        self.calls.append((list(symbols), start, end))
        return super().fetch_bars(symbols, start, end)


def test_sync_fetches_everything_when_store_is_empty(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    provider = RecordingProvider({"7203": bars([100.0] * 10)})

    store.sync(provider, ["7203"], dt.date(2025, 1, 6), dt.date(2025, 1, 17))

    assert len(provider.calls) == 1
    assert provider.calls[0][1] == dt.date(2025, 1, 6)
    assert store.read("7203").height == 10


def test_sync_only_requests_the_missing_tail(tmp_path: Path) -> None:
    """既に持っている期間を取り直さない。

    yfinance は連投すると遮断されるため、ここが効率の要になる。
    """
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0] * 30))  # 1/6 〜 2/14
    stored_last = store.last_date("7203")
    assert stored_last is not None

    provider = RecordingProvider({"7203": bars([100.0] * 40)})
    store.sync(provider, ["7203"], dt.date(2025, 1, 6), dt.date(2025, 2, 28))

    requested_start = provider.calls[0][1]
    assert requested_start > dt.date(2025, 1, 6), "先頭から取り直している"
    # 保存済み最終日から重ね幅ぶんだけ戻った位置を要求する
    assert requested_start == stored_last - dt.timedelta(days=OVERLAP_DAYS)


def test_sync_skips_when_already_current(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0] * 10))  # 1/6 〜 1/17
    provider = RecordingProvider({"7203": bars([100.0] * 10)})

    store.sync(provider, ["7203"], dt.date(2025, 1, 6), dt.date(2025, 1, 17))

    assert provider.calls == []


def test_sync_force_refetches_everything(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0] * 10))
    provider = RecordingProvider({"7203": bars([100.0] * 10)})

    store.sync(provider, ["7203"], dt.date(2025, 1, 6), dt.date(2025, 1, 17), force=True)

    assert provider.calls[0][1] == dt.date(2025, 1, 6)


def test_sync_groups_symbols_sharing_a_start_date(tmp_path: Path) -> None:
    """同じ開始日の銘柄は1回の問い合わせにまとめる。"""
    store = BarStore(tmp_path)
    provider = RecordingProvider({"7203": bars([100.0] * 5), "6758": bars([200.0] * 5)})

    store.sync(provider, ["7203", "6758"], dt.date(2025, 1, 6), dt.date(2025, 1, 10))

    assert len(provider.calls) == 1
    assert sorted(provider.calls[0][0]) == ["6758", "7203"]


def test_sync_survives_a_missing_symbol(tmp_path: Path) -> None:
    """一部の銘柄が取れなくても、他の銘柄の処理を止めない。"""
    store = BarStore(tmp_path)
    provider = RecordingProvider({"7203": bars([100.0] * 5)})

    counts = store.sync(provider, ["7203", "9999"], dt.date(2025, 1, 6), dt.date(2025, 1, 10))

    assert counts["7203"] == 5
    assert counts["9999"] == 0


# --------------------------------------------------------------------------
# DuckDB
# --------------------------------------------------------------------------


def test_query_spans_all_symbols(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0] * 5))
    store.write("6758", bars([200.0] * 3))

    result = store.query("SELECT symbol, count(*) AS n FROM bars GROUP BY symbol ORDER BY symbol")

    assert result.to_dicts() == [
        {"symbol": "6758", "n": 3},
        {"symbol": "7203", "n": 5},
    ]


def test_summary_reports_coverage(tmp_path: Path) -> None:
    store = BarStore(tmp_path)
    store.write("7203", bars([100.0] * 5))

    row = store.summary().row(0, named=True)

    assert row["symbol"] == "7203"
    assert row["bars"] == 5
    assert row["first"] == dt.date(2025, 1, 6)


def test_summary_on_empty_store(tmp_path: Path) -> None:
    assert BarStore(tmp_path).summary().height == 0


# --------------------------------------------------------------------------
# CSV リプレイ
# --------------------------------------------------------------------------


def test_csv_replay_reads_and_filters(tmp_path: Path) -> None:
    frame = bars([100.0, 101.0, 102.0, 103.0])
    frame.write_csv(tmp_path / "7203.csv")

    provider = CsvReplayProvider(tmp_path)
    result = provider.fetch_daily_bars(["7203"], dt.date(2025, 1, 7), dt.date(2025, 1, 8))

    assert result["7203"].height == 2


def test_csv_replay_is_deterministic(tmp_path: Path) -> None:
    """同じ入力からは必ず同じ結果。障害の再現に使う。"""
    bars([100.0, 101.0, 102.0]).write_csv(tmp_path / "7203.csv")
    provider = CsvReplayProvider(tmp_path)

    runs = [
        provider.fetch_daily_bars(["7203"], dt.date(2025, 1, 1), dt.date(2025, 12, 31))["7203"]
        for _ in range(3)
    ]
    assert all(run.equals(runs[0]) for run in runs)


def test_csv_replay_lists_available_symbols(tmp_path: Path) -> None:
    bars([100.0]).write_csv(tmp_path / "7203.csv")
    bars([100.0]).write_csv(tmp_path / "6758.csv")
    assert CsvReplayProvider(tmp_path).available_symbols() == ["6758", "7203"]


def test_csv_replay_ignores_unknown_symbol(tmp_path: Path) -> None:
    provider = CsvReplayProvider(tmp_path)
    assert provider.fetch_daily_bars(["9999"], dt.date(2025, 1, 1), dt.date(2025, 12, 31)) == {}


def test_store_accepts_index_tickers(tmp_path: Path) -> None:
    """指数のティッカーは ^ で始まる（^N225 など）。"""
    store = BarStore(tmp_path)
    assert store.path_for("^N225").name == "^N225.parquet"
    store.write("^N225", bars([100.0, 101.0]))
    assert store.read("^N225").height == 2
    assert "^N225" in store.symbols()


def test_store_still_rejects_separators(tmp_path: Path) -> None:
    for bad in ("a/b", "a\\b", "^../x"):
        with pytest.raises(ValueError, match="使えない文字"):
            BarStore(tmp_path).path_for(bad)
