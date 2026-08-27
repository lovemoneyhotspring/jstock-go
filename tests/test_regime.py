"""相場レジーム・待機資金の利息・Chandelier トレーリング・質スクリーニングの検証。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import polars as pl
import pytest

from wbjp.broker.paper import PaperBroker
from wbjp.config import FileConfig, RegimeConfig, StopsConfig, UniverseConfig
from wbjp.data.fundamentals import QualityThresholds, evaluate
from wbjp.domain.models import Position
from wbjp.engine.backtest import DecisionPipeline
from wbjp.risk.stops import StopBook

D = Decimal


def _bars(closes: list[float], start: dt.date = dt.date(2020, 1, 1)) -> pl.DataFrame:
    dates, current = [], start
    while len(dates) < len(closes):
        if current.weekday() < 5:
            dates.append(current)
        current += dt.timedelta(days=1)
    return pl.DataFrame(
        {
            "date": dates,
            "open": closes,
            "high": [c * 1.01 for c in closes],
            "low": [c * 0.99 for c in closes],
            "close": closes,
            "volume": [1_000_000] * len(closes),
        }
    )


def _pipeline(**regime: object) -> DecisionPipeline:
    config = FileConfig(
        universe=UniverseConfig(market="US", symbols=["SPY", "AAA"]),
        regime=RegimeConfig(enabled=True, sma_long=20, sma_mid=5, slope_lookback=3, **regime),
    )
    return DecisionPipeline([], config)


# --- レジーム判定 -------------------------------------------------------


def test_regime_is_bull_when_above_rising_long_ma() -> None:
    pipeline = _pipeline()
    rising = [100.0 + i for i in range(40)]
    indexed = pipeline.index_by_date(pipeline.precompute_indicators({"SPY": _bars(rising)}))
    day = _bars(rising)["date"][-1]
    assert pipeline.regime_exposure(indexed, day) == ("bull", D(1))


def test_regime_is_bear_below_long_ma_and_unknown_during_warmup() -> None:
    pipeline = _pipeline()
    falling = [140.0 - i for i in range(40)]
    frame = _bars(falling)
    indexed = pipeline.index_by_date(pipeline.precompute_indicators({"SPY": frame}))
    assert pipeline.regime_exposure(indexed, frame["date"][-1]) == ("bear", D(0))
    # ウォームアップ中（SMA 未確定）は弱気扱い
    assert pipeline.regime_exposure(indexed, frame["date"][3]) == ("bear", D(0))


def test_regime_is_caution_when_below_mid_ma_but_above_long_ma() -> None:
    pipeline = _pipeline()
    closes = [100.0 + 2 * i for i in range(35)] + [165.0, 163.0, 162.0, 161.0, 160.0]
    frame = _bars(closes)
    indexed = pipeline.index_by_date(pipeline.precompute_indicators({"SPY": frame}))
    assert pipeline.regime_exposure(indexed, frame["date"][-1]) == ("caution", D("0.5"))


def test_regime_disabled_is_always_bull() -> None:
    config = FileConfig(universe=UniverseConfig(market="US", symbols=["AAA"]))
    pipeline = DecisionPipeline([], config)
    assert pipeline.regime_exposure({}, dt.date(2020, 1, 1)) == ("bull", D(1))


def test_regime_config_rejects_unordered_exposure() -> None:
    with pytest.raises(ValueError, match="弱気 ≤ 警戒 ≤ 強気"):
        RegimeConfig(exposure_bull=D("0.5"), exposure_caution=D("0.8"))


# --- 待機資金の利息 -----------------------------------------------------


def test_accrue_interest_uses_360_day_convention() -> None:
    broker = PaperBroker(initial_cash=D(360_000), currency="USD")
    interest = broker.accrue_interest(D("0.05"), days=1)
    assert interest == D("50.00")
    assert broker.get_balance().cash_balance == D("360050.00")
    assert broker.accrue_interest(D(0)) == D(0)


# --- Chandelier トレーリング -------------------------------------------------


def test_trailing_multiple_can_differ_from_initial_stop() -> None:
    book = StopBook()
    position = Position(
        symbol="AAA", quantity=D(10), available_quantity=D(10), cost_price=D(100), last_price=D(100)
    )
    book.ensure(
        {"AAA": position},
        {"AAA": D(2)},
        dt.date(2020, 1, 1),
        atr_multiple=D("1.5"),
        trailing=True,
        trailing_atr_multiple=D("2.5"),
    )
    stop = book.get("AAA")
    assert stop is not None
    assert stop.stop_price == D(97)  # 初期: 100 − 1.5 × 2
    book.update_trailing({"AAA": D(120)}, {"AAA": D(2)})
    assert book.get("AAA").stop_price == D(115)  # 追従: 120 − 2.5 × 2


def test_stops_config_accepts_trailing_multiple() -> None:
    assert StopsConfig(trailing_atr_multiple=D("2.5")).trailing_atr_multiple == D("2.5")


# --- 質スクリーニング -------------------------------------------------------


def _statements(**overrides: list[float]) -> dict[str, object]:
    base = {
        "symbol": "GOOD",
        "revenue": [100.0, 90.0, 80.0, 70.0],
        "gross_profit": [60.0, 54.0, 48.0, 42.0],
        "operating_income": [30.0, 27.0, 24.0, 21.0],
        "ebit": [30.0, 27.0, 24.0, 21.0],
        "interest_expense": [1.0, 1.0, 1.0, 1.0],
        "net_income": [20.0, 18.0, 16.0, 14.0],
        "equity": [80.0, 75.0, 70.0, 65.0],
        "total_debt": [20.0, 20.0, 20.0, 20.0],
        "fcf": [24.0, 20.0, 17.0, 15.0],
    }
    base.update(overrides)
    return base


def test_quality_passes_a_textbook_compounder() -> None:
    report = evaluate(_statements())
    assert report.passed, report.failed
    assert report.metrics["roe_min"] == pytest.approx(14 / 65)
    assert report.metrics["interest_coverage"] == 30.0


def test_quality_fails_on_each_weak_point() -> None:
    assert any("ROE" in f for f in evaluate(_statements(equity=[200.0] * 4)).failed)
    assert any("粗利率" in f for f in evaluate(_statements(gross_profit=[60, 54, 48, 20])).failed)
    assert any("D/E" in f for f in evaluate(_statements(total_debt=[100.0] * 4)).failed)
    assert any("FCF/純利益" in f for f in evaluate(_statements(fcf=[10, 20, 17, 15])).failed)
    assert any("FCF 成長" in f for f in evaluate(_statements(fcf=[15, 15, 15, 15])).failed)
    assert any("履歴" in f for f in evaluate(_statements(revenue=[100.0, 90.0])).failed)


def test_quality_thresholds_are_adjustable() -> None:
    lenient = QualityThresholds(min_roe=0.05)
    assert evaluate(_statements(equity=[200.0] * 4), lenient).passed


# --- %トレーリング・出口の種類 ---------------------------------------------


def test_percent_trailing_follows_highest_close() -> None:
    book = StopBook()
    position = Position(
        symbol="AAA", quantity=D(10), available_quantity=D(10), cost_price=D(100), last_price=D(100)
    )
    book.ensure(
        {"AAA": position}, {"AAA": D(2)}, dt.date(2020, 1, 1), trailing=True, trailing_pct=D("0.1")
    )
    book.update_trailing({"AAA": D(150)}, {})  # ATR が無くても % なら動く
    assert book.get("AAA").stop_price == D(135)
    book.update_trailing({"AAA": D(120)}, {})
    assert book.get("AAA").stop_price == D(135)  # 下げない


def test_trend_exit_kind_is_validated_and_named() -> None:
    from wbjp.config import UniverseConfig

    with pytest.raises(ValueError, match="trend_exit_kind"):
        StopsConfig(trend_exit_kind="wma")
    config = FileConfig(
        universe=UniverseConfig(market="US", symbols=["AAA"]),
        stops=StopsConfig(trend_exit_sma=10, trend_exit_kind="donchian"),
    )
    pipeline = DecisionPipeline([], config)
    frame = pipeline.precompute_indicators({"AAA": _bars([100.0 + i for i in range(30)])})["AAA"]
    assert pipeline.trend_exit_col in frame.columns
    # 前日までの 10 日安値なので、当日終値より必ず低い
    assert frame[pipeline.trend_exit_col][-1] < frame["close"][-1]


# --- 復元した rsi_pullback -------------------------------------------------


def test_rsi_pullback_is_registered_with_switchable_exits() -> None:
    from wbjp.strategy.registry import get

    cls = get("rsi_pullback")
    strategy = cls(exit_on_rsi=False, exit_on_high=False, exit_on_sma_recovery=False)
    assert strategy.name == "rsi_pullback"
    assert not strategy.exit_on_rsi


def test_equal_weight_budget_is_capped_by_buying_power() -> None:
    from wbjp.config import SizingConfig
    from wbjp.portfolio.sizer import SizingContext, build_sizer

    sizer = build_sizer(SizingConfig(method="equal_weight", max_positions=2))
    ctx = SizingContext(
        equity=D(10_000),
        buying_power=D(1_000),  # 含み益で評価額は大きいが現金は少ない
        prices={"AAA": D(10)},
        atr={},
        lot_sizes={},
        positions={},
        default_lot_size=D(1),
    )
    assert sizer._quantity("AAA", None, ctx) == D(99)  # 1,000 × 0.99 / 10
