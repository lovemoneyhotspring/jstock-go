"""日中足でのバックテストと戦略。

確かめること:
- 日足と同じ経路で 5 分足が回り、約定は「次の足の寄付」で起きる
- 暦日の変わり目でだけ日付の意味を持つ処理（当日買付の記録）が動く
- 戦略は足の間隔を宣言し、対応しない間隔は起動時に弾かれる
- 窓を時間で持つ戦略が、間隔に応じて本数に直す
- ライブ運用は日中足を明示的に拒む（黙って日足で動かない）
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path
from typing import ClassVar

import polars as pl
import pytest

from wbcore.broker.paper import PaperBroker
from wbcore.data.provider import Interval, parse_duration
from wbcore.data.store import BarStore
from wbcore.domain.models import Market, Position, Signal
from wbcore.domain.session import SessionWindow
from wbjp.config import (
    AppSettings,
    Config,
    ExecutionConfig,
    FileConfig,
    RiskConfig,
    SizingConfig,
    StrategiesConfig,
    StrategyEntry,
    UniverseConfig,
)
from wbjp.db.repo import Journal
from wbjp.engine.backtest import BacktestRunner, DecisionPipeline
from wbjp.engine.live import LiveRunner
from wbjp.strategy.base import Strategy, StrategyContext
from wbjp.strategy.registry import build_all
from wbjp.strategy.samples.intraday_sma_cross import IntradaySmaCrossStrategy

D = Decimal

#: 東証 09:00 JST = 00:00 UTC。
SESSION_START = dt.datetime(2026, 8, 3, 0, 0, tzinfo=dt.UTC)


def five_minute_bars(closes: list[float], *, per_day: int = 12) -> pl.DataFrame:
    """5分足を営業日ごとに ``per_day`` 本ずつ並べる。open は前の足の終値。"""
    stamps: list[dt.datetime] = []
    day = SESSION_START
    while len(stamps) < len(closes):
        if day.weekday() < 5:
            for i in range(per_day):
                if len(stamps) == len(closes):
                    break
                stamps.append(day + dt.timedelta(minutes=5 * i))
        day += dt.timedelta(days=1)
    opens = [closes[0], *closes[:-1]]
    return pl.DataFrame(
        {
            "ts": stamps,
            "open": opens,
            "high": [max(o, c) * 1.002 for o, c in zip(opens, closes, strict=True)],
            "low": [min(o, c) * 0.998 for o, c in zip(opens, closes, strict=True)],
            "close": closes,
            "volume": [10_000.0] * len(closes),
        }
    ).with_columns(pl.col("ts").dt.date().alias("date"))


class AlwaysBuy(Strategy):
    """常に買い。約定のタイミングを見るための最小の戦略。"""

    name: ClassVar[str] = "always_buy_intraday"
    warmup_bars = 1

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        assert ctx.at is not None and ctx.interval is Interval.M5
        return [Signal(self.name, s, direction=1.0, reason="常に買い") for s in ctx.symbols]


class DailyOnly(Strategy):
    name: ClassVar[str] = "daily_only"
    intervals: ClassVar[frozenset[Interval]] = frozenset({Interval.D1})

    def on_bars(self, ctx: StrategyContext) -> list[Signal]:
        return []


def make_config(interval: str = "5m", **overrides) -> FileConfig:  # type: ignore[no-untyped-def]
    base = {
        "universe": UniverseConfig(symbols=["X"], interval=interval),
        "risk": RiskConfig(
            max_order_value=D(50_000_000),
            max_position_weight=D(1),
            max_orders_per_day=1000,
            max_daily_loss=D(50_000_000),
        ),
        "sizing": SizingConfig(method="equal_weight", max_positions=1),
        "execution": ExecutionConfig(order_type="market"),
        "strategies": StrategiesConfig(strategies=[]),
    }
    return FileConfig(**{**base, **overrides})  # type: ignore[arg-type]


# --- エンジン ------------------------------------------------------------


def test_intraday_backtest_fills_at_the_next_bars_open() -> None:
    bars = {"X": five_minute_bars([100.0 + i for i in range(30)])}
    runner = BacktestRunner(
        [AlwaysBuy()], make_config(), initial_cash=D(1_000_000), commission_rate=D(0)
    )
    result = runner.run(bars)

    assert len(result.records) == 30
    assert all(r.at is not None for r in result.records)
    # 1本目の終値で判断 → 2本目の寄付で約定
    assert result.records[0].fills == []
    first = result.records[1].fills
    assert first and first[0].side.value == "BUY"
    assert result.records[1].at == SESSION_START + dt.timedelta(minutes=5)


def test_summary_counts_days_and_decisions_separately() -> None:
    bars = {"X": five_minute_bars([100.0] * 24, per_day=12)}  # 2 営業日 × 12 本
    result = BacktestRunner([AlwaysBuy()], make_config()).run(bars)
    summary = result.summary()
    assert summary["日数"] == 2
    assert summary["判断回数"] == 24


def test_bought_today_resets_only_when_the_calendar_day_changes() -> None:
    """差金決済の判定は暦日単位。足ごとに空にしてはいけない。"""
    bars = {"X": five_minute_bars([100.0] * 24, per_day=12)}
    runner = BacktestRunner([AlwaysBuy()], make_config(), initial_cash=D(1_000_000))
    seen: list[tuple[dt.date, bool]] = []
    original = runner.broker.begin_day

    def spy() -> None:
        seen.append((dt.date.today(), True))
        original()

    runner.broker.begin_day = spy  # type: ignore[method-assign]
    runner.run(bars)
    assert len(seen) == 2  # 2 営業日 → 2 回だけ


def test_daily_backtest_is_unchanged_by_the_generalisation() -> None:
    from tests.test_backtest import make_bars, wavy

    config = make_config(
        interval="1d",
        strategies=StrategiesConfig(strategies=[StrategyEntry(name="sma_cross", fast=5, slow=20)]),
    )
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(5_000_000))
    result = runner.run({"X": make_bars(wavy(120))})
    assert result.records and all(r.at is None for r in result.records)
    assert "判断回数" not in result.summary()


def test_bars_with_the_wrong_interval_are_rejected() -> None:
    from tests.test_backtest import make_bars

    runner = BacktestRunner([AlwaysBuy()], make_config("5m"))
    with pytest.raises(ValueError, match=r"universe\.interval"):
        runner.run({"X": make_bars([100.0] * 10)})


# --- 戦略の間隔宣言 --------------------------------------------------------


def test_strategy_that_only_supports_daily_is_rejected_for_intraday() -> None:
    with pytest.raises(ValueError, match="daily_only は 5m 足に対応していません"):
        DecisionPipeline([DailyOnly()], make_config("5m"))


def test_pipeline_binds_the_interval_to_strategies() -> None:
    strategy = IntradaySmaCrossStrategy(fast="15m", slow="1h")
    DecisionPipeline([strategy], make_config("5m"))
    assert (strategy.fast, strategy.slow) == (3, 12)
    assert strategy.warmup_bars == 13

    one_minute = IntradaySmaCrossStrategy(fast="15m", slow="1h")
    DecisionPipeline([one_minute], make_config("1m"))
    assert (one_minute.fast, one_minute.slow) == (15, 60)


def test_time_window_that_collapses_to_the_same_bar_count_is_rejected() -> None:
    strategy = IntradaySmaCrossStrategy(fast="15m", slow="20m")
    with pytest.raises(ValueError, match="逆転または同じ"):
        strategy.bind(Interval.M15)  # 15m → 1 本、20m → 1 本


def test_interval_bars_in_and_parse_duration() -> None:
    assert Interval.M5.bars_in("1h") == 12
    assert Interval.M1.bars_in(dt.timedelta(minutes=90)) == 90
    assert Interval.D1.bars_in("25d") == 25
    assert parse_duration("2h") == dt.timedelta(hours=2)
    with pytest.raises(ValueError, match="1 本より短い"):
        Interval.H1.bars_in("15m")
    with pytest.raises(ValueError, match="時間の表記が不正"):
        parse_duration("soon")


# --- 取引時間帯と引け前 ---------------------------------------------------


def _ctx(at: dt.datetime, *, holding: bool) -> StrategyContext:
    positions = {"X": Position("X", D(100), D(100), D(100), D(101), "JPY")} if holding else {}
    return StrategyContext(
        as_of=at.date(), _bars={}, _positions=positions, interval=Interval.M5, at=at
    )


def _crossed_up() -> pl.DataFrame:
    return pl.DataFrame({"sma_3": [99.0, 101.0], "sma_12": [100.0, 100.0]})


def test_flat_before_the_close_sells_holdings_and_never_buys() -> None:
    strategy = IntradaySmaCrossStrategy(fast="15m", slow="1h", flat_before="15:00")
    strategy.bind(Interval.M5)
    at_1505_jst = dt.datetime(2026, 8, 3, 6, 5, tzinfo=dt.UTC)
    sell = strategy.evaluate("X", _crossed_up(), _ctx(at_1505_jst, holding=True))
    assert sell is not None and sell.direction == -1.0
    assert strategy.evaluate("X", _crossed_up(), _ctx(at_1505_jst, holding=False)) is None


def test_outside_the_session_no_new_entries_but_holdings_keep_an_opinion() -> None:
    strategy = IntradaySmaCrossStrategy(
        fast="15m", slow="1h", session={"start": "09:30", "end": "14:30"}, flat_before=""
    )
    strategy.bind(Interval.M5)
    at_0905_jst = dt.datetime(2026, 8, 3, 0, 5, tzinfo=dt.UTC)
    assert strategy.evaluate("X", _crossed_up(), _ctx(at_0905_jst, holding=False)) is None
    kept = strategy.evaluate("X", _crossed_up(), _ctx(at_0905_jst, holding=True))
    assert kept is not None and kept.direction == 1.0
    at_1000_jst = dt.datetime(2026, 8, 3, 1, 0, tzinfo=dt.UTC)
    entry = strategy.evaluate("X", _crossed_up(), _ctx(at_1000_jst, holding=False))
    assert entry is not None and entry.direction == 1.0 and "上抜け" in entry.reason


def test_session_window_uses_the_exchange_timezone() -> None:
    window = SessionWindow.parse({"start": "09:30", "end": "16:00"}, Market.US)
    assert window is not None
    # 13:30 UTC = 09:30 ET（夏時間）
    assert window.allows(dt.datetime(2026, 8, 3, 13, 30, tzinfo=dt.UTC))
    assert not window.allows(dt.datetime(2026, 8, 3, 13, 25, tzinfo=dt.UTC))
    assert SessionWindow.parse(None, Market.JP) is None
    with pytest.raises(ValueError, match="HH:MM"):
        SessionWindow.parse({"start": "9am", "end": "15:00"}, Market.JP)


# --- ライブ ------------------------------------------------------------


def test_live_runner_refuses_intraday_until_epoch_handling_exists(tmp_path: Path) -> None:
    config = Config(settings=AppSettings(data_dir=tmp_path), file=make_config("5m"))
    with pytest.raises(NotImplementedError, match="ライブ運用は日足のみ"):
        LiveRunner(
            config=config,
            strategies=[AlwaysBuy()],
            broker=PaperBroker(),
            store=BarStore(tmp_path, Interval.M5),
            journal=Journal(tmp_path / "j.db"),
        )
