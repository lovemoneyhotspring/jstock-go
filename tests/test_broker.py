"""ブローカー層のテスト。"""

from __future__ import annotations

from decimal import Decimal

import pytest

from wbcore.broker.base import InsufficientFundsError, OrderRejectedError
from wbcore.broker.paper import PaperBroker
from wbcore.broker.ratelimit import Cached, Limit, RateLimiter
from wbcore.domain.models import (
    OrderRequest,
    OrderStatus,
    OrderType,
    Side,
    TaxAccountType,
)

D = Decimal


def buy(symbol: str = "7203", qty: int = 100, price: str | None = "2500", oid: str = "o1"):  # type: ignore[no-untyped-def]
    return OrderRequest(
        client_order_id=oid,
        symbol=symbol,
        side=Side.BUY,
        order_type=OrderType.LIMIT if price else OrderType.MARKET,
        quantity=D(qty),
        limit_price=D(price) if price else None,
        tax_type=TaxAccountType.SPECIFIC,
    )


def sell(symbol: str = "7203", qty: int = 100, price: str | None = "2500", oid: str = "s1"):  # type: ignore[no-untyped-def]
    return OrderRequest(
        client_order_id=oid,
        symbol=symbol,
        side=Side.SELL,
        order_type=OrderType.LIMIT if price else OrderType.MARKET,
        quantity=D(qty),
        limit_price=D(price) if price else None,
    )


# --------------------------------------------------------------------------
# OrderRequest の検証
# --------------------------------------------------------------------------


def test_limit_order_requires_a_price() -> None:
    with pytest.raises(ValueError, match="limit_price が必要"):
        OrderRequest("o", "7203", Side.BUY, OrderType.LIMIT, D(100))


def test_market_order_rejects_a_price() -> None:
    with pytest.raises(ValueError, match="limit_price は指定できない"):
        OrderRequest("o", "7203", Side.BUY, OrderType.MARKET, D(100), limit_price=D(1))


def test_quantity_must_be_positive() -> None:
    with pytest.raises(ValueError, match="quantity"):
        OrderRequest("o", "7203", Side.BUY, OrderType.MARKET, D(0))


def test_client_order_id_length_is_capped() -> None:
    """注文IDの上限は32文字。超えると発注時に弾かれる。"""
    with pytest.raises(ValueError, match="32文字"):
        OrderRequest("x" * 33, "7203", Side.BUY, OrderType.MARKET, D(100))


def test_order_status_openness() -> None:
    assert OrderStatus.SUBMITTED.is_open
    assert OrderStatus.PARTIALLY_FILLED.is_open
    assert OrderStatus.FILLED.is_terminal
    assert OrderStatus.CANCELLED.is_terminal


def test_unknown_status_is_treated_as_still_open() -> None:
    """未知の状態を「終了」と誤認すると二重発注につながる。安全側に倒す。"""
    assert OrderStatus.UNKNOWN.is_open is True


def test_other_order_type_can_never_be_placed() -> None:
    """OTHER は読み取り専用。発注経路に流れてはいけない。"""
    assert OrderType.OTHER.is_placeable is False
    assert OrderType.MARKET.is_placeable is True
    assert OrderType.LIMIT.is_placeable is True


# --------------------------------------------------------------------------
# PaperBroker
# --------------------------------------------------------------------------


def test_paper_broker_starts_with_the_given_cash() -> None:
    broker = PaperBroker(initial_cash=D(1_000_000))
    assert broker.get_balance().cash_balance == D(1_000_000)
    assert broker.equity == D(1_000_000)


def test_paper_order_is_not_filled_until_settled() -> None:
    """発注しただけでは約定しない。翌営業日の寄付で約定させる。"""
    broker = PaperBroker(initial_cash=D(1_000_000))
    broker.place(buy())

    assert broker.get_positions() == []
    assert len(broker.get_open_orders()) == 1


def test_paper_limit_buy_fills_at_the_open_when_favourable() -> None:
    broker = PaperBroker(initial_cash=D(1_000_000))
    broker.place(buy(price="2500"))

    fills = broker.settle({"7203": D("2480")})

    assert len(fills) == 1
    assert fills[0].price == D("2480")  # 指値より有利な寄付で約定
    assert broker.get_positions()[0].quantity == D(100)


def test_paper_limit_buy_does_not_fill_above_the_limit() -> None:
    broker = PaperBroker(initial_cash=D(1_000_000))
    broker.place(buy(price="2500"))

    assert broker.settle({"7203": D("2520")}) == []
    assert len(broker.get_open_orders()) == 1


def test_paper_limit_sell_does_not_fill_below_the_limit() -> None:
    broker = PaperBroker(initial_cash=D(1_000_000))
    broker.place(buy(price="2500"))
    broker.settle({"7203": D("2500")})

    broker.place(sell(price="2600"))
    assert broker.settle({"7203": D("2550")}) == []


def test_paper_market_order_slips_against_you() -> None:
    """成行は不利な方向に滑る。楽観的なバックテストを防ぐ。"""
    broker = PaperBroker(initial_cash=D(1_000_000), slippage_rate=D("0.01"))
    broker.mark({"7203": D(2500)})
    broker.place(buy(price=None))

    fills = broker.settle({"7203": D(2500)})

    assert fills[0].price == D(2525)  # 1% 不利


def test_paper_charges_commission() -> None:
    broker = PaperBroker(initial_cash=D(1_000_000), commission_rate=D("0.0011"))
    broker.place(buy(price="2500"))

    fills = broker.settle({"7203": D("2500")})

    assert fills[0].fee == D(275)  # 250,000 × 0.11%（UAT の実測値と一致）
    assert broker.get_balance().cash_balance == D(1_000_000) - D(250_000) - D(275)


def test_paper_rejects_buying_beyond_cash() -> None:
    broker = PaperBroker(initial_cash=D(10_000))
    with pytest.raises(InsufficientFundsError, match="買付余力"):
        broker.place(buy(price="2500"))


def test_paper_rejects_short_selling() -> None:
    """現物口座なので空売りはできない。"""
    broker = PaperBroker(initial_cash=D(1_000_000))
    with pytest.raises(OrderRejectedError, match="保有"):
        broker.place(sell())


def test_paper_tracks_average_cost_across_buys() -> None:
    broker = PaperBroker(initial_cash=D(10_000_000), commission_rate=D(0))
    broker.place(buy(price="2000", oid="a"))
    broker.settle({"7203": D(2000)})
    broker.place(buy(price="3000", oid="b"))
    broker.settle({"7203": D(3000)})

    position = broker.get_positions()[0]
    assert position.quantity == D(200)
    assert position.cost_price == D(2500)


def test_paper_realizes_profit_on_sale() -> None:
    broker = PaperBroker(initial_cash=D(10_000_000), commission_rate=D(0))
    broker.place(buy(price="2000", oid="a"))
    broker.settle({"7203": D(2000)})
    broker.place(sell(price="2000", oid="b"))
    broker.settle({"7203": D(2500)})

    assert broker.realized_pnl == D(50_000)  # (2500-2000) × 100
    assert broker.get_positions() == []


def test_paper_ignores_duplicate_client_order_id() -> None:
    """同じ注文IDの再送で二重発注しない（冪等性）。"""
    broker = PaperBroker(initial_cash=D(10_000_000))
    broker.place(buy(oid="same"))
    broker.place(buy(oid="same"))

    assert len(broker.get_open_orders()) == 1


def test_paper_unfilled_day_orders_expire() -> None:
    broker = PaperBroker(initial_cash=D(10_000_000))
    broker.place(buy(price="2000"))

    broker.expire_open_orders()

    assert broker.get_open_orders() == []
    assert broker.get_order("o1").status is OrderStatus.EXPIRED  # type: ignore[union-attr]


def test_begin_day_does_not_expire_pending_orders() -> None:
    """前日の終値で判断して出した注文は、当日の寄付で約定する機会が要る。

    ここで失効させると、注文が一度も約定しないまま消える。
    """
    broker = PaperBroker(initial_cash=D(10_000_000))
    broker.place(buy(price="2500"))

    broker.begin_day()

    assert len(broker.get_open_orders()) == 1
    assert len(broker.settle({"7203": D(2450)})) == 1


def test_paper_tracks_symbols_bought_today() -> None:
    """差金決済の判定材料。"""
    broker = PaperBroker(initial_cash=D(10_000_000))
    broker.place(buy())
    broker.settle({"7203": D(2500)})

    assert broker.bought_today == {"7203"}

    broker.begin_day()
    assert broker.bought_today == set()


def test_paper_cancel_removes_from_open_orders() -> None:
    broker = PaperBroker(initial_cash=D(10_000_000))
    broker.place(buy())
    broker.cancel("o1")

    assert broker.get_open_orders() == []
    assert broker.settle({"7203": D(2000)}) == []


def test_paper_cancel_unknown_order_raises() -> None:
    with pytest.raises(OrderRejectedError, match="見つかりません"):
        PaperBroker().cancel("nope")


def test_paper_market_preview_needs_a_mark_price() -> None:
    broker = PaperBroker(initial_cash=D(10_000_000))
    with pytest.raises(OrderRejectedError, match="時価がありません"):
        broker.preview(buy(price=None))


def test_paper_equity_reflects_marks() -> None:
    broker = PaperBroker(initial_cash=D(1_000_000), commission_rate=D(0))
    broker.place(buy(price="2500"))
    broker.settle({"7203": D(2500)})

    broker.mark({"7203": D(3000)})

    assert broker.equity == D(750_000) + D(300_000)


# --------------------------------------------------------------------------
# レート制限
# --------------------------------------------------------------------------


def test_rate_limiter_allows_the_burst() -> None:
    slept: list[float] = []
    limiter = RateLimiter(Limit(2, 2.0), sleep=slept.append)

    limiter.acquire()
    limiter.acquire()

    assert slept == []


def test_rate_limiter_waits_when_exhausted() -> None:
    """上限に当たったら例外ではなく待つ。"""
    slept: list[float] = []
    limiter = RateLimiter(Limit(2, 2.0), sleep=slept.append)

    for _ in range(3):
        limiter.acquire()

    assert slept, "3回目は待つはず"
    assert sum(slept) > 0


def test_rate_limiter_rejects_impossible_request() -> None:
    limiter = RateLimiter(Limit(2, 2.0))
    with pytest.raises(ValueError, match="上限"):
        limiter.acquire(5)


def test_limit_validates_arguments() -> None:
    with pytest.raises(ValueError, match="不正なレート制限"):
        Limit(0, 1.0)


def test_cached_reuses_within_ttl() -> None:
    calls = []
    now = [0.0]

    cache = Cached(lambda: calls.append(1) or len(calls), ttl=2.0, clock=lambda: now[0])

    assert cache.get() == 1
    assert cache.get() == 1  # TTL 内は再取得しない
    assert len(calls) == 1


def test_cached_refetches_after_ttl() -> None:
    calls = []
    now = [0.0]
    cache = Cached(lambda: calls.append(1) or len(calls), ttl=2.0, clock=lambda: now[0])

    cache.get()
    now[0] = 3.0
    cache.get()

    assert len(calls) == 2


def test_cached_invalidate_forces_refetch() -> None:
    """発注後に残高が変わるので、明示的に捨てられる必要がある。"""
    calls = []
    cache = Cached(lambda: calls.append(1) or len(calls), ttl=999.0)

    cache.get()
    cache.invalidate()
    cache.get()

    assert len(calls) == 2
