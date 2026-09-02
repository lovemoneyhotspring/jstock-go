"""Backtrader エンジンのテスト。

重点:
    - 判断は自前エンジンと同一（同じ DecisionPipeline を使う）こと
    - 約定モデルが自前エンジンと一致すること（成行なら約定日・数量・
      価格がすべて揃い、資産曲線が丸め誤差の範囲で一致する）
    - 先読みバイアスが構造的に起きないこと
    - 米国株ではブローカー逆指値がバー内で執行されること
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import pytest

from tests.test_backtest import SpyStrategy, make_bars, make_config, wavy
from wbjp.config import ExecutionConfig, SizingConfig, StopsConfig, UniverseConfig
from wbjp.engine.backtest import BacktestRunner
from wbjp.engine.bt_engine import BacktraderRunner
from wbjp.strategy.registry import build_all

D = Decimal
START = dt.date(2024, 4, 1)
CASH = D(5_000_000)


def market_config(**overrides):  # type: ignore[no-untyped-def]
    """成行で発注する設定。指値は両エンジンで約定判定が違うので比較に使わない。"""
    return make_config(execution=ExecutionConfig(order_type="market"), **overrides)


# --------------------------------------------------------------------------
# 基本動作
# --------------------------------------------------------------------------


def test_backtrader_produces_trades() -> None:
    config = market_config()
    runner = BacktraderRunner(
        build_all(config.strategies.enabled), config, initial_cash=D(5_000_000)
    )
    result = runner.run({"X": make_bars(wavy(300))}, start=START)

    assert result.records
    assert result.fills, "1件も約定していない。橋渡しが繋がっていない可能性がある"
    assert result.initial_equity == D(5_000_000)
    assert result.analysis, "アナライザの出力が無い"


def test_backtrader_rejects_empty_bars() -> None:
    config = market_config()
    with pytest.raises(ValueError, match="空"):
        BacktraderRunner(build_all(config.strategies.enabled), config).run({})


def test_backtrader_rejects_period_without_trading_days() -> None:
    config = market_config()
    runner = BacktraderRunner(build_all(config.strategies.enabled), config)
    with pytest.raises(ValueError, match="取引日"):
        runner.run({"X": make_bars(wavy(50))}, start=dt.date(2030, 1, 1))


def test_records_only_within_period() -> None:
    config = market_config()
    runner = BacktraderRunner(build_all(config.strategies.enabled), config)
    end = dt.date(2024, 6, 28)
    result = runner.run({"X": make_bars(wavy(300))}, start=START, end=end)

    assert result.records[0].date >= START
    assert result.records[-1].date <= end


# --------------------------------------------------------------------------
# 自前エンジンとの突き合わせ
# --------------------------------------------------------------------------


def _run_both(config, bars, cash=CASH):  # type: ignore[no-untyped-def]
    native = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=cash)
    bt_ = BacktraderRunner(build_all(config.strategies.enabled), config, initial_cash=cash)
    return native.run(bars, start=START), bt_.run(bars, start=START)


def _fill_keys(result):  # type: ignore[no-untyped-def]
    return [(r.date, f.symbol, f.side, f.quantity) for r in result.records for f in r.fills]


def test_market_orders_match_native_engine_single_symbol() -> None:
    """成行・JP・エンジン合成ストップ: 約定の日付・銘柄・方向・数量が完全一致する。"""
    config = market_config()
    native, bt_ = _run_both(config, {"X": make_bars(wavy(300))})

    assert _fill_keys(bt_) == _fill_keys(native)
    for a, b in zip(native.fills, bt_.fills, strict=True):
        # 価格は寄付 × (1 ± 滑り)。float と Decimal の差だけ
        assert abs(a.price - b.price) / a.price < D("1e-9")
        assert abs(a.fee - b.fee) <= D(1)  # 手数料は円に丸めるので ±1円

    # 資産曲線は手数料の丸め差が積もる程度
    assert abs(native.final_equity - bt_.final_equity) / native.final_equity < D("1e-5")
    for a, b in zip(native.records, bt_.records, strict=True):
        assert a.date == b.date
        assert abs(a.equity - b.equity) / a.equity < D("1e-5")


def test_market_orders_match_native_engine_multi_symbol() -> None:
    """複数銘柄・トレーリングストップ・時間切れも含めて一致する。"""
    config = market_config(
        universe=UniverseConfig(symbols=["A", "B", "C"]),
        sizing=SizingConfig(method="equal_weight", max_positions=2),
        stops=StopsConfig(trailing=True, max_hold_days=40),
    )
    bars = {
        "A": make_bars(wavy(300)),
        "B": make_bars([p * 0.7 + 30 for p in wavy(300)]),
        "C": make_bars(list(reversed(wavy(300)))),
    }
    native, bt_ = _run_both(config, bars)

    assert native.fills, "比較対象の約定が無い"
    assert _fill_keys(bt_) == _fill_keys(native)
    assert abs(native.final_equity - bt_.final_equity) / native.final_equity < D("1e-5")


# --------------------------------------------------------------------------
# 先読みバイアス
# --------------------------------------------------------------------------


def test_strategy_never_sees_future_bars() -> None:
    spy = SpyStrategy()
    config = market_config()
    runner = BacktraderRunner([spy], config)
    runner.run({"X": make_bars(wavy(120))}, start=START)

    assert spy.seen
    for as_of, last in spy.seen:
        assert last == as_of, f"{as_of} の判断で {last} の足が見えている"


# --------------------------------------------------------------------------
# 米国株: ブローカー逆指値
# --------------------------------------------------------------------------
