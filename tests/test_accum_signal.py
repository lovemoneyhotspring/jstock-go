"""判定用の銘柄（signal_symbol）で倍率を決め、買うのは別の銘柄。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import polars as pl

from accum.config import AccumConfig
from accum.execute import todays_contributions
from accum.plan import AccumulationSettings, build_plan
from accum.tactics import BearStack
from wbcore.domain.models import Market


def _bars(closes: list[float], *, start: dt.date, weekdays_only: bool = True) -> pl.DataFrame:
    dates: list[dt.date] = []
    day = start
    while len(dates) < len(closes):
        if not weekdays_only or day.weekday() < 5:
            dates.append(day)
        day += dt.timedelta(days=1)
    return pl.DataFrame(
        {
            "date": dates,
            "open": closes,
            "high": [c * 1.01 for c in closes],
            "low": [c * 0.99 for c in closes],
            "close": closes,
            "volume": [1.0] * len(closes),
        }
    )


def _falling(n: int) -> list[float]:
    """完全下降配列になる系列（終値 < MA20 < MA50 < MA200）。"""
    return [1000.0 - i for i in range(n)]


def _flat(n: int) -> list[float]:
    return [100.0] * n


START = dt.date(2025, 1, 1)


def test_multiplier_comes_from_the_signal_bars_not_the_purchased_ones() -> None:
    n = 260
    target = _bars(_flat(n), start=START)  # 買う銘柄は横ばい → 自身なら倍率 1
    signal = _bars(_falling(n), start=START)  # 判定用は下降配列 → 倍率 4
    settings = AccumulationSettings(Decimal(10_000), BearStack(multiplier=4))

    own = build_plan(target, settings)
    judged = build_plan(target, settings, signal_bars=signal)
    assert own["multiplier"][-1] == 1.0
    assert judged["multiplier"][-1] == 4.0
    # 投下額の入金日と終値は買う銘柄のまま
    assert judged["close"][-1] == 100.0
    assert judged.filter(pl.col("base") > 0).height == own.filter(pl.col("base") > 0).height


def test_signal_with_a_different_calendar_uses_the_latest_prior_value() -> None:
    """判定用が別の暦（祝日のずれ・米国市場）でも、その日以前の最新値を当てる。"""
    # 判定用は 1 日おきにしか足が無い（600 営業日を間引いて 300 本 → MA200 は揃う）
    signal = _bars(_falling(600), start=START).with_row_index().filter(pl.col("index") % 2 == 0)
    # 買う銘柄は判定用の途中から始まり、判定用に無い日も多く含む
    target = _bars(_flat(60), start=START + dt.timedelta(days=700))
    judged = build_plan(
        target, AccumulationSettings(Decimal(10_000), BearStack()), signal_bars=signal.drop("index")
    )
    assert judged.height == 60
    assert judged["multiplier"].null_count() == 0
    assert judged["multiplier"][-1] == 4.0


def test_days_before_the_signal_exists_get_multiplier_one() -> None:
    target = _bars(_flat(30), start=START)
    signal = _bars(_falling(5), start=START + dt.timedelta(days=60))  # 判定用は後から始まる
    judged = build_plan(
        target, AccumulationSettings(Decimal(10_000), BearStack()), signal_bars=signal
    )
    assert judged["multiplier"].to_list() == [1.0] * 30


def test_config_collects_signal_symbols_for_fetching_but_not_for_buying() -> None:
    config = AccumConfig.model_validate(
        {
            "tactics": [
                {
                    "id": "a",
                    "tactic": "bear_stack",
                    "symbols": ["452A.T"],
                    "market": "JP",
                    "signal_symbol": "^GSPC",
                    "signal_market": "US",
                    "monthly_budget": 10_000,
                },
                {
                    "id": "b",
                    "tactic": "constant",
                    "symbols": ["1306.T"],
                    "monthly_budget": 10_000,
                },
            ]
        }
    )
    assert config.symbols == ["1306.T", "452A.T"]
    assert config.all_symbols == ["1306.T", "452A.T", "^GSPC"]
    assert config.symbols_by_market() == {Market.JP: ["1306.T", "452A.T"], Market.US: ["^GSPC"]}
    entry = config.tactics[0]
    assert entry.signal_market_resolved is Market.US
    assert entry.params == {}  # signal_* は戦略のパラメータに漏れない
    assert config.tactics[1].signal_market_resolved is Market.JP


def test_todays_contributions_judge_with_the_signal_symbol() -> None:
    n = 260
    config = AccumConfig.model_validate(
        {
            "monthly_budget": 10_000,
            "tactics": [
                {
                    "id": "a",
                    "tactic": "bear_stack",
                    "symbols": ["T"],
                    "signal_symbol": "S",
                    "multiplier": 8,  # 1 週ぶんの増額が基本予算を超え、毎週の月曜に出る
                    "window": False,
                    "monthly_budget": 10_000,
                }
            ],
        }
    )
    bars = {"T": _bars(_flat(n), start=START), "S": _bars(_falling(n), start=START)}
    # 増額は翌週の月曜にまとまるので、最終日を月曜に揃える
    last_monday = bars["T"].filter(pl.col("date").dt.weekday() == 1)["date"].max()
    bars = {k: f.filter(pl.col("date") <= last_monday) for k, f in bars.items()}
    (c,) = todays_contributions(config, bars)
    assert c.symbol == "T" and c.multiplier == 8.0 and c.close == Decimal(100)
    assert c.amount > 0  # 下降配列なので先週ぶんの増額が月曜に出る
    # 判定用の足が無ければ倍率 1（増額なし）。最終日が入金日でなければ投下も無い
    assert todays_contributions(config, {"T": bars["T"]}) == []


def test_signal_from_a_market_that_closes_later_uses_the_previous_day() -> None:
    """東証の銘柄を米国指数で判定: 同じ日付の指数の足は判断時点に無いので前日を使う。"""
    from wbcore.domain.session import closes_after

    assert closes_after(Market.US, Market.JP)
    assert not closes_after(Market.JP, Market.US)
    assert not closes_after(Market.JP, Market.JP)

    n = 260
    target = _bars(_flat(n), start=START)
    signal = _bars(_falling(n), start=START)
    # 判定用の最終日だけ配列を崩す（最後の 1 本を大きく上げる）
    broken = signal.with_columns(
        pl.when(pl.col("date") == signal["date"][-1])
        .then(5000.0)
        .otherwise(pl.col("close"))
        .alias("close")
    )
    settings = AccumulationSettings(Decimal(10_000), BearStack(multiplier=4))
    same_day = build_plan(target, settings, signal_bars=broken)
    previous_day = build_plan(target, settings, signal_bars=broken, signal_strict=True)
    assert same_day["multiplier"][-1] == 1.0  # 最終日の足（崩れた）を見ている
    assert previous_day["multiplier"][-1] == 4.0  # 前日までの足（下降配列）で判定


def test_entry_decides_the_lag_from_the_markets() -> None:
    def entry(**fields):  # type: ignore[no-untyped-def]
        base = {"id": "x", "tactic": "constant", "symbols": ["563A.T"], "monthly_budget": 10_000}
        return AccumConfig.model_validate({"tactics": [{**base, **fields}]}).tactics[0]

    assert entry(market="JP", signal_symbol="^IXIC", signal_market="US").signal_lags
    assert not entry(
        symbols=["VOO"], market="US", signal_symbol="^N225", signal_market="JP"
    ).signal_lags
    assert not entry(signal_symbol="^N225").signal_lags  # 同一市場
    assert not entry().signal_lags  # 判定用なし
