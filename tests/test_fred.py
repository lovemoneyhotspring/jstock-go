"""FRED（米国指数の終値）の読み取りと、日足スキーマへの変換。"""

from __future__ import annotations

import datetime as dt

import pytest

from wbcore.credentials import Environment
from wbcore.data.fred_provider import (
    FredProvider,
    closes_to_bars,
    fetch_closes,
    parse_csv,
    series_id,
)
from wbcore.data.provider import Interval, MarketDataError
from wbcore.domain.models import Market

SAMPLE = """observation_date,VIXCLS
2026-08-27,14.51
2026-08-28,.
2026-08-31,14.92
"""


class _Response:
    def __init__(self, status: int, text: str = "") -> None:
        self.status_code = status
        self.text = text


class _Session:
    def __init__(self, status: int = 200, text: str = SAMPLE) -> None:
        self.calls: list[dict[str, str]] = []
        self._response = _Response(status, text)

    def get(self, url: str, *, params: dict[str, str], timeout: int) -> _Response:
        self.calls.append(params)
        return self._response


def test_parse_csv_drops_missing_days_and_sorts() -> None:
    frame = parse_csv(SAMPLE)
    assert frame["date"].to_list() == [dt.date(2026, 8, 27), dt.date(2026, 8, 31)]
    assert frame["close"].to_list() == [14.51, 14.92]


def test_parse_csv_rejects_non_csv() -> None:
    with pytest.raises(MarketDataError, match="CSV"):
        parse_csv("<html>challenge</html>")


def test_closes_to_bars_fills_ohlc_with_close() -> None:
    bars = closes_to_bars(parse_csv(SAMPLE))
    assert bars.columns == ["date", "open", "high", "low", "close", "volume"]
    assert bars["open"].to_list() == bars["close"].to_list()
    assert bars["volume"].to_list() == [0.0, 0.0]


def test_series_id_maps_index_tickers() -> None:
    assert series_id("^GSPC") == "SP500"
    assert series_id("^VIX") == "VIXCLS"
    assert series_id("NASDAQCOM") == "NASDAQCOM"  # FRED の ID はそのまま通す
    with pytest.raises(MarketDataError, match="対応していない"):
        series_id("^N225")


def test_fetch_closes_passes_the_period_and_reports_404() -> None:
    session = _Session()
    fetch_closes("VIXCLS", dt.date(2026, 8, 27), dt.date(2026, 8, 31), session=session)
    assert session.calls == [{"id": "VIXCLS", "cosd": "2026-08-27", "coed": "2026-08-31"}]

    with pytest.raises(MarketDataError, match="系列がありません"):
        fetch_closes("NOPE", dt.date(2026, 8, 1), dt.date(2026, 8, 2), session=_Session(404))


def test_provider_is_us_only_and_daily_only() -> None:
    with pytest.raises(MarketDataError, match="US"):
        FredProvider(market=Market.JP)
    provider = FredProvider.connect(Environment.UAT, market=Market.US)
    with pytest.raises(MarketDataError, match="対応していません"):
        provider.fetch_bars(["^VIX"], dt.date(2026, 8, 1), dt.date(2026, 8, 2), interval=Interval.M5)


def test_provider_returns_bars_keyed_by_the_configured_symbol() -> None:
    provider = FredProvider(session=_Session())
    result = provider.fetch_bars(["^VIX", " ^VIX "], dt.date(2026, 8, 27), dt.date(2026, 8, 31))
    assert list(result) == ["^VIX"]
    assert result["^VIX"]["close"].to_list() == [14.51, 14.92]
