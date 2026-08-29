"""収集設定がウォッチリストを銘柄の源にする。"""

from __future__ import annotations

from pathlib import Path

import pytest

from wbcore.data.webull_watchlist import (
    Watchlist,
    WatchlistItem,
    index_ticker,
    is_index,
    market_of,
)
from wbcore.domain.models import Market
from wbjp.cli import _collect_symbols
from wbjp.config import load_config


def test_market_of_uses_exchange_code_then_symbol_shape() -> None:
    assert market_of(WatchlistItem("452A", exchange="TSE")) is Market.JP
    assert market_of(WatchlistItem("AAPL", exchange="NASDAQ")) is Market.US
    # 未知の取引所コードは銘柄コードの形で推定
    assert market_of(WatchlistItem("7203", exchange="??")) is Market.JP
    assert market_of(WatchlistItem("452A")) is Market.JP
    assert market_of(WatchlistItem("BRK.B")) is Market.US
    assert market_of(WatchlistItem("MSFT")) is Market.US
    assert market_of(WatchlistItem("^N225")) is None
    # 実機で見た値: 米国 ETF は PSE（NYSE Arca）、指数は INDEXNASDAQ / SP
    assert market_of(WatchlistItem("SCHD", exchange="PSE")) is Market.US
    assert market_of(WatchlistItem("IXIC", exchange="INDEXNASDAQ")) is None
    assert is_index(WatchlistItem("SPX", exchange="SP"))
    assert index_ticker(WatchlistItem("SPX", exchange="SP")) == "^GSPC"
    assert index_ticker(WatchlistItem("IXIC", exchange="INDEXNASDAQ")) == "^IXIC"
    assert index_ticker(WatchlistItem("FOO", exchange="INDEXX")) == "^FOO"


def _config(tmp_path: Path, *, watchlists: str) -> Path:
    (tmp_path / "settings.toml").write_text(
        "[universe]\n"
        'market = "JP"\n'
        'symbols_file = "universe.txt"\n'
        'symbols = ["7203"]\n'
        f"watchlists = {watchlists}\n",
        encoding="utf-8",
    )
    (tmp_path / "universe.txt").write_text("6758\n", encoding="utf-8")
    return tmp_path


def _fake_lists(monkeypatch: pytest.MonkeyPatch, lists: list[Watchlist]) -> None:
    class Fake:
        @classmethod
        def connect(cls, env):  # type: ignore[no-untyped-def]
            return cls()

        def lists(self, *, with_items=True):  # type: ignore[no-untyped-def]
            return lists

    monkeypatch.setattr("wbcore.data.webull_watchlist.WebullWatchlists", Fake)


def test_watchlist_symbols_join_the_universe_and_are_persisted(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _fake_lists(
        monkeypatch,
        [
            Watchlist(
                "1",
                "日本株",
                items=[
                    WatchlistItem("452A", "iShares", "TSE"),
                    WatchlistItem("6758", "ソニー", "TSE"),  # 既にある
                    WatchlistItem("AAPL", "Apple", "NASDAQ"),  # 別市場
                    WatchlistItem("SPX", "S&P 500", "SP"),  # 指数はどの市場でも蓄積
                ],
            ),
            Watchlist("2", "米国株", items=[WatchlistItem("MSFT", "", "NASDAQ")]),
        ],
    )
    config = load_config(_config(tmp_path, watchlists='["*"]'))
    symbols = _collect_symbols(config)
    assert symbols == ["7203", "6758", "452A", "^GSPC"]
    # universe.txt に書き足され、次回は API が落ちても残る
    text = (tmp_path / "universe.txt").read_text(encoding="utf-8")
    assert "6758" in text and "452A" in text and "AAPL" not in text


def test_named_watchlists_only(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    _fake_lists(
        monkeypatch,
        [
            Watchlist("1", "日本株", items=[WatchlistItem("452A", "", "TSE")]),
            Watchlist("2", "候補", items=[WatchlistItem("9984", "", "TSE")]),
        ],
    )
    config = load_config(_config(tmp_path, watchlists='["候補"]'))
    assert _collect_symbols(config) == ["7203", "6758", "9984"]


def test_watchlist_failure_falls_back_to_the_file(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    class Broken:
        @classmethod
        def connect(cls, env):  # type: ignore[no-untyped-def]
            raise RuntimeError("接続できない")

    monkeypatch.setattr("wbcore.data.webull_watchlist.WebullWatchlists", Broken)
    config = load_config(_config(tmp_path, watchlists='["*"]'))
    assert _collect_symbols(config) == ["7203", "6758"]


def test_no_watchlists_means_no_api_call(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    class MustNotConnect:
        @classmethod
        def connect(cls, env):  # type: ignore[no-untyped-def]
            raise AssertionError("呼ばれてはいけない")

    monkeypatch.setattr("wbcore.data.webull_watchlist.WebullWatchlists", MustNotConnect)
    config = load_config(_config(tmp_path, watchlists="[]"))
    assert _collect_symbols(config) == ["7203", "6758"]
