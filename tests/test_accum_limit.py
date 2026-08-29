"""積立の指値と、成行が気配値無しで拒否されたときの指値への切替。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

from accum.config import AccumConfig
from accum.execute import Contribution, should_fallback_to_limit, to_order
from accum.tactics import Constant
from wbcore.broker.base import BrokerError
from wbcore.domain.market_rules import rules_for
from wbcore.domain.models import Market, OrderType, Side, TaxAccountType


def _contribution() -> Contribution:
    return Contribution(
        symbol="452A.T",
        market=Market.JP,
        date=dt.date(2026, 8, 29),
        close=Decimal("827.1"),
        amount=Decimal(25_000),
        multiplier=1.0,
        reason="今月の目標",
        tactic=Constant(window=False),
    )


def test_limit_order_sits_above_the_price_on_a_valid_tick() -> None:
    order = to_order(
        _contribution(),
        tax_type=TaxAccountType.NISA,
        lot_size=1,
        order_type=OrderType.LIMIT,
        limit_offset=Decimal("0.01"),
    )
    assert order.order_type is OrderType.LIMIT and order.limit_price is not None
    assert order.limit_price >= Decimal("827.1") * Decimal("1.01")
    rules = rules_for(Market.JP)
    assert rules.snap_to_tick(order.limit_price, Side.BUY, symbol="452A") == order.limit_price
    assert order.quantity == Decimal(30)  # 株数は価格ではなく最新値で決める


def test_market_and_limit_retry_get_different_order_ids() -> None:
    market = to_order(_contribution(), tax_type=TaxAccountType.SPECIFIC, lot_size=1)
    retry = to_order(
        _contribution(),
        tax_type=TaxAccountType.SPECIFIC,
        lot_size=1,
        order_type=OrderType.LIMIT,
        seed="accum-limit",
    )
    assert market.client_order_id != retry.client_order_id


def test_fallback_only_for_market_orders_rejected_for_missing_quote() -> None:
    market = to_order(_contribution(), tax_type=TaxAccountType.SPECIFIC, lot_size=1)
    limit = to_order(
        _contribution(), tax_type=TaxAccountType.SPECIFIC, lot_size=1, order_type=OrderType.LIMIT
    )
    quote_missing = BrokerError("発注に失敗しました（OPENAPI_QUOTE_NOT_FOUND）")
    assert should_fallback_to_limit(market, quote_missing)
    assert not should_fallback_to_limit(limit, quote_missing)
    assert not should_fallback_to_limit(market, BrokerError("買付余力が足りません"))


def test_execution_config_validates_order_type_and_offset() -> None:
    config = AccumConfig.model_validate(
        {"execution": {"order_type": "limit", "limit_offset": "0.02"}, "tactics": []}
    )
    assert config.execution.order_type == "limit"
    assert config.execution.limit_offset == Decimal("0.02")
    assert config.execution.fallback_to_limit
    for bad in ({"order_type": "stop"}, {"limit_offset": "0.5"}):
        try:
            AccumConfig.model_validate({"execution": bad, "tactics": []})
        except ValueError:
            continue
        raise AssertionError(f"弾かれるべき設定が通った: {bad}")
