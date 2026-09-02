"""足の間隔（日足・日中足）と取得元の登録簿。

分足に対応するための土台: 間隔が抽象に含まれていること、日中足が UTC の
``ts`` と暦日 ``date`` の両方を持つこと、保存場所が間隔で分かれること、
取得元を設定の名前で差し替えられること。
"""

from __future__ import annotations

import datetime as dt
from pathlib import Path
from typing import ClassVar

import polars as pl
import pytest

from wbcore.credentials import Environment
from wbcore.data.csv_replay import CsvReplayProvider, InMemoryProvider
from wbcore.data.provider import (
    INTRADAY_BAR_SCHEMA,
    Interval,
    MarketDataError,
    MarketDataProvider,
    bar_schema,
    empty_bars,
    normalize_bars,
)
from wbcore.data.registry import PROVIDERS, available, connect
from wbcore.data.store import BarStore
from wbcore.domain.models import Market


@pytest.fixture(autouse=True)
def _isolate(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    import os

    for key in list(os.environ):
        if key.startswith("WBJP_"):
            monkeypatch.delenv(key, raising=False)
    monkeypatch.setattr("wbcore.credentials.DEFAULT_ENV_FILE", tmp_path / "absent.env")
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env, *_: {})
    monkeypatch.setattr(PROVIDERS, "_items", dict(PROVIDERS._items))


def _intraday(n: int, *, start: dt.datetime | None = None, tz: str | None = "UTC") -> pl.DataFrame:
    base = start or dt.datetime(2026, 8, 3, 0, 0, tzinfo=dt.UTC)  # 09:00 JST
    stamps = [base + dt.timedelta(minutes=5 * i) for i in range(n)]
    frame = pl.DataFrame(
        {
            "ts": stamps,
            "open": [100.0] * n,
            "high": [101.0] * n,
            "low": [99.0] * n,
            "close": [100.5] * n,
            "volume": [10.0] * n,
        }
    )
    if tz is None:
        frame = frame.with_columns(pl.col("ts").dt.replace_time_zone(None))
    return frame


# --- Interval ------------------------------------------------------------


def test_interval_parse_rejects_unknown_with_candidates() -> None:
    assert Interval.parse("5m") is Interval.M5
    assert Interval.parse(Interval.D1) is Interval.D1
    with pytest.raises(ValueError, match="未知の足の間隔 '7m'。利用可能:"):
        Interval.parse("7m")


def test_interval_properties() -> None:
    assert Interval.D1.time_column == "date" and not Interval.D1.is_intraday
    assert Interval.M5.time_column == "ts" and Interval.M5.is_intraday
    assert Interval.H1.duration == dt.timedelta(hours=1)


# --- スキーマ ------------------------------------------------------------


def test_intraday_bars_carry_utc_timestamp_and_calendar_date() -> None:
    frame = normalize_bars(_intraday(3), Interval.M5)
    assert list(frame.columns) == list(INTRADAY_BAR_SCHEMA)
    assert frame["ts"].dtype == pl.Datetime("us", "UTC")
    assert frame["date"].to_list() == [dt.date(2026, 8, 3)] * 3


def test_naive_timestamps_are_treated_as_utc() -> None:
    frame = normalize_bars(_intraday(2, tz=None), Interval.M5)
    assert frame["ts"][0] == dt.datetime(2026, 8, 3, 0, 0, tzinfo=dt.UTC)


def test_exchange_timezone_is_converted_to_utc() -> None:
    tokyo = dt.datetime(2026, 8, 3, 9, 0, tzinfo=dt.timezone(dt.timedelta(hours=9)))
    frame = normalize_bars(_intraday(1, start=tokyo), Interval.M5)
    assert frame["ts"][0] == dt.datetime(2026, 8, 3, 0, 0, tzinfo=dt.UTC)


def test_intraday_duplicates_collapse_on_timestamp_last_wins() -> None:
    a = _intraday(2)
    b = _intraday(1).with_columns(pl.lit(999.0).alias("close"))
    merged = normalize_bars(pl.concat([a, b]), Interval.M5)
    assert merged.height == 2
    assert merged["close"][0] == 999.0


def test_daily_schema_is_unchanged() -> None:
    assert list(bar_schema(Interval.D1)) == ["date", "open", "high", "low", "close", "volume"]
    assert empty_bars(Interval.M5).columns[0] == "ts"


def test_missing_timestamp_column_is_an_error() -> None:
    with pytest.raises(MarketDataError, match="列が不足"):
        normalize_bars(_intraday(1).drop("ts"), Interval.M5)


# --- 保存 ----------------------------------------------------------------


def test_intraday_store_lives_beside_daily_store(tmp_path: Path) -> None:
    daily = BarStore(tmp_path)
    five = BarStore(tmp_path, Interval.M5)
    five.write("7203", _intraday(4))
    assert five.path_for("7203") == tmp_path / "5m" / "7203.parquet"
    assert daily.symbols() == [] and five.symbols() == ["7203"]
    assert five.last_date("7203") == dt.date(2026, 8, 3)
    assert five.last_timestamp("7203") == dt.datetime(2026, 8, 3, 0, 15, tzinfo=dt.UTC)
    assert daily.last_timestamp("7203") is None


def test_intraday_sync_passes_the_interval_to_the_provider(tmp_path: Path) -> None:
    provider = InMemoryProvider({"7203": _intraday(6)}, interval=Interval.M5)
    store = BarStore(tmp_path, Interval.M5)
    counts = store.sync(provider, ["7203"], dt.date(2026, 8, 1), dt.date(2026, 8, 3))
    assert counts == {"7203": 6}
    assert store.read("7203")["ts"].dtype == pl.Datetime("us", "UTC")


def test_intraday_sync_refetches_today_even_when_last_bar_is_today(tmp_path: Path) -> None:
    """日中足は「最終日が今日」でもその日の続きが来る。日足と違って再取得する。"""
    first = InMemoryProvider({"7203": _intraday(2)}, interval=Interval.M5)
    store = BarStore(tmp_path, Interval.M5)
    store.sync(first, ["7203"], dt.date(2026, 8, 3), dt.date(2026, 8, 3))
    later = InMemoryProvider({"7203": _intraday(5)}, interval=Interval.M5)
    counts = store.sync(later, ["7203"], dt.date(2026, 8, 3), dt.date(2026, 8, 3))
    assert counts == {"7203": 5}


def test_provider_rejects_unsupported_interval() -> None:
    provider = InMemoryProvider({"7203": _intraday(1)}, interval=Interval.M5)
    with pytest.raises(MarketDataError, match="1d 足に対応していません"):
        provider.fetch_bars(["7203"], dt.date(2026, 1, 1), dt.date(2026, 12, 31))


def test_csv_replay_reads_intraday_files(tmp_path: Path) -> None:
    (tmp_path / "7203.csv").write_text(
        "ts,open,high,low,close,volume\n"
        "2026-08-03T00:00:00,100,101,99,100.5,10\n"
        "2026-08-03T00:05:00,100.5,102,100,101,12\n",
        encoding="utf-8",
    )
    result = CsvReplayProvider(tmp_path).fetch_bars(
        ["7203"], dt.date(2026, 8, 3), dt.date(2026, 8, 3), interval=Interval.M5
    )
    assert result["7203"]["ts"].dtype == pl.Datetime("us", "UTC")
    assert result["7203"].height == 2


# --- 登録簿 --------------------------------------------------------------


def test_builtin_providers_are_registered() -> None:
    assert {"fred", "jquants"} <= set(available())


def test_connect_fred_needs_no_credentials() -> None:
    from wbcore.data.fred_provider import FredProvider

    provider = connect("fred", Environment.UAT, market=Market.US)
    assert isinstance(provider, FredProvider)
    assert provider.market is Market.US


def test_unknown_provider_lists_the_alternatives() -> None:
    with pytest.raises(ValueError, match="未知のデータソース 'nope'。利用可能:"):
        connect("nope", Environment.UAT, market=Market.JP)


def test_a_new_source_plugs_in_by_subclassing() -> None:
    """取得元を足す手順そのもの: 継承 → name / intervals / fetch_bars / connect → 登録。"""

    class FakeFeed(MarketDataProvider):
        name: ClassVar[str] = "fake"
        intervals: ClassVar[frozenset[Interval]] = frozenset({Interval.M1})

        @classmethod
        def connect(cls, env: Environment, *, market: Market) -> FakeFeed:
            return cls()

        def fetch_bars(self, symbols, start, end, *, interval=Interval.D1):  # type: ignore[no-untyped-def]
            self._require(interval)
            return {}

    PROVIDERS.register(FakeFeed)
    provider = connect("fake", Environment.PROD, market=Market.US)
    assert isinstance(provider, FakeFeed)
    with pytest.raises(MarketDataError, match="1d 足に対応していません"):
        provider.fetch_daily_bars(["AAPL"], dt.date(2026, 1, 1), dt.date(2026, 1, 2))


def test_test_only_providers_cannot_be_selected_from_config(tmp_path: Path) -> None:
    with pytest.raises(MarketDataError, match="設定からは選べません"):
        CsvReplayProvider.connect(Environment.UAT, market=Market.JP)
