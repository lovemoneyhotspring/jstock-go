"""実行品質——想定と実績の差を、上書きされない形で残す。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import pytest

from wbcore import execution
from wbcore.execution import ReasonCode
from wbcore.history import HistoryStore

DAY = dt.date(2026, 9, 3)


@pytest.fixture(autouse=True)
def _clean() -> None:
    execution.reset()


class TestSlippage:
    """符号の向きを間違えると、平均が意味を持たなくなる。"""

    def test_buying_cheaper_than_planned_is_favourable(self) -> None:
        assert execution.slippage_bp("BUY", 1000, 999) == pytest.approx(10.0)

    def test_buying_dearer_than_planned_is_unfavourable(self) -> None:
        assert execution.slippage_bp("BUY", 1000, 1001) == pytest.approx(-10.0)

    def test_selling_higher_than_planned_is_favourable(self) -> None:
        assert execution.slippage_bp("SELL", 1000, 1001) == pytest.approx(10.0)

    def test_selling_lower_than_planned_is_unfavourable(self) -> None:
        assert execution.slippage_bp("SELL", 1000, 999) == pytest.approx(-10.0)

    def test_missing_values_give_none(self) -> None:
        assert execution.slippage_bp("BUY", None, 1000) is None
        assert execution.slippage_bp("BUY", 1000, None) is None
        # 0 円で割らない
        assert execution.slippage_bp("BUY", 0, 1000) is None

    def test_decimal_survives_the_trip(self) -> None:
        assert execution.slippage_bp("BUY", Decimal("2000"), Decimal("1990")) == pytest.approx(50.0)


def test_rows_are_buffered_then_written_once(tmp_path: Path) -> None:
    """発注のたびに Parquet を書かず、実行の終わりに 1 ファイルにまとめる。"""
    store = HistoryStore(tmp_path)
    for symbol in ("1305.T", "1306.T"):
        execution.collect(
            event="intent",
            app="accum",
            symbol=symbol,
            side="BUY",
            reason=ReasonCode.PLACED,
            quantity=10,
            intent_price=Decimal("2000"),
        )
    assert len(execution.pending()) == 2

    execution.flush(store, day=DAY)
    assert execution.pending() == []

    files = store.files(execution.KIND)
    assert len(files) == 1
    frame = store.read(execution.KIND)
    assert frame.height == 2
    assert sorted(frame["symbol"]) == ["1305.T", "1306.T"]
    # 追記専用の鍵が付く
    assert frame["day"][0] == DAY


def test_nothing_written_when_there_was_no_order(tmp_path: Path) -> None:
    """発注の機会が無い実行が大半。空ファイルを毎回作らない。"""
    store = HistoryStore(tmp_path)
    execution.flush(store, day=DAY)
    assert store.files(execution.KIND) == []


def test_fill_row_carries_slippage(tmp_path: Path) -> None:
    store = HistoryStore(tmp_path)
    execution.collect(
        event="fill",
        app="daytrade",
        symbol="7203.T",
        side="BUY",
        reason=ReasonCode.FILLED,
        quantity=100,
        intent_price=Decimal("3000"),
        fill_price=Decimal("3006"),
        fill_quantity=100,
    )
    execution.flush(store, day=DAY)

    row = store.read(execution.KIND).row(0, named=True)
    assert row["event"] == "fill"
    # 想定より 6 円高く買った＝不利
    assert row["slippage_bp"] == pytest.approx(-20.0)
    assert row["fill_quantity"] == 100


def test_enum_values_are_stored_as_plain_strings(tmp_path: Path) -> None:
    """``Side`` / ``TradeType``（StrEnum）が渡ってきても Parquet に収まる。"""
    from wbcore.domain.models import Side, TradeType

    store = HistoryStore(tmp_path)
    execution.collect(
        event="intent",
        app="daytrade",
        symbol="7203.T",
        side=Side.SELL,
        trade=TradeType.MARGIN_OPEN,
        reason=ReasonCode.PLACED,
    )
    execution.flush(store, day=DAY)

    row = store.read(execution.KIND).row(0, named=True)
    assert row["side"] == "SELL"
    assert row["trade"] == TradeType.MARGIN_OPEN.value
    assert isinstance(row["trade"], str)


def test_summary_counts_reasons_and_averages_slippage(tmp_path: Path) -> None:
    """「どの理由でどれだけ見送ったか」が改善の材料になる。"""
    for reason in (ReasonCode.WINDOW_CLOSED, ReasonCode.WINDOW_CLOSED, ReasonCode.LOT_TOO_SMALL):
        execution.collect(
            event="skip", app="accum", symbol="1305.T", side="BUY", reason=reason, intent_amount=100
        )
    execution.collect(
        event="fill",
        app="accum",
        symbol="1305.T",
        side="BUY",
        reason=ReasonCode.FILLED,
        intent_price=1000,
        fill_price=1002,
    )
    store = HistoryStore(tmp_path)
    execution.flush(store, day=DAY)

    summary = execution.summarize(store.read(execution.KIND))
    by_reason = {r["reason"]: r for r in summary.iter_rows(named=True)}
    assert by_reason["window_closed"]["count"] == 2
    assert by_reason["lot_too_small"]["count"] == 1
    assert by_reason["filled"]["avg_slippage_bp"] == pytest.approx(-20.0)


def test_summary_of_nothing_keeps_its_shape() -> None:
    import polars as pl

    empty = pl.DataFrame(schema=execution.EXECUTION_SCHEMA)
    assert execution.summarize(empty).is_empty()


def test_write_failure_does_not_break_the_run(tmp_path: Path) -> None:
    """記録は付帯物。書けなくても発注の実行は落とさない。"""
    blocked = tmp_path / "file"
    blocked.write_text("not a directory", encoding="utf-8")
    execution.collect(
        event="intent", app="accum", symbol="1305.T", side="BUY", reason=ReasonCode.PLACED
    )
    execution.flush(HistoryStore(blocked / "history"), day=DAY)
    assert execution.pending() == []
