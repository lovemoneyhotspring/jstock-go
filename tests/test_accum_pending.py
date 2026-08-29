"""ライブの積立の規則: 確定足で判断し、当日の価格で買い、未発注ぶんを繰り越す。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

import polars as pl
import pytest

from accum.config import AccumConfig
from accum.execute import build_orders, pending_contributions
from wbcore.domain.models import Fill, Order, OrderStatus, OrderType, Side, TaxAccountType

JST = dt.timezone(dt.timedelta(hours=9))


def _bars(closes: list[float], *, start: dt.date) -> pl.DataFrame:
    dates: list[dt.date] = []
    day = start
    while len(dates) < len(closes):
        if day.weekday() < 5:
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


def _config(**execution) -> AccumConfig:  # type: ignore[no-untyped-def]
    return AccumConfig.model_validate(
        {
            "monthly_budget": 10_000,
            "execution": execution,
            "tactics": [{"id": "a", "tactic": "constant", "symbols": ["T"], "window": False}],
        }
    )


# 2026-08-03（月）が 8 月の入金日。足は 7/1 から。
PAYDAY = dt.date(2026, 8, 3)


def _at(day: dt.date, hour: int = 14) -> dt.datetime:
    return dt.datetime.combine(day, dt.time(hour, 5), tzinfo=JST)


def test_payday_is_due_the_day_after_it_closes_using_todays_price() -> None:
    """入金日 D の投下は、D の足が確定した翌日 D+1 に、D+1 の価格で買う（バックテストと同じ）。"""
    n = 24  # 7/1 〜 8/3
    bars = _bars([100.0] * (n - 1) + [123.0], start=dt.date(2026, 7, 1))  # 最終行 = 8/3（入金日）
    # 8/3 の 14:05 JST: 8/3 の足はザラ場中の途中経過 → まだ判定しない
    assert pending_contributions(_config(), {"T": bars}, now=_at(PAYDAY)).contributions == []
    # 8/4 の 14:05 JST: 8/3 が確定 → 入金日の投下が出る。価格は最新（8/4 の途中経過）
    with_today = pl.concat([bars, _bars([150.0], start=dt.date(2026, 8, 4))])
    (c,) = pending_contributions(
        _config(), {"T": with_today}, now=_at(dt.date(2026, 8, 4))
    ).contributions
    assert c.date == PAYDAY and c.amount == Decimal(10_000)
    assert c.close == Decimal(150)  # 買う価格は当日


def test_missed_payday_is_carried_forward_with_a_stable_order_id() -> None:
    """入金日に cron が動かなくても、7 日以内なら次の実行で買う。注文IDは計画の日付から。"""
    bars = _bars([100.0] * 27, start=dt.date(2026, 7, 1))  # 7/1 〜 8/6
    on_aug_5 = pending_contributions(_config(), {"T": bars}, now=_at(dt.date(2026, 8, 5)))
    on_aug_7 = pending_contributions(_config(), {"T": bars}, now=_at(dt.date(2026, 8, 7)))
    assert [c.date for c in on_aug_5.contributions] == [PAYDAY]
    assert [c.date for c in on_aug_7.contributions] == [PAYDAY]
    (o5,) = build_orders(
        on_aug_5.contributions, tax_type=TaxAccountType.SPECIFIC, lot_sizes={"T": 1}
    )
    (o7,) = build_orders(
        on_aug_7.contributions, tax_type=TaxAccountType.SPECIFIC, lot_sizes={"T": 1}
    )
    assert o5.request is not None and o7.request is not None
    assert o5.request.client_order_id == o7.request.client_order_id  # 台帳で弾ける


def test_catch_up_window_is_bounded() -> None:
    bars = _bars([100.0] * 40, start=dt.date(2026, 7, 1))  # 7/1 〜 8/25
    late = pending_contributions(
        _config(), {"T": bars}, now=_at(dt.date(2026, 8, 20)), lookback_days=7
    )
    assert late.contributions == []  # 入金日から 17 日: 繰り越し窓の外


def test_stale_bars_are_not_judged_and_are_reported() -> None:
    bars = _bars([100.0] * 24, start=dt.date(2026, 7, 1))  # 最終 8/3
    pending = pending_contributions(
        _config(), {"T": bars}, now=_at(dt.date(2026, 8, 12)), max_stale_days=4
    )
    assert pending.contributions == []
    assert pending.stale == {"T": PAYDAY}


def test_signal_bars_are_also_restricted_to_completed_days() -> None:
    config = AccumConfig.model_validate(
        {
            "monthly_budget": 10_000,
            "tactics": [
                {
                    "id": "a",
                    "tactic": "bear_stack",
                    "symbols": ["T"],
                    "signal_symbol": "S",
                    "multiplier": 4,
                    "window": False,
                }
            ],
        }
    )
    n = 300
    target = _bars([100.0] * n, start=dt.date(2025, 6, 2))
    falling = [1000.0 - i for i in range(n)]
    # 判定用の最終行（当日）だけ配列を崩す。当日は確定していないので判定に使われない
    signal = _bars([*falling[:-1], 5000.0], start=dt.date(2025, 6, 2))
    today = target["date"][-1]
    pending = pending_contributions(config, {"T": target, "S": signal}, now=_at(today))
    assert all(c.multiplier == 4.0 for c in pending.contributions)


# --- 時刻の規約: モデルは tz 無しの時刻を受け付けない ---


def test_domain_models_reject_naive_timestamps() -> None:
    with pytest.raises(ValueError, match="時間帯が必要"):
        Order(
            client_order_id="x",
            broker_order_id=None,
            symbol="T",
            side=Side.BUY,
            order_type=OrderType.MARKET,
            quantity=Decimal(1),
            filled_quantity=Decimal(0),
            status=OrderStatus.SUBMITTED,
            created_at=dt.datetime(2026, 8, 3, 5, 5),
        )
    with pytest.raises(ValueError, match="時間帯が必要"):
        Fill("x", "T", Side.BUY, Decimal(1), Decimal(100), filled_at=dt.datetime(2026, 8, 3))
    assert Fill("x", "T", Side.BUY, Decimal(1), Decimal(100), filled_at=_at(PAYDAY)).filled_at
