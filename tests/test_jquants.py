"""J-Quants 取得元のテスト。HTTP は偽のセッションで差し替え、実接続はしない。"""

from __future__ import annotations

import datetime as dt
from typing import Any, ClassVar

import polars as pl
import pytest

from wbcore.credentials import Environment, MissingCredentialsError
from wbcore.data.jquants_provider import (
    API_KEY_VAR,
    JQuantsProvider,
    RateLimited,
    to_jquants_code,
)
from wbcore.data.provider import MarketDataError
from wbcore.data.registry import available, connect, default_provider
from wbcore.domain.models import Market


class FakeResponse:
    def __init__(self, status: int, payload: Any) -> None:
        self.status_code = status
        self._payload = payload
        self.text = str(payload)

    def json(self) -> Any:
        return self._payload


class FakeSession:
    """呼ばれた順に用意した応答を返す。問い合わせも記録する。"""

    def __init__(self, responses: list[FakeResponse]) -> None:
        self.responses = list(responses)
        self.calls: list[tuple[str, dict[str, str]]] = []

    def get(self, url: str, params: dict[str, str], timeout: int) -> FakeResponse:
        self.calls.append((url, dict(params)))
        return self.responses.pop(0)


def _row(date: str, close: float, **extra: Any) -> dict[str, Any]:
    return {
        "Date": date,
        "Code": "72030",
        "O": close * 2,  # 未調整を誤って使っていないことを検出する
        "H": close * 2,
        "L": close * 2,
        "C": close * 2,
        "Vo": 1,
        "AdjO": close,
        "AdjH": close * 1.01,
        "AdjL": close * 0.99,
        "AdjC": close,
        "AdjVo": 1000,
        **extra,
    }


def provider(*responses: FakeResponse, **kwargs: Any) -> tuple[JQuantsProvider, FakeSession]:
    session = FakeSession(list(responses))
    return JQuantsProvider(
        "k" * 16, session=session, max_attempts=3, rate_per_minute=0, **kwargs
    ), session


# --------------------------------------------------------------------------
# 銘柄コード
# --------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("symbol", "expected"),
    [
        ("7203", ("7203", False)),
        ("7203.T", ("7203", False)),
        (" 452a.t ", ("452A", False)),
        ("72030", ("72030", False)),
        ("^TOPIX", ("0000", True)),
        ("^0028", ("0028", True)),
    ],
)
def test_to_jquants_code(symbol: str, expected: tuple[str, bool]) -> None:
    assert to_jquants_code(symbol) == expected


@pytest.mark.parametrize("symbol", ["", "AAPL", "^N225", "^GSPC", "BRK.B"])
def test_to_jquants_code_rejects_non_tse(symbol: str) -> None:
    with pytest.raises(ValueError):
        to_jquants_code(symbol)


# --------------------------------------------------------------------------
# 取得
# --------------------------------------------------------------------------


def test_fetch_uses_adjusted_prices_and_normal_schema() -> None:
    p, session = provider(
        FakeResponse(200, {"data": [_row("2026-08-03", 100.0), _row("2026-08-04", 101.0)]})
    )
    result = p.fetch_bars(["7203.T"], dt.date(2026, 8, 1), dt.date(2026, 8, 5))

    frame = result["7203.T"]
    assert frame.columns == ["date", "open", "high", "low", "close", "volume"]
    assert frame["close"].to_list() == [100.0, 101.0]
    assert frame["volume"].to_list() == [1000.0, 1000.0]
    assert frame["date"].dtype == pl.Date

    url, params = session.calls[0]
    assert url.endswith("/equities/bars/daily")
    assert params == {"code": "7203", "from": "2026-08-01", "to": "2026-08-05"}


def test_fetch_follows_pagination_key() -> None:
    p, session = provider(
        FakeResponse(200, {"data": [_row("2026-08-03", 100.0)], "pagination_key": "p2"}),
        FakeResponse(200, {"data": [_row("2026-08-04", 101.0)]}),
    )
    result = p.fetch_bars(["7203"], dt.date(2026, 8, 1), dt.date(2026, 8, 5))
    assert result["7203"].height == 2
    assert session.calls[1][1]["pagination_key"] == "p2"


def test_fetch_drops_non_trading_days_and_omits_empty_symbols() -> None:
    p, _ = provider(
        FakeResponse(
            200,
            {
                "data": [
                    _row("2026-08-03", 100.0),
                    {"Date": "2026-08-04", "Code": "72030", "AdjO": None, "AdjC": None},
                ]
            },
        ),
        FakeResponse(200, {"data": []}),
    )
    result = p.fetch_bars(["7203", "6758"], dt.date(2026, 8, 1), dt.date(2026, 8, 5))
    assert set(result) == {"7203"}
    assert result["7203"].height == 1


def test_index_uses_indices_endpoint_and_zero_volume() -> None:
    p, session = provider(
        FakeResponse(
            200, {"data": [{"Date": "2026-08-03", "Code": "0000", "O": 1, "H": 2, "L": 1, "C": 2}]}
        )
    )
    result = p.fetch_bars(["^TOPIX"], dt.date(2026, 8, 1), dt.date(2026, 8, 5))
    assert session.calls[0][0].endswith("/indices/bars/daily")
    assert session.calls[0][1]["code"] == "0000"
    assert result["^TOPIX"]["volume"].to_list() == [0.0]


def test_unsupported_symbols_are_skipped_not_fatal() -> None:
    """米国の指数が混ざっていても、日本株は取れる。"""
    p, session = provider(FakeResponse(200, {"data": [_row("2026-08-03", 100.0)]}))
    result = p.fetch_bars(["^GSPC", "7203"], dt.date(2026, 8, 1), dt.date(2026, 8, 5))
    assert set(result) == {"7203"}
    assert len(session.calls) == 1


def test_rate_limit_is_retried(monkeypatch: pytest.MonkeyPatch) -> None:
    import wbcore.data.jquants_client as mod

    # 429 の待ち（本来 1 分）を潰す
    monkeypatch.setattr(mod, "RATE_LIMIT_WAIT", 0.0)
    p, session = provider(
        FakeResponse(429, {"message": "too many"}),
        FakeResponse(200, {"data": [_row("2026-08-03", 100.0)]}),
    )
    result = p.fetch_bars(["7203"], dt.date(2026, 8, 1), dt.date(2026, 8, 5))
    assert result["7203"].height == 1
    assert len(session.calls) == 2


def test_client_error_is_not_retried() -> None:
    p, session = provider(FakeResponse(401, {"message": "bad key"}))
    with pytest.raises(MarketDataError, match="401"):
        p.fetch_bars(["7203"], dt.date(2026, 8, 1), dt.date(2026, 8, 5))
    assert len(session.calls) == 1


def test_rate_limited_is_a_market_data_error() -> None:
    assert issubclass(RateLimited, MarketDataError)


# --------------------------------------------------------------------------
# 組み立て
# --------------------------------------------------------------------------


def test_refuses_us_market() -> None:
    with pytest.raises(MarketDataError, match="JP"):
        JQuantsProvider("k" * 16, market=Market.US)


def test_connect_reads_api_key_from_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(API_KEY_VAR, "secret-key-value")
    p = connect("jquants", Environment.PROD, market=Market.JP)
    assert isinstance(p, JQuantsProvider)
    assert p.market is Market.JP


def test_connect_without_api_key_explains(monkeypatch: pytest.MonkeyPatch, tmp_path: Any) -> None:
    monkeypatch.delenv(API_KEY_VAR, raising=False)
    monkeypatch.chdir(tmp_path)  # .env が無い場所
    with pytest.raises(MissingCredentialsError, match=API_KEY_VAR):
        connect("jquants", Environment.UAT, market=Market.JP)


def test_registered_and_default_per_market() -> None:
    assert "jquants" in available()
    assert default_provider(Market.JP) == "jquants"
    assert default_provider(Market.US) == "fred"


def test_rate_limit_honours_retry_after_header(monkeypatch: pytest.MonkeyPatch) -> None:
    import wbcore.data.jquants_client as mod

    monkeypatch.setattr(mod, "RATE_LIMIT_WAIT", 99.0)
    slept: list[float] = []
    monkeypatch.setattr(mod.time, "sleep", lambda s: slept.append(s))

    class WithHeader(FakeResponse):
        headers: ClassVar[dict[str, str]] = {"Retry-After": "0"}

    p, session = provider(
        WithHeader(429, {"message": "too many"}),
        FakeResponse(200, {"data": [_row("2026-08-03", 100.0)]}),
    )
    assert p.fetch_bars(["7203"], dt.date(2026, 8, 1), dt.date(2026, 8, 5))["7203"].height == 1
    assert len(session.calls) == 2


def test_throttle_spaces_requests(monkeypatch: pytest.MonkeyPatch) -> None:
    from wbcore.data.jquants_client import Throttle

    clock = {"t": 100.0}
    slept: list[float] = []
    monkeypatch.setattr("wbcore.data.jquants_client.time.monotonic", lambda: clock["t"])

    def sleep(s: float) -> None:
        slept.append(s)
        clock["t"] += s

    monkeypatch.setattr("wbcore.data.jquants_client.time.sleep", sleep)
    t = Throttle(120)  # 0.5 秒間隔
    t.wait()  # 最初は待たない
    t.wait()
    t.wait()
    assert slept == [0.5, 0.5]
    assert Throttle(0).interval == 0
