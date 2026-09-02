"""米国株対応のテスト。

重点:
    - 市場ルールの差し替えで、エンジン本体を触らずに米国株が動くこと
    - 逆指値は「これから約定する注文」ではないこと（数えると買い直す）
    - ブローカー逆指値の置き直しが冪等で、手仕舞いと二重にならないこと
"""

from __future__ import annotations

import datetime as dt
import math
from decimal import Decimal

import polars as pl
import pytest

from wbcore.broker.paper import PaperBroker
from wbcore.data.yfinance_provider import to_yahoo_ticker
from wbcore.domain.jp_rules import PriceRounding
from wbcore.domain.market_rules import JpMarketRules, UsMarketRules, rules_for
from wbcore.domain.models import (
    Market,
    Order,
    OrderRequest,
    OrderStatus,
    OrderType,
    Position,
    Side,
    TargetPosition,
    TimeInForce,
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
from wbjp.engine.backtest import BacktestRunner
from wbjp.engine.reconcile import effective_quantity, reconcile
from wbjp.risk.limits import RiskContext, RiskManager
from wbjp.risk.stops import Stop, sync_broker_stops
from wbjp.strategy.registry import build_all

D = Decimal
TODAY = dt.date(2026, 8, 25)
US = UsMarketRules()


def position(symbol: str = "AAPL", qty: int = 10, cost: str = "200", last: str = "200") -> Position:
    return Position(
        symbol=symbol,
        quantity=D(qty),
        available_quantity=D(qty),
        cost_price=D(cost),
        last_price=D(last),
        currency="USD",
    )


def stop_order(
    symbol: str = "AAPL", qty: int = 10, stop: str = "190", oid: str = "stop-1"
) -> Order:
    return Order(
        client_order_id=oid,
        broker_order_id=oid,
        symbol=symbol,
        side=Side.SELL,
        order_type=OrderType.STOP_LOSS,
        quantity=D(qty),
        filled_quantity=D(0),
        status=OrderStatus.SUBMITTED,
        stop_price=D(stop),
        time_in_force=TimeInForce.GTC,
    )


def stop_request(symbol: str = "AAPL", qty: int = 10, stop: str = "190") -> OrderRequest:
    return OrderRequest(
        client_order_id="s",
        symbol=symbol,
        side=Side.SELL,
        order_type=OrderType.STOP_LOSS,
        quantity=D(qty),
        stop_price=D(stop),
        time_in_force=TimeInForce.GTC,
    )


# ==========================================================================
# 市場ルール
# ==========================================================================


def test_rules_for_returns_market_specific_rules() -> None:
    assert isinstance(rules_for(Market.JP), JpMarketRules)
    assert isinstance(rules_for(Market.US), UsMarketRules)
    assert rules_for(Market.US).currency == "USD"
    assert rules_for(Market.JP).currency == "JPY"


def test_us_lot_size_is_one_share() -> None:
    assert US.default_lot_size == D(1)
    assert US.round_to_lot(D("57")) == D("57")
    assert US.round_to_lot(D("57.9")) == D("57")


@pytest.mark.parametrize(
    ("raw", "side", "rounding", "expected"),
    [
        ("200.123", Side.BUY, PriceRounding.CONSERVATIVE, "200.12"),
        ("200.123", Side.SELL, PriceRounding.CONSERVATIVE, "200.13"),
        ("200.123", Side.BUY, PriceRounding.AGGRESSIVE, "200.13"),
        ("200.125", Side.BUY, PriceRounding.NEAREST, "200.13"),
        ("0.12345", Side.BUY, PriceRounding.CONSERVATIVE, "0.1234"),
        ("1000", Side.BUY, PriceRounding.CONSERVATIVE, "1000"),
    ],
)
def test_us_tick_is_one_cent(raw: str, side: Side, rounding: PriceRounding, expected: str) -> None:
    assert US.snap_to_tick(D(raw), side, rounding=rounding) == D(expected)


def test_us_snap_never_returns_scientific_notation() -> None:
    assert str(US.snap_to_tick(D("1000.00"), Side.BUY)) == "1000"


def test_us_has_no_daily_price_limit() -> None:
    assert US.is_within_price_limit(D("1"), D("1000"))
    assert US.is_within_price_limit(D("100000"), D("1"))


def test_us_supports_broker_stops_and_jp_does_not() -> None:
    assert US.supports_broker_stops
    assert US.broker_stop_order_type is OrderType.STOP_LOSS
    assert not JpMarketRules().supports_broker_stops


def test_us_does_not_block_same_day_sale() -> None:
    assert US.blocks_same_day_sale is False
    assert JpMarketRules().blocks_same_day_sale is True


# ==========================================================================
# 注文モデル
# ==========================================================================


def test_stop_order_requires_a_stop_price() -> None:
    with pytest.raises(ValueError, match="stop_price が必要"):
        OrderRequest("o", "AAPL", Side.SELL, OrderType.STOP_LOSS, D(10))


def test_limit_order_rejects_a_stop_price() -> None:
    with pytest.raises(ValueError, match="stop_price は指定できない"):
        OrderRequest("o", "AAPL", Side.SELL, OrderType.LIMIT, D(10), D(1), stop_price=D(1))


def test_stop_limit_needs_both_prices() -> None:
    with pytest.raises(ValueError, match="limit_price が必要"):
        OrderRequest("o", "AAPL", Side.SELL, OrderType.STOP_LOSS_LIMIT, D(10), stop_price=D(1))
    ok = OrderRequest(
        "o", "AAPL", Side.SELL, OrderType.STOP_LOSS_LIMIT, D(10), D("189"), stop_price=D("190")
    )
    assert ok.order_type.is_stop


def test_stop_types_are_placeable_but_other_is_not() -> None:
    assert OrderType.STOP_LOSS.is_placeable
    assert OrderType.STOP_LOSS_LIMIT.is_placeable
    assert not OrderType.OTHER.is_placeable
    assert not OrderType.OTHER.is_stop


# ==========================================================================
# 実効ポジション — 逆指値を数えると買い直してしまう
# ==========================================================================


def test_effective_quantity_ignores_resting_stop_orders() -> None:
    """逆指値は保険であって「もうすぐ減る株数」ではない。

    数えてしまうと 10 − 10 = 0 と見え、目標 10 株に対して
    もう 10 株買ってしまう。
    """
    positions = {"AAPL": position(qty=10)}
    assert effective_quantity("AAPL", positions, [stop_order(qty=10)]) == D(10)


def test_reconcile_does_not_rebuy_when_a_stop_is_resting() -> None:
    plan = reconcile(
        [TargetPosition("AAPL", D(10))],
        {"AAPL": position(qty=10)},
        [stop_order(qty=10)],
        {"AAPL": D(200)},
        order_id_seed="20260825",
        rules=US,
    )
    assert plan.orders == []


# ==========================================================================
# リコンサイル（米国ルール）
# ==========================================================================


def test_reconcile_us_trades_single_shares_and_cent_ticks() -> None:
    plan = reconcile(
        [TargetPosition("AAPL", D(7))],
        {},
        [],
        {"AAPL": D("200.10")},
        order_id_seed="20260825",
        rules=US,
    )
    assert len(plan.orders) == 1
    order = plan.orders[0]
    assert order.quantity == D(7)
    # 0.5% 上に置き、1セント刻みで約定しやすい方向へ丸める
    assert order.limit_price == D("201.11")
    assert order.limit_price % D("0.01") == 0


def test_reconcile_us_allows_selling_what_was_bought_today() -> None:
    plan = reconcile(
        [TargetPosition("AAPL", D(0))],
        {"AAPL": position(qty=10)},
        [],
        {"AAPL": D(200)},
        order_id_seed="20260825",
        rules=US,
        bought_today={"AAPL"},
    )
    assert [o.side for o in plan.orders] == [Side.SELL]


def test_reconcile_jp_still_blocks_same_day_sale() -> None:
    plan = reconcile(
        [TargetPosition("7203", D(0))],
        {"7203": Position("7203", D(100), D(100), D(2500), D(2500))},
        [],
        {"7203": D(2500)},
        order_id_seed="20260825",
        bought_today={"7203"},
    )
    assert plan.orders == []
    assert "差金決済" in plan.skipped["7203"]


# ==========================================================================
# リスク（米国ルール）
# ==========================================================================


def test_risk_messages_use_account_currency() -> None:
    manager = RiskManager(RiskConfig(max_order_value=D(100)), ["AAPL"], US)
    request = OrderRequest("o", "AAPL", Side.BUY, OrderType.LIMIT, D(10), D(200))
    ctx = RiskContext(
        equity=D(100_000),
        balance=__import__("wbcore.domain.models", fromlist=["Balance"]).Balance(
            "USD", D(100_000), D(100_000)
        ),
        base_prices={"AAPL": D(200)},
    )
    decision = manager.check(request, ctx)
    assert not decision.approved
    assert "USD" in decision.reason


def test_order_value_cap_does_not_block_exits_or_stops() -> None:
    """損切りが金額上限で弾かれると、上限が損失を膨らませる装置になる。"""
    from wbcore.domain.models import Balance

    manager = RiskManager(RiskConfig(max_order_value=D(100)), ["AAPL"], US)
    ctx = RiskContext(
        equity=D(100_000),
        balance=Balance("USD", D(100_000), D(100_000)),
        positions={"AAPL": position(qty=50)},
        base_prices={"AAPL": D(200)},
    )
    exit_ = OrderRequest("o", "AAPL", Side.SELL, OrderType.MARKET, D(50))
    assert manager.check(exit_, ctx).approved
    assert manager.check(stop_request(qty=50), ctx).approved
    buy = OrderRequest("o", "AAPL", Side.BUY, OrderType.MARKET, D(50))
    assert not manager.check(buy, ctx).approved


def test_risk_skips_price_limit_check_for_us() -> None:
    """東証なら値幅制限で弾かれる指値も、米国株では通る。"""
    from wbcore.domain.models import Balance

    manager = RiskManager(RiskConfig(max_order_value=D(10_000_000)), ["AAPL"], US)
    request = OrderRequest("o", "AAPL", Side.BUY, OrderType.LIMIT, D(1), D(10_000))
    ctx = RiskContext(
        equity=D(1_000_000),
        balance=Balance("USD", D(1_000_000), D(1_000_000)),
        base_prices={"AAPL": D(100)},
    )
    assert manager.check(request, ctx).approved


# ==========================================================================
# PaperBroker の逆指値
# ==========================================================================


def us_paper(cash: int = 100_000) -> PaperBroker:
    return PaperBroker(initial_cash=D(cash), currency="USD", commission_rate=D(0))


def buy_and_fill(broker: PaperBroker, qty: int = 10, price: str = "200") -> None:
    broker.place(OrderRequest("b", "AAPL", Side.BUY, OrderType.LIMIT, D(qty), D(price)))
    broker.settle({"AAPL": D(price)})


def test_paper_stop_survives_end_of_day_because_it_is_gtc() -> None:
    broker = us_paper()
    buy_and_fill(broker)
    broker.place(stop_request())

    broker.expire_open_orders()

    assert [o.order_type for o in broker.get_open_orders()] == [OrderType.STOP_LOSS]


def test_paper_stop_does_not_trigger_above_the_stop() -> None:
    broker = us_paper()
    buy_and_fill(broker)
    broker.place(stop_request(stop="190"))

    assert broker.settle({"AAPL": D(195)}, low_prices={"AAPL": D(191)}) == []


def test_paper_stop_triggers_intraday_at_the_stop_price() -> None:
    """寄付は上だが安値が届いた → トリガー価格で成行になったとみなす。"""
    broker = PaperBroker(initial_cash=D(100_000), currency="USD", slippage_rate=D(0))
    buy_and_fill(broker)
    broker.place(stop_request(stop="190"))

    fills = broker.settle({"AAPL": D(195)}, low_prices={"AAPL": D(188)})

    assert len(fills) == 1
    assert fills[0].price == D(190)
    assert broker.get_positions() == []


def test_paper_stop_gap_down_fills_at_the_open() -> None:
    """ギャップダウンはストップ価格では売れない。寄付で食らう。"""
    broker = PaperBroker(initial_cash=D(100_000), currency="USD", slippage_rate=D(0))
    buy_and_fill(broker)
    broker.place(stop_request(stop="190"))

    fills = broker.settle({"AAPL": D(180)}, low_prices={"AAPL": D(178)})

    assert fills[0].price == D(180)


def test_paper_stop_without_lows_falls_back_to_open_only() -> None:
    """安値を渡さなければ、エンジン合成と同じ「寄付だけ」の判定になる。"""
    broker = PaperBroker(initial_cash=D(100_000), currency="USD", slippage_rate=D(0))
    buy_and_fill(broker)
    broker.place(stop_request(stop="190"))

    assert broker.settle({"AAPL": D(195)}) == []
    assert broker.settle({"AAPL": D(185)})[0].price == D(185)


def test_paper_usd_fees_are_rounded_to_cents() -> None:
    broker = PaperBroker(initial_cash=D(100_000), currency="USD", commission_rate=D("0.0001"))
    buy_and_fill(broker, qty=3, price="199.99")
    assert broker.fills[0].fee == D("0.06")  # 599.97 × 0.01% = 0.059997


# ==========================================================================
# ブローカー逆指値の同期
# ==========================================================================


def a_stop(price: str = "190") -> Stop:
    return Stop("AAPL", D(price), D(200), TODAY)


def test_sync_places_a_stop_for_an_unprotected_position() -> None:
    plan = sync_broker_stops(
        {"AAPL": a_stop()},
        {"AAPL": position(qty=10)},
        [],
        order_type=OrderType.STOP_LOSS,
        order_id_seed="20260825",
    )
    assert len(plan.place) == 1
    order = plan.place[0]
    assert order.order_type is OrderType.STOP_LOSS
    assert order.side is Side.SELL
    assert order.stop_price == D(190)
    assert order.quantity == D(10)
    assert order.time_in_force is TimeInForce.GTC
    assert plan.cancel == []


def test_sync_is_idempotent_when_the_right_stop_is_already_resting() -> None:
    plan = sync_broker_stops(
        {"AAPL": a_stop("190")},
        {"AAPL": position(qty=10)},
        [stop_order(qty=10, stop="190")],
        order_type=OrderType.STOP_LOSS,
        order_id_seed="20260825",
    )
    assert not plan


def test_sync_replaces_a_stop_when_trailing_raised_it() -> None:
    plan = sync_broker_stops(
        {"AAPL": a_stop("195")},
        {"AAPL": position(qty=10)},
        [stop_order(qty=10, stop="190")],
        order_type=OrderType.STOP_LOSS,
        order_id_seed="20260825",
    )
    assert [o.stop_price for o in plan.cancel] == [D(190)]
    assert [o.stop_price for o in plan.place] == [D(195)]


def test_sync_cancels_a_stop_whose_position_is_gone() -> None:
    plan = sync_broker_stops(
        {},
        {},
        [stop_order()],
        order_type=OrderType.STOP_LOSS,
        order_id_seed="20260825",
    )
    assert len(plan.cancel) == 1
    assert plan.place == []


def test_sync_cancels_but_does_not_replace_when_an_exit_is_pending() -> None:
    """戦略の手仕舞いが決まった銘柄に逆指値を残すと、両方約定して空売りになる。"""
    plan = sync_broker_stops(
        {"AAPL": a_stop()},
        {"AAPL": position(qty=10)},
        [stop_order()],
        order_type=OrderType.STOP_LOSS,
        order_id_seed="20260825",
        pending_exits={"AAPL"},
    )
    assert len(plan.cancel) == 1
    assert plan.place == []


def test_sync_removes_duplicate_stops() -> None:
    plan = sync_broker_stops(
        {"AAPL": a_stop("190")},
        {"AAPL": position(qty=10)},
        [stop_order(oid="a"), stop_order(oid="b")],
        order_type=OrderType.STOP_LOSS,
        order_id_seed="20260825",
    )
    assert len(plan.cancel) == 1
    assert plan.place == []


def test_sync_order_id_changes_with_the_stop_price() -> None:
    first = sync_broker_stops(
        {"AAPL": a_stop("190")},
        {"AAPL": position()},
        [],
        order_type=OrderType.STOP_LOSS,
        order_id_seed="d",
    ).place[0]
    second = sync_broker_stops(
        {"AAPL": a_stop("195")},
        {"AAPL": position()},
        [],
        order_type=OrderType.STOP_LOSS,
        order_id_seed="d",
    ).place[0]
    assert first.client_order_id != second.client_order_id
    assert len(first.client_order_id) <= 32


# ==========================================================================
# 設定
# ==========================================================================


def us_config(**overrides) -> FileConfig:  # type: ignore[no-untyped-def]
    base = {
        "universe": UniverseConfig(market=Market.US, symbols=["AAPL"]),
        "risk": RiskConfig(max_order_value=D(1_000_000), max_position_weight=D(1)),
        "sizing": SizingConfig(method="equal_weight", max_positions=1),
        "execution": ExecutionConfig(order_type="market"),
        "strategies": StrategiesConfig(
            strategies=[StrategyEntry(name="sma_cross", fast=3, slow=8)]
        ),
    }
    return FileConfig(**{**base, **overrides})


def test_us_config_uses_broker_stops_by_default() -> None:
    assert us_config().uses_broker_stops is True
    assert us_config(execution=ExecutionConfig(stop_mode="engine")).uses_broker_stops is False


def test_jp_config_never_uses_broker_stops_in_auto() -> None:
    jp = FileConfig(universe=UniverseConfig(symbols=["7203"]))
    assert jp.uses_broker_stops is False


def test_broker_stop_mode_is_rejected_for_jp() -> None:
    with pytest.raises(ValueError, match="stop_mode"):
        FileConfig(
            universe=UniverseConfig(symbols=["7203"]),
            execution=ExecutionConfig(stop_mode="broker"),
        )


def test_topix500_is_rejected_for_us() -> None:
    with pytest.raises(ValueError, match="topix500"):
        UniverseConfig(market=Market.US, symbols=["AAPL"], topix500_symbols=["AAPL"])


def test_legacy_jpy_field_names_still_load() -> None:
    config = FileConfig.model_validate(
        {
            "risk": {"max_order_value_jpy": "1", "max_daily_loss_jpy": "2"},
            "sizing": {"fixed_notional_jpy": "3"},
        }
    )
    assert config.risk.max_order_value == D(1)
    assert config.risk.max_daily_loss == D(2)
    assert config.sizing.fixed_notional == D(3)


# ==========================================================================
# データ取得
# ==========================================================================


def test_yahoo_ticker_mapping_per_market() -> None:
    assert to_yahoo_ticker("7203") == "7203.T"
    assert to_yahoo_ticker("AAPL", Market.US) == "AAPL"
    assert to_yahoo_ticker("BRK.B", Market.US) == "BRK-B"
    assert to_yahoo_ticker("^GSPC", Market.US) == "^GSPC"


# ==========================================================================
# バックテスト（米国ルールで一気通貫）
# ==========================================================================


def make_bars(closes: list[float], start: dt.date = dt.date(2024, 1, 1)) -> pl.DataFrame:
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
            "low": [min(o, c) * 0.97 for o, c in zip(opens, closes, strict=True)],
            "close": closes,
            "volume": [1_000_000.0] * len(closes),
        }
    )


def wavy(n: int) -> list[float]:
    out, price = [], 200.0
    for i in range(n):
        price *= 1 + 0.02 * math.sin(i / 9) + 0.004 * math.cos(i / 2.3)
        out.append(round(price, 2))
    return out


def test_us_backtest_places_broker_stops_and_trades_single_shares() -> None:
    config = us_config()
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(30_000))
    result = runner.run({"AAPL": make_bars(wavy(120))}, start=dt.date(2024, 2, 1))

    assert runner.broker.currency == "USD"
    assert result.fills, "売買が起きるはず"
    # 1株単位で建てる（100株の倍数に縛られない）
    assert any(f.quantity % 100 != 0 for f in result.fills)
    # 逆指値がブローカーに置かれた記録がある
    stop_orders = [
        oid
        for r in result.records
        for oid in r.orders
        if runner.broker.get_order(oid) and runner.broker.get_order(oid).order_type.is_stop  # type: ignore[union-attr]
    ]
    assert stop_orders, "逆指値が1件も置かれていない"


def test_us_backtest_with_engine_stops_places_no_stop_orders() -> None:
    config = us_config(execution=ExecutionConfig(order_type="market", stop_mode="engine"))
    runner = BacktestRunner(build_all(config.strategies.enabled), config, initial_cash=D(30_000))
    result = runner.run({"AAPL": make_bars(wavy(120))}, start=dt.date(2024, 2, 1))

    for record in result.records:
        for oid in record.orders:
            order = runner.broker.get_order(oid)
            assert order is not None and not order.order_type.is_stop
