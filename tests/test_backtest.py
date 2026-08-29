"""バックテストと永続化の統合テスト。

重点:
    - 先読みバイアスが構造的に起きないこと
    - 約定が「翌営業日の寄付」で起きること
    - journal が判断過程を丸ごと再現できること
"""

from __future__ import annotations

import datetime as dt
import math
from decimal import Decimal
from pathlib import Path

import polars as pl
import pytest

from wbcore.domain.models import (
    CombinedSignal,
    Order,
    OrderRequest,
    OrderStatus,
    OrderType,
    Side,
    Signal,
    TargetPosition,
)
from wbjp.config import (
    ExecutionConfig,
    FileConfig,
    RiskConfig,
    SizingConfig,
    StrategiesConfig,
    StrategyEntry,
    UniverseConfig,
)
from wbjp.db.repo import Journal
from wbjp.engine.backtest import BacktestRunner
from wbjp.risk.stops import Stop
from wbjp.strategy.base import Strategy, StrategyContext
from wbjp.strategy.registry import build_all

D = Decimal


def make_bars(closes: list[float], start: dt.date = dt.date(2024, 1, 1)) -> pl.DataFrame:
    """平日だけ並べた足。open は前日終値と同じにして扱いやすくする。"""
    dates, current = [], start
    while len(dates) < len(closes):
        if current.weekday() < 5:
            dates.append(current)
        current += dt.timedelta(days=1)
    opens = [closes[0], *closes[:-1]]
    return pl.DataFrame(
        {
            "date": dates,
            "open": opens,
            "high": [max(o, c) * 1.005 for o, c in zip(opens, closes, strict=True)],
            "low": [min(o, c) * 0.995 for o, c in zip(opens, closes, strict=True)],
            "close": closes,
            "volume": [1_000_000.0] * len(closes),
        }
    )


def wavy(n: int) -> list[float]:
    out, price = [], 2000.0
    for i in range(n):
        price *= 1 + 0.015 * math.sin(i / 13) + 0.005 * math.cos(i / 2.7)
        out.append(round(price, 1))
    return out


def make_config(**overrides) -> FileConfig:  # type: ignore[no-untyped-def]
    base = {
        "universe": UniverseConfig(symbols=["X"]),
        "risk": RiskConfig(
            max_order_value_jpy=D(50_000_000),
            max_position_weight=D(1),
            max_orders_per_day=1000,
            max_daily_loss_jpy=D(50_000_000),
        ),
        "sizing": SizingConfig(method="equal_weight", max_positions=1),
        "execution": ExecutionConfig(order_type="limit"),
        "strategies": StrategiesConfig(
            combiner="weighted_vote",
            entry_threshold=0.3,
            exit_threshold=0.1,
            strategies=[StrategyEntry(name="sma_cross", fast=5, slow=20)],
        ),
    }
    return FileConfig(**{**base, **overrides})  # type: ignore[arg-type]


# --------------------------------------------------------------------------
# 基本動作
# --------------------------------------------------------------------------


def test_backtest_produces_trades() -> None:
    config = make_config()
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(5_000_000))
    result = runner.run({"X": make_bars(wavy(300))}, start=dt.date(2024, 4, 1))

    assert result.records
    assert result.fills, "1件も約定していない。配管が繋がっていない可能性がある"


def test_backtest_rejects_empty_bars() -> None:
    config = make_config()
    runner = BacktestRunner(build_all(config.strategies.enabled), config)
    with pytest.raises(ValueError, match="空"):
        runner.run({})


def test_backtest_rejects_period_without_trading_days() -> None:
    config = make_config()
    runner = BacktestRunner(build_all(config.strategies.enabled), config)
    with pytest.raises(ValueError, match="取引日"):
        runner.run({"X": make_bars(wavy(50))}, start=dt.date(2030, 1, 1))


def test_equity_curve_matches_record_count() -> None:
    config = make_config()
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(5_000_000))
    result = runner.run({"X": make_bars(wavy(200))}, start=dt.date(2024, 4, 1))

    assert result.equity_curve.height == len(result.records)


def test_max_drawdown_is_non_negative() -> None:
    config = make_config()
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(5_000_000))
    result = runner.run({"X": make_bars(wavy(300))}, start=dt.date(2024, 4, 1))

    assert result.max_drawdown >= 0


# --------------------------------------------------------------------------
# 先読みバイアス
# --------------------------------------------------------------------------


class SpyStrategy(Strategy):
    """戦略が実際に見た足の最終日を記録する。"""

    name = "spy"
    warmup_bars = 5

    def __init__(self) -> None:
        self.seen: list[tuple[dt.date, dt.date]] = []

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        for symbol in ctx.symbols:
            last = ctx.bars(symbol)["date"].max()
            self.seen.append((ctx.as_of, last))  # type: ignore[arg-type]
        return []


def test_strategy_never_sees_future_bars() -> None:
    """戦略に渡る足の最終日が、判断日を超えないこと。

    ここが破れるとバックテストの成績は無意味になる。
    """
    spy = SpyStrategy()
    config = make_config()
    runner = BacktestRunner([spy], config, initial_cash=D(1_000_000))
    runner.run({"X": make_bars(wavy(100))}, start=dt.date(2024, 2, 1))

    assert spy.seen
    for as_of, last_bar in spy.seen:
        assert last_bar <= as_of, f"判断日 {as_of} に {last_bar} の足が見えている"


def test_strategy_sees_the_current_bar() -> None:
    """当日の終値までは見える（そうでないと1日遅れの判断になる）。"""
    spy = SpyStrategy()
    config = make_config()
    runner = BacktestRunner([spy], config, initial_cash=D(1_000_000))
    runner.run({"X": make_bars(wavy(100))}, start=dt.date(2024, 2, 1))

    assert any(as_of == last for as_of, last in spy.seen)


# --------------------------------------------------------------------------
# 約定タイミング
# --------------------------------------------------------------------------


class BuyOnceStrategy(Strategy):
    """最初の判断日に1回だけ買いシグナルを出す。"""

    name = "buy_once"
    warmup_bars = 2

    def __init__(self) -> None:
        self.fired = False

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        if self.fired:
            return []
        self.fired = True
        return [
            Signal(self.name, s, direction=1.0, confidence=1.0, reason="テスト")
            for s in ctx.symbols
        ]


def test_fill_happens_on_the_next_session_open() -> None:
    """判断した当日の終値では約定しない。翌営業日の寄付で約定する。

    同日終値で約定させると、実際には取れない価格で売買できることになり、
    バックテストの成績が実態より良く出る。
    """
    closes = [1000.0] * 30
    config = make_config(
        sizing=SizingConfig(method="fixed_notional", fixed_notional_jpy=D(200_000))
    )
    runner = BacktestRunner([BuyOnceStrategy()], config, initial_cash=D(1_000_000))

    bars = make_bars(closes)
    result = runner.run({"X": bars}, start=bars["date"][5])

    assert result.fills
    fill_date = next(r.date for r in result.records if any(f.client_order_id for f in r.fills))
    decision_date = next(r.date for r in result.records if r.orders)
    assert fill_date > decision_date


def test_orders_are_recorded_with_their_reason() -> None:
    config = make_config()
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(5_000_000))
    result = runner.run({"X": make_bars(wavy(200))}, start=dt.date(2024, 4, 1))

    with_targets = [r for r in result.records if r.targets]
    assert with_targets


# --------------------------------------------------------------------------
# 合成方式の違いが結果に出るか
# --------------------------------------------------------------------------


@pytest.mark.parametrize("combiner", ["weighted_vote", "majority", "veto", "priority"])
def test_every_combiner_runs_end_to_end(combiner: str) -> None:
    config = make_config(
        strategies=StrategiesConfig(
            combiner=combiner,
            entry_threshold=0.3,
            exit_threshold=0.1,
            strategies=[
                StrategyEntry(name="sma_cross", fast=5, slow=20),
                StrategyEntry(name="rsi_reversion", period=14, adx_period=5),
                StrategyEntry(name="atr_breakout", channel=10, atr_period=10),
            ],
        )
    )
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(5_000_000))
    result = runner.run({"X": make_bars(wavy(250))}, start=dt.date(2024, 4, 1))

    assert result.records
    assert result.final_equity > 0


def test_kill_switch_prevents_all_trading() -> None:
    config = make_config(risk=RiskConfig(kill_switch=True, max_order_value_jpy=D(50_000_000)))
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(5_000_000))
    result = runner.run({"X": make_bars(wavy(200))}, start=dt.date(2024, 4, 1))

    assert result.fills == []
    assert result.final_equity == result.initial_equity


def test_allowlist_blocks_unlisted_symbols() -> None:
    """universe に無い銘柄には、足があっても発注しない。"""
    config = make_config(universe=UniverseConfig(symbols=["OTHER"]))
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(5_000_000))
    result = runner.run({"X": make_bars(wavy(200))}, start=dt.date(2024, 4, 1))

    assert result.fills == []


# ==========================================================================
# Journal
# ==========================================================================


@pytest.fixture
def journal(tmp_path: Path) -> Journal:
    return Journal(tmp_path / "test.db")


def test_journal_records_and_reads_a_run(journal: Journal) -> None:
    journal.start_run("r1", dt.date(2026, 8, 25), "uat", "dry_run", D(1_000_000))
    journal.finish_run("r1")

    row = journal.get_run("r1")
    assert row is not None
    assert row["status"] == "ok"
    assert row["equity"] == "1000000"


def test_journal_preserves_decimal_precision(journal: Journal) -> None:
    """SQLite の REAL に通すと丸め誤差が入る。TEXT で保持する。"""
    journal.start_run("r1", dt.date(2026, 8, 25), "uat", "live", D("1234567.89"))
    assert Decimal(journal.get_run("r1")["equity"]) == D("1234567.89")  # type: ignore[index]


def test_journal_captures_the_whole_decision(journal: Journal) -> None:
    """シグナルから拒否理由まで、判断過程を丸ごと追える。"""
    journal.start_run("r1", dt.date(2026, 8, 25), "uat", "dry_run")
    journal.record_signals(
        "r1", [Signal("sma_cross", "7203", 1.0, 0.8, "ゴールデンクロス", {"fast": 100})]
    )
    journal.record_combined(
        "r1", {"7203": CombinedSignal("7203", 0.8, {"sma_cross": 0.8}, "加重平均")}
    )
    journal.record_targets("r1", [TargetPosition("7203", D(100), "atr_risk")])
    journal.record_risk_events("r1", {"6758": "allowlist 外"})

    explained = journal.explain("r1")

    assert explained["signals"][0]["reason"] == "ゴールデンクロス"
    assert explained["combined_signals"][0]["direction"] == 0.8
    assert explained["targets"][0]["quantity"] == "100"
    assert explained["risk_events"][0]["reason"] == "allowlist 外"


def test_journal_detects_already_placed_orders(journal: Journal) -> None:
    """冪等性の最後の砦。決定論的な注文IDが既にあれば発注しない。"""
    journal.start_run("r1", dt.date(2026, 8, 25), "uat", "live")
    request = OrderRequest("coid1", "7203", Side.BUY, OrderType.LIMIT, D(100), limit_price=D(2500))

    assert journal.was_placed("coid1") is False
    journal.record_order("r1", request, "SUBMITTED")
    assert journal.was_placed("coid1") is True


def test_dry_run_does_not_count_as_placed(journal: Journal) -> None:
    """dry-run はブローカーに何も送っていない。発注済みに数えてはいけない。

    注文IDは取引日から作られるため、dry-run と実発注は同じIDになる。
    dry-run を「発注済み」と数えると、README が勧める「まず dry-run で
    確認する」手順を踏んだ日は、実発注が丸ごと抑止される。
    """
    journal.start_run("r1", dt.date(2026, 8, 25), "prod", "dry_run")
    request = OrderRequest("coid1", "7203", Side.BUY, OrderType.LIMIT, D(100), limit_price=D(2500))
    journal.record_order("r1", request, Journal.DRY_RUN_STATUS)

    assert journal.was_placed("coid1") is False

    # 同じ日に実発注すると、dry-run の記録を上書きして発注済みになる
    journal.start_run("r2", dt.date(2026, 8, 25), "prod", "live")
    journal.record_order("r2", request, "SUBMITTED")
    assert journal.was_placed("coid1") is True


def test_dry_run_does_not_consume_the_daily_order_budget(journal: Journal) -> None:
    """dry-run は1件もブローカーに送っていない。発注枠を減らしてはいけない。

    減らすと、設定確認のために dry-run を数回回しただけで
    max_orders_per_day が尽き、その日の実発注が止まる。
    """
    # placed_at は挿入時の実時刻。as_of ではなく実日付で数える。
    day = Journal.today()
    journal.start_run("r1", day, "prod", "dry_run")
    for i in range(5):
        request = OrderRequest(
            f"c{i}", "3664", Side.BUY, OrderType.LIMIT, D(100), limit_price=D(28)
        )
        journal.record_order("r1", request, Journal.DRY_RUN_STATUS)

    assert journal.orders_today(day) == 0

    journal.record_order(
        "r1",
        OrderRequest("live1", "3664", Side.BUY, OrderType.LIMIT, D(100), limit_price=D(28)),
        "SUBMITTED",
    )
    assert journal.orders_today(day) == 1


def test_cancelled_orders_still_consume_the_daily_budget(journal: Journal) -> None:
    """取り消した注文も枠を消費する。

    max_orders_per_day は「暴走した戦略が API を叩き続ける」ことへの
    ブレーキであって、約定回数の上限ではない。出して取り消す往復も
    ブローカーへの発注として数える。
    """
    day = Journal.today()
    journal.start_run("r1", day, "prod", "live")
    request = OrderRequest("c1", "3664", Side.BUY, OrderType.LIMIT, D(100), limit_price=D(28))
    journal.record_order("r1", request, "CANCELLED")

    assert journal.orders_today(day) == 1


def test_journal_updates_order_status(journal: Journal) -> None:
    journal.start_run("r1", dt.date(2026, 8, 25), "uat", "live")
    request = OrderRequest("coid1", "7203", Side.BUY, OrderType.LIMIT, D(100), limit_price=D(2500))
    journal.record_order("r1", request, "SUBMITTED")

    journal.update_order(
        Order(
            client_order_id="coid1",
            broker_order_id="b1",
            symbol="7203",
            side=Side.BUY,
            order_type=OrderType.LIMIT,
            quantity=D(100),
            filled_quantity=D(100),
            status=OrderStatus.FILLED,
            avg_fill_price=D(2495),
        )
    )

    row = journal.explain("r1")["orders"][0]
    assert row["status"] == "FILLED"
    assert row["avg_fill_price"] == "2495"


def test_journal_counts_orders_placed_today(journal: Journal) -> None:
    journal.start_run("r1", dt.date(2026, 8, 25), "uat", "live")
    for i in range(3):
        journal.record_order(
            "r1",
            OrderRequest(f"c{i}", "7203", Side.BUY, OrderType.MARKET, D(100)),
            "SUBMITTED",
        )

    assert journal.orders_today() == 3


def test_journal_round_trips_stops(journal: Journal) -> None:
    """プロセスが落ちてもストップが失われないこと。

    ここが消えると建玉が無防備になる。
    """
    stops = {
        "7203": Stop(
            symbol="7203",
            stop_price=D("2400.5"),
            entry_price=D(2500),
            created_on=dt.date(2026, 8, 25),
            trailing=True,
            atr_multiple=D("2.5"),
            highest_close=D(2600),
        )
    }
    journal.save_stops(stops)

    loaded = journal.load_stops()

    assert loaded["7203"].stop_price == D("2400.5")
    assert loaded["7203"].trailing is True
    assert loaded["7203"].highest_close == D(2600)


def test_saving_stops_replaces_the_previous_set(journal: Journal) -> None:
    journal.save_stops({"7203": Stop("7203", D(2400), D(2500), dt.date(2026, 8, 25))})
    journal.save_stops({"6758": Stop("6758", D(3400), D(3500), dt.date(2026, 8, 25))})

    assert set(journal.load_stops()) == {"6758"}


def test_journal_transaction_rolls_back_on_error(journal: Journal) -> None:
    journal.start_run("r1", dt.date(2026, 8, 25), "uat", "live")

    with pytest.raises(RuntimeError), journal.transaction():
        journal.record_targets("r1", [TargetPosition("7203", D(100))])
        raise RuntimeError("失敗")

    assert journal.explain("r1")["targets"] == []


def test_recent_runs_are_newest_first(journal: Journal) -> None:
    for i in range(3):
        journal.start_run(f"r{i}", dt.date(2026, 8, 25), "uat", "dry_run")

    runs = journal.recent_runs(10)
    assert len(runs) == 3


# ==========================================================================
# 指標の先回り計算が結果を変えないこと
# ==========================================================================


def test_precomputed_indicators_match_per_slice_computation() -> None:
    """全期間で計算してから切っても、切ってから計算しても同じ値になること。

    バックテストは高速化のため指標を先に一度だけ計算する。これが
    成り立つのは本システムの指標がすべて因果的（過去だけを見る）だから。
    将来この前提を壊す指標を足したら、ここで気付ける。
    """
    from wbcore.indicators.ohlcv import adx, atr, bollinger_bands, macd, rsi, sma

    frame = make_bars(wavy(200))
    expressions = [sma(25), rsi(14), atr(14), *macd(), *bollinger_bands(), *adx(14)]

    precomputed = frame.with_columns(expressions)

    for cutoff in (60, 120, 199):
        sliced_then_computed = frame.head(cutoff).with_columns(expressions)
        computed_then_sliced = precomputed.head(cutoff)

        for column in sliced_then_computed.columns:
            assert sliced_then_computed[column].to_list() == pytest.approx(
                computed_then_sliced[column].to_list(), nan_ok=True
            ), f"{column} が {cutoff} 行目で食い違う"


def test_indicator_strategy_skips_recomputation_when_columns_exist() -> None:
    """先回り計算済みの列があれば、戦略は再計算しない。"""
    from wbjp.strategy.samples.sma_cross import SmaCrossStrategy

    strategy = SmaCrossStrategy(fast=5, slow=20)
    frame = make_bars(wavy(60)).with_columns(strategy.indicators())

    computed: list[int] = []
    original = pl.DataFrame.with_columns

    def counting(self, *args, **kwargs):  # type: ignore[no-untyped-def]
        computed.append(1)
        return original(self, *args, **kwargs)

    pl.DataFrame.with_columns = counting  # type: ignore[method-assign]
    try:
        strategy.on_bars(StrategyContext(as_of=frame["date"][-1], _bars={"X": frame}))
    finally:
        pl.DataFrame.with_columns = original  # type: ignore[method-assign]

    assert computed == [], "指標が再計算されている"


def test_indicator_strategy_still_computes_when_columns_are_missing() -> None:
    """先回り計算されていない場合は、これまで通り自分で計算する。"""
    from wbjp.strategy.samples.sma_cross import SmaCrossStrategy

    strategy = SmaCrossStrategy(fast=5, slow=20)
    frame = make_bars(wavy(60))  # 指標なし

    signals = strategy.on_bars(StrategyContext(as_of=frame["date"][-1], _bars={"X": frame}))

    assert signals, "指標を自前で計算できていない"
