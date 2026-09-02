"""約定モデル（open / intrabar）と、バックテスト結果の分析値。

重点:
    - intrabar は指値をその足の高安で約定させ、open は寄付だけで判定する
    - 成行だけの設定なら、2 つの約定モデルで約定・資産曲線が完全に一致する
    - 分析値（ドローダウン期間・トレード統計・シャープ）が資産曲線と約定列だけから出る
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import pytest

from tests.test_backtest import make_bars, make_config, wavy
from wbcore.broker.paper import PaperBroker, limit_fill_price
from wbcore.domain.models import Fill, OrderRequest, OrderType, Side
from wbjp.config import ExecutionConfig, SizingConfig, StopsConfig, UniverseConfig
from wbjp.engine.analysis import analyze, closed_trades, longest_drawdown, sharpe_ratio
from wbjp.engine.backtest import BacktestRunner
from wbjp.strategy.registry import build_all

D = Decimal
START = dt.date(2024, 4, 1)
CASH = D(5_000_000)


# --------------------------------------------------------------------------
# 指値の約定判定
# --------------------------------------------------------------------------


def test_open_model_only_looks_at_the_open() -> None:
    assert limit_fill_price(Side.BUY, D(100), D(99), D(105), D(95), "open") == D(99)
    assert limit_fill_price(Side.BUY, D(100), D(101), D(105), D(95), "open") is None
    assert limit_fill_price(Side.SELL, D(100), D(101), D(105), D(95), "open") == D(101)
    assert limit_fill_price(Side.SELL, D(100), D(99), D(105), D(95), "open") is None


def test_intrabar_model_fills_at_the_limit_when_the_range_reaches_it() -> None:
    # 寄付が有利ならより有利な寄付で
    assert limit_fill_price(Side.BUY, D(100), D(99), D(105), D(95), "intrabar") == D(99)
    # 寄付では届かないが安値が届けば指値で
    assert limit_fill_price(Side.BUY, D(100), D(101), D(105), D(95), "intrabar") == D(100)
    assert limit_fill_price(Side.BUY, D(100), D(101), D(105), D(101), "intrabar") is None
    assert limit_fill_price(Side.SELL, D(100), D(99), D(105), D(95), "intrabar") == D(100)
    assert limit_fill_price(Side.SELL, D(100), D(99), D(99), D(95), "intrabar") is None
    # 高安が無ければ open と同じ
    assert limit_fill_price(Side.BUY, D(100), D(101), None, None, "intrabar") is None


def test_paper_broker_intrabar_settles_with_highs_and_lows() -> None:
    broker = PaperBroker(initial_cash=D(1_000_000), commission_rate=D(0), fill_model="intrabar")
    broker.place(
        OrderRequest("o1", "7203", Side.BUY, OrderType.LIMIT, D(100), limit_price=D(2400))
    )
    # 寄付 2450 では届かないが安値 2380 で届く → 2400 で約定
    fills = broker.settle({"7203": D(2450)}, highs={"7203": D(2500)}, lows={"7203": D(2380)})
    assert [f.price for f in fills] == [D(2400)]
    assert broker.positions_by_symbol()["7203"].quantity == D(100)


def test_unknown_fill_model_is_rejected() -> None:
    with pytest.raises(ValueError, match="fill_model"):
        PaperBroker(fill_model="magic")


# --------------------------------------------------------------------------
# 2 つの約定モデルの突き合わせ
# --------------------------------------------------------------------------


def _run_both(config, bars):  # type: ignore[no-untyped-def]
    open_ = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=CASH)
    intrabar = BacktestRunner(
        build_all(config.strategies.enabled), config, initial_cash=CASH, fill_model="intrabar"
    )
    return open_.run(bars, start=START), intrabar.run(bars, start=START)


def _fill_keys(result):  # type: ignore[no-untyped-def]
    return [(r.date, f.symbol, f.side, f.quantity, f.price) for r in result.records for f in r.fills]


def test_market_orders_match_across_fill_models() -> None:
    """成行・複数銘柄・トレーリングストップ: 約定も資産曲線も完全に一致する。"""
    config = make_config(
        execution=ExecutionConfig(order_type="market"),
        universe=UniverseConfig(symbols=["A", "B", "C"]),
        sizing=SizingConfig(method="equal_weight", max_positions=2),
        stops=StopsConfig(trailing=True, max_hold_days=40),
    )
    bars = {
        "A": make_bars(wavy(300)),
        "B": make_bars([p * 0.7 + 30 for p in wavy(300)]),
        "C": make_bars(list(reversed(wavy(300)))),
    }
    open_, intrabar = _run_both(config, bars)

    assert open_.fills, "比較対象の約定が無い"
    assert _fill_keys(intrabar) == _fill_keys(open_)
    assert intrabar.final_equity == open_.final_equity
    assert [r.equity for r in intrabar.records] == [r.equity for r in open_.records]
    assert open_.analysis and intrabar.analysis


def test_limit_orders_fill_at_least_as_often_with_intrabar() -> None:
    """指値では intrabar の方が約定しやすい（open の約定は intrabar でも必ず約定する）。"""
    config = make_config(execution=ExecutionConfig(order_type="limit", limit_offset=D("0.001")))
    open_, intrabar = _run_both(config, {"X": make_bars(wavy(300))})

    open_keys = {(r.date, f.symbol, f.side) for r in open_.records for f in r.fills}
    intrabar_keys = {(r.date, f.symbol, f.side) for r in intrabar.records for f in r.fills}
    assert len(intrabar.fills) >= len(open_.fills)
    # 建玉の食い違いで手仕舞いの日付がずれることはあるが、最初の約定日は一致する
    assert min(open_keys)[0] == min(intrabar_keys)[0]


# --------------------------------------------------------------------------
# 分析値
# --------------------------------------------------------------------------


def _fill(symbol: str, side: Side, qty: int, price: str, fee: str = "0") -> Fill:
    return Fill(client_order_id="c", symbol=symbol, side=side, quantity=D(qty), price=D(price), fee=D(fee))


def test_closed_trades_pair_fills_fifo_with_fees() -> None:
    fills = [
        _fill("A", Side.BUY, 100, "1000", "100"),
        _fill("A", Side.BUY, 100, "1100", "100"),
        _fill("A", Side.SELL, 150, "1200", "150"),
        _fill("B", Side.SELL, 100, "500"),  # 保有が無い売り（空売り）は対象外
    ]
    trades = closed_trades(fills)
    assert [(t.symbol, t.quantity) for t in trades] == [("A", D(100)), ("A", D(50))]
    # 1 本目: 100 株 × (1200 − 1) − 100 株 × (1000 + 1) = 119,900 − 100,100
    assert trades[0].pnl == D(119_900) - D(100_100)
    # 2 本目: 50 株 × (1200 − 1) − 50 株 × (1100 + 1)
    assert trades[1].pnl == D(59_950) - D(55_050)


def test_longest_drawdown_counts_bars_below_the_running_peak() -> None:
    assert longest_drawdown([D(100), D(101), D(102)]) == 0
    assert longest_drawdown([D(100), D(90), D(95), D(101), D(99)]) == 2
    assert longest_drawdown([D(100), D(90), D(80), D(85), D(100)]) == 3  # 100 に戻った本で解消


def test_sharpe_ratio_needs_variance_and_annualizes() -> None:
    assert sharpe_ratio([D(100)]) is None
    assert sharpe_ratio([D(100), D(100), D(100)]) is None
    flat_then_up = [D(100), D(101), D(102), D(104)]
    value = sharpe_ratio(flat_then_up, periods_per_year=245)
    assert value is not None and value > 0


def test_analyze_reports_dashes_when_there_is_nothing_to_measure() -> None:
    out = analyze([D(100), D(100)], [])
    assert out["決済トレード数"] == 0
    assert out["勝率"] == "-"
    assert out["シャープレシオ (年率)"] == "-"
    assert out["最長ドローダウン期間 (本)"] == 0
