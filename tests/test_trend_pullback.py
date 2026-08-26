"""押し目買い戦略・スクリーニング・時間切れ損切りのテスト。"""

from __future__ import annotations

import datetime as dt
import math
from decimal import Decimal
from pathlib import Path

import polars as pl
import pytest

from wbjp.config import StopsConfig, load_file_config
from wbjp.db.repo import Journal
from wbjp.domain.models import Position
from wbjp.risk.stops import Stop, StopBook
from wbjp.strategy.base import StrategyContext
from wbjp.strategy.registry import create
from wbjp.strategy.samples.trend_pullback import TrendPullbackStrategy, load_blackout

D = Decimal


# --------------------------------------------------------------------------
# 足の生成: 上昇トレンド → 直近で押し目 → 当日反転
# --------------------------------------------------------------------------


def _dates(n: int, start: dt.date = dt.date(2025, 1, 1)) -> list[dt.date]:
    out, cur = [], start
    while len(out) < n:
        if cur.weekday() < 5:
            out.append(cur)
        cur += dt.timedelta(days=1)
    return out


def frame_from(
    closes: list[float], *, volume: float = 2_000_000.0, reversal: bool = True
) -> pl.DataFrame:
    n = len(closes)
    opens = [closes[0], *closes[:-1]]
    highs = [max(o, c) * 1.01 for o, c in zip(opens, closes, strict=True)]
    lows = [min(o, c) * 0.99 for o, c in zip(opens, closes, strict=True)]
    if reversal:
        # 当日を陽線にする（前日終値より少し上で引ける）
        closes = [*closes[:-1], closes[-2] * 1.008]
        highs[-1] = max(highs[-1], closes[-1] * 1.001)
    return pl.DataFrame(
        {
            "date": _dates(n),
            "open": opens,
            "high": highs,
            "low": lows,
            "close": closes,
            "volume": [volume] * n,
        }
    )


def uptrend_with_dip(n: int = 260, dip_days: int = 4, dip: float = 0.02) -> list[float]:
    closes = [100.0 * (1.001**i) * (1 + 0.004 * math.sin(i / 5)) for i in range(n)]
    for k in range(dip_days):
        closes[n - dip_days + k] = closes[n - dip_days - 1] * (1 - dip * (k + 1))
    return [round(c, 2) for c in closes]


def downtrend(n: int = 260) -> list[float]:
    return [round(200.0 * (0.998**i), 2) for i in range(n)]


def benchmark_up(n: int = 260) -> pl.DataFrame:
    return frame_from([round(400.0 * (1.0006**i), 2) for i in range(n)], reversal=False)


def benchmark_down(n: int = 260) -> pl.DataFrame:
    return frame_from(downtrend(n), reversal=False)


def context(
    bars: dict[str, pl.DataFrame], positions: dict[str, Position] | None = None
) -> StrategyContext:
    as_of = max(f["date"].max() for f in bars.values())  # type: ignore[type-var]
    return StrategyContext(as_of=as_of, _bars=bars, _positions=positions or {})  # type: ignore[arg-type]


def strategy(**kw) -> TrendPullbackStrategy:  # type: ignore[no-untyped-def]
    return TrendPullbackStrategy(**{"min_dollar_volume": 1_000_000.0, **kw})


# --------------------------------------------------------------------------
# エントリー条件
# --------------------------------------------------------------------------


def test_registered_under_its_name() -> None:
    assert isinstance(create("trend_pullback"), TrendPullbackStrategy)


def test_signals_a_pullback_in_an_uptrend() -> None:
    bars = {"AAA": frame_from(uptrend_with_dip()), "SPY": benchmark_up()}
    signals = strategy().on_bars(context(bars))

    assert [s.symbol for s in signals] == ["AAA"]
    sig = signals[0]
    assert 0.3 <= sig.direction <= 1.0
    assert sig.meta["rsi"] < 20
    assert "score" in sig.meta


def test_no_signal_in_a_downtrend() -> None:
    bars = {"AAA": frame_from(downtrend()), "SPY": benchmark_up()}
    assert strategy().on_bars(context(bars)) == []


def test_no_signal_without_a_dip() -> None:
    bars = {"AAA": frame_from(uptrend_with_dip(dip_days=0)), "SPY": benchmark_up()}
    assert strategy().on_bars(context(bars)) == []


def test_no_signal_without_reversal_bar() -> None:
    bars = {"AAA": frame_from(uptrend_with_dip(), reversal=False), "SPY": benchmark_up()}
    assert strategy().on_bars(context(bars)) == []
    assert strategy(require_reversal_bar=False).on_bars(context(bars))


def test_market_filter_blocks_entries_when_benchmark_is_weak() -> None:
    bars = {"AAA": frame_from(uptrend_with_dip()), "SPY": benchmark_down()}
    assert strategy().on_bars(context(bars)) == []


def test_missing_benchmark_blocks_entries_rather_than_guessing() -> None:
    bars = {"AAA": frame_from(uptrend_with_dip())}
    assert strategy().on_bars(context(bars)) == []
    assert strategy(benchmark=None).on_bars(context(bars))


def test_benchmark_itself_is_never_a_candidate() -> None:
    bars = {"SPY": frame_from(uptrend_with_dip())}
    assert strategy().on_bars(context(bars)) == []


def test_illiquid_symbols_are_screened_out() -> None:
    bars = {"AAA": frame_from(uptrend_with_dip(), volume=1_000.0), "SPY": benchmark_up()}
    assert strategy().on_bars(context(bars)) == []


def test_deep_drawdown_is_a_breakdown_not_a_pullback() -> None:
    bars = {"AAA": frame_from(uptrend_with_dip(dip_days=6, dip=0.04)), "SPY": benchmark_up()}
    assert strategy(max_drawdown_from_high=0.10).on_bars(context(bars)) == []


def test_screen_explains_every_failed_condition() -> None:
    s = strategy()
    s._market_ok = True
    frame = frame_from(downtrend(), volume=1_000.0, reversal=False).with_columns(s.indicators())
    result = s.screen(frame)
    assert not result.passed
    assert any("トレンド" in f for f in result.failed)
    assert any("売買代金" in f for f in result.failed)
    assert any("陽線" in f for f in result.failed)


# --------------------------------------------------------------------------
# 順位付け
# --------------------------------------------------------------------------


def test_deeper_dip_ranks_higher() -> None:
    bars = {
        "SHALLOW": frame_from(uptrend_with_dip(dip=0.012)),
        "DEEP": frame_from(uptrend_with_dip(dip=0.03)),
        "SPY": benchmark_up(),
    }
    signals = {s.symbol: s for s in strategy().on_bars(context(bars))}
    assert set(signals) == {"SHALLOW", "DEEP"}
    assert signals["DEEP"].direction > signals["SHALLOW"].direction
    assert signals["DEEP"].meta["dip"] > signals["SHALLOW"].meta["dip"]


def test_more_liquid_symbol_ranks_higher_all_else_equal() -> None:
    bars = {
        "THIN": frame_from(uptrend_with_dip(), volume=9_000.0),  # 売買代金 ≈ 110万ドル
        "THICK": frame_from(uptrend_with_dip(), volume=50_000_000.0),
        "SPY": benchmark_up(),
    }
    signals = {s.symbol: s for s in strategy().on_bars(context(bars))}
    assert signals["THICK"].direction > signals["THIN"].direction


# --------------------------------------------------------------------------
# 手仕舞い
# --------------------------------------------------------------------------


def held(symbol: str = "AAA", cost: str = "100") -> dict[str, Position]:
    return {symbol: Position(symbol, D(10), D(10), D(cost), D(cost), currency="USD")}


def test_holds_while_the_dip_is_recovering() -> None:
    bars = {"AAA": frame_from(uptrend_with_dip()), "SPY": benchmark_up()}
    signals = strategy().on_bars(context(bars, held(cost="200")))
    assert signals[0].direction == 0.5


def test_exits_on_overbought_rsi() -> None:
    closes = uptrend_with_dip(dip_days=0)
    closes[-3:] = [closes[-4] * 1.03, closes[-4] * 1.06, closes[-4] * 1.09]
    bars = {"AAA": frame_from(closes, reversal=False), "SPY": benchmark_up()}
    signals = strategy().on_bars(context(bars, held(cost="50")))
    assert signals[0].direction == -1.0
    assert "買われすぎ" in signals[0].reason or "高値" in signals[0].reason


def recovered_above_sma20() -> list[float]:
    """SMA20 の少し上で、RSI が中立圏、直近高値には届いていない足。"""
    closes = uptrend_with_dip(dip_days=0)
    base = closes[-5]
    closes[-4:] = [base * 1.02, base * 1.01, base * 1.025, base * 1.02]
    return closes


def test_exits_when_profitable_and_back_above_short_sma() -> None:
    bars = {"AAA": frame_from(recovered_above_sma20(), reversal=False), "SPY": benchmark_up()}
    signals = strategy(rsi_exit=99.0).on_bars(context(bars, held(cost="1")))
    assert signals[0].direction == -1.0
    assert "第一目標" in signals[0].reason


def test_does_not_take_first_target_at_a_loss() -> None:
    bars = {"AAA": frame_from(recovered_above_sma20(), reversal=False), "SPY": benchmark_up()}
    signals = strategy(rsi_exit=99.0).on_bars(context(bars, held(cost="100000")))
    assert signals[0].direction == 0.5


# --------------------------------------------------------------------------
# 決算ブラックアウト
# --------------------------------------------------------------------------


def test_blackout_file_round_trip(tmp_path: Path) -> None:
    path = tmp_path / "earnings.toml"
    path.write_text('[earnings]\nAAA = ["2025-12-30", 2026-01-28]\n', encoding="utf-8")
    assert load_blackout(path) == {"AAA": [dt.date(2025, 12, 30), dt.date(2026, 1, 28)]}


def test_blackout_blocks_entry_and_forces_exit(tmp_path: Path) -> None:
    bars = {"AAA": frame_from(uptrend_with_dip()), "SPY": benchmark_up()}
    as_of = bars["AAA"]["date"].max()
    path = tmp_path / "earnings.toml"
    path.write_text(f'[earnings]\nAAA = ["{as_of + dt.timedelta(days=2)}"]\n', encoding="utf-8")

    s = strategy(blackout_file=str(path), blackout_days_before=3)
    assert s.on_bars(context(bars)) == []
    exit_signal = s.on_bars(context(bars, held(cost="200")))[0]
    assert exit_signal.direction == -1.0
    assert "決算" in exit_signal.reason


def test_missing_blackout_file_is_loud() -> None:
    with pytest.raises(FileNotFoundError):
        strategy(blackout_file="/nonexistent/earnings.toml")


# --------------------------------------------------------------------------
# 銘柄リストファイル
# --------------------------------------------------------------------------


def test_symbols_file_merges_into_allowlist(tmp_path: Path) -> None:
    (tmp_path / "universe.txt").write_text(
        "SPY  # bench\nAAPL\n\n# comment\nMSFT\nAAPL\n", encoding="utf-8"
    )
    (tmp_path / "settings.toml").write_text(
        '[universe]\nmarket = "US"\nsymbols = ["NVDA"]\nsymbols_file = "universe.txt"\n',
        encoding="utf-8",
    )
    config = load_file_config(tmp_path)
    assert config.universe.symbols == ["NVDA", "SPY", "AAPL", "MSFT"]


def test_missing_symbols_file_is_an_error(tmp_path: Path) -> None:
    (tmp_path / "settings.toml").write_text(
        '[universe]\nsymbols_file = "nope.txt"\n', encoding="utf-8"
    )
    with pytest.raises(FileNotFoundError):
        load_file_config(tmp_path)


def test_shipped_us_config_loads() -> None:
    config = load_file_config(Path("config/us"))
    assert "SPY" in config.universe.symbols
    assert config.strategies.enabled[0].name == "trend_pullback"
    assert config.stops.max_hold_days == 10


# --------------------------------------------------------------------------
# ストップの拡張: 建値移動・時間切れ
# --------------------------------------------------------------------------

TODAY = dt.date(2026, 8, 25)  # 火曜


def a_stop(entry: str = "100", stop: str = "95", created: dt.date = TODAY) -> Stop:
    return Stop("AAA", D(stop), D(entry), created, initial_stop_price=D(stop))


def test_initial_risk_survives_stop_moves() -> None:
    stop = a_stop()
    assert stop.initial_risk == D(5)
    moved = Stop("AAA", D(100), D(100), TODAY, initial_stop_price=D(95))
    assert moved.initial_risk == D(5)
    assert moved.risk_per_share == D(0)


def test_breakeven_moves_stop_to_entry_after_one_r() -> None:
    book = StopBook({"AAA": a_stop()})
    book.update_breakeven({"AAA": D("104")}, D("1.0"))
    assert book.get("AAA").stop_price == D(95)  # type: ignore[union-attr]

    book.update_breakeven({"AAA": D("105")}, D("1.0"))
    assert book.get("AAA").stop_price == D(100)  # type: ignore[union-attr]


def test_breakeven_never_lowers_a_trailed_stop() -> None:
    book = StopBook({"AAA": Stop("AAA", D(108), D(100), TODAY, initial_stop_price=D(95))})
    book.update_breakeven({"AAA": D("120")}, D("1.0"))
    assert book.get("AAA").stop_price == D(108)  # type: ignore[union-attr]


def test_breakeven_disabled_when_none() -> None:
    book = StopBook({"AAA": a_stop()})
    book.update_breakeven({"AAA": D("150")}, None)
    assert book.get("AAA").stop_price == D(95)  # type: ignore[union-attr]


def test_trading_days_skip_weekends() -> None:
    stop = a_stop(created=dt.date(2026, 8, 21))  # 金曜
    assert stop.trading_days_held(dt.date(2026, 8, 24)) == 1  # 月曜
    assert stop.trading_days_held(dt.date(2026, 8, 28)) == 5
    assert stop.trading_days_held(dt.date(2026, 8, 21)) == 0


def test_stale_exit_only_when_not_profitable() -> None:
    book = StopBook({"AAA": a_stop(created=dt.date(2026, 8, 18))})  # 5営業日前
    assert book.time_exit_targets({"AAA": D("100")}, TODAY, stale_days=5) != []
    assert book.time_exit_targets({"AAA": D("101")}, TODAY, stale_days=5) == []
    assert book.time_exit_targets({"AAA": D("90")}, TODAY, stale_days=6) == []


def test_max_hold_exits_regardless_of_profit() -> None:
    book = StopBook({"AAA": a_stop(created=dt.date(2026, 8, 11))})  # 10営業日前
    targets = book.time_exit_targets({"AAA": D("130")}, TODAY, max_days=10)
    assert targets and targets[0].quantity == 0


def test_journal_persists_initial_stop_and_migrates_old_db(tmp_path: Path) -> None:
    import sqlite3

    path = tmp_path / "old.db"
    with sqlite3.connect(path) as conn:
        conn.execute(
            "CREATE TABLE stops (symbol TEXT PRIMARY KEY, stop_price TEXT NOT NULL, "
            "entry_price TEXT NOT NULL, created_on TEXT NOT NULL, trailing INTEGER NOT NULL DEFAULT 0, "
            "atr_multiple TEXT NOT NULL DEFAULT '2.0', highest_close TEXT, updated_at TEXT)"
        )
        conn.execute(
            "INSERT INTO stops VALUES ('OLD', '95', '100', '2026-08-01', 0, '2.0', NULL, NULL)"
        )

    journal = Journal(path)
    loaded = journal.load_stops()
    assert loaded["OLD"].initial_stop_price is None
    assert loaded["OLD"].initial_risk == D(5)  # 旧レコードは現在のストップを初期値とみなす

    journal.save_stops({"AAA": a_stop()})
    assert journal.load_stops()["AAA"].initial_stop_price == D(95)
    journal.close()


def test_stops_config_validation() -> None:
    with pytest.raises(ValueError, match="stale_exit_days"):
        StopsConfig(stale_exit_days=10, max_hold_days=5)
    with pytest.raises(ValueError, match="正の数"):
        StopsConfig(breakeven_after_r=D(0))
