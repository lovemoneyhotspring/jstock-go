"""取得元の登録簿。設定の名前で取得元を差し替えられること。"""

from __future__ import annotations

import datetime as dt
from pathlib import Path
from typing import ClassVar

import pytest

from wbcore.credentials import Environment
from wbcore.data.csv_replay import CsvReplayProvider
from wbcore.data.provider import MarketDataError, MarketDataProvider
from wbcore.data.registry import PROVIDERS, available, connect
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
    """取得元を足す手順そのもの: 継承 → name / fetch_bars / connect → 登録。"""

    class FakeFeed(MarketDataProvider):
        name: ClassVar[str] = "fake"

        @classmethod
        def connect(cls, env: Environment, *, market: Market) -> FakeFeed:
            return cls()

        def fetch_bars(self, symbols, start, end):  # type: ignore[no-untyped-def]
            return {}

    PROVIDERS.register(FakeFeed)
    provider = connect("fake", Environment.PROD, market=Market.JP)
    assert isinstance(provider, FakeFeed)
    assert provider.fetch_daily_bars(["7203"], dt.date(2026, 1, 1), dt.date(2026, 1, 2)) == {}


def test_test_only_providers_cannot_be_selected_from_config(tmp_path: Path) -> None:
    with pytest.raises(MarketDataError, match="設定からは選べません"):
        CsvReplayProvider.connect(Environment.UAT, market=Market.JP)
