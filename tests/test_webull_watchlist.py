"""Webull ウォッチリストの応答の読み取りと、収集用リストへの書き出し。"""

from __future__ import annotations

from pathlib import Path

from wbcore.data.webull_watchlist import (
    Watchlist,
    WatchlistItem,
    parse_instruments,
    parse_watchlists,
    write_universe,
)
from wbjp.config import read_symbols_file


def test_parse_watchlists_accepts_snake_and_camel_case_and_wrappers() -> None:
    assert parse_watchlists([{"watchlist_id": "1", "name": "JP", "sort": 2}]) == [
        Watchlist("1", "JP", 2)
    ]
    camel = parse_watchlists({"data": [{"watchlistId": 7, "name": "US", "sortOrder": "1"}]})
    assert camel[0].id == "7" and camel[0].name == "US" and camel[0].sort == 1
    assert parse_watchlists({"data": [{"name": "id が無い"}]}) == []
    assert parse_watchlists("garbage") == []


def test_parse_instruments_keeps_symbol_name_exchange() -> None:
    items = parse_instruments(
        {
            "watchlist_id": "1",
            "instruments": [
                {
                    "symbol": "452A",
                    "name": "iShares S&P500",
                    "exchange_code": "TSE",
                    "instrument_id": 9,
                },
                {"symbol": "AAPL", "exchangeCode": "NASDAQ"},
                {"name": "symbol が無い"},
            ],
        }
    )
    assert [i.symbol for i in items] == ["452A", "AAPL"]
    assert items[0].exchange == "TSE" and items[0].instrument_id == "9"
    assert items[1].exchange == "NASDAQ" and items[1].name == ""


def test_write_universe_overwrites_by_default(tmp_path: Path) -> None:
    path = tmp_path / "universe.txt"
    path.write_text("7203\n", encoding="utf-8")
    written = write_universe(
        path, [WatchlistItem("452A", "iShares"), WatchlistItem("6758")], source="JP"
    )
    assert written == ["452A", "6758"]
    assert read_symbols_file(path) == ["452A", "6758"]
    assert "iShares" in path.read_text(encoding="utf-8")


def test_write_universe_can_merge_with_the_existing_list(tmp_path: Path) -> None:
    path = tmp_path / "universe.txt"
    path.write_text("7203  # トヨタ\n452A\n", encoding="utf-8")
    written = write_universe(
        path, [WatchlistItem("452A"), WatchlistItem("9984")], source="JP", merge_with=path
    )
    assert written == ["7203", "452A", "9984"]
    assert read_symbols_file(path) == ["7203", "452A", "9984"]
