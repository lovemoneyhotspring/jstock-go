"""サイジング・リコンサイル・リスクのテスト。

重点は **冪等性**。同じ状態から何度実行しても、二重に建たないこと。
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal

from wbjp.config import RiskConfig, SizingConfig
from wbjp.domain.models import (
    Balance,
    CombinedSignal,
    Order,
    OrderPreview,
    OrderRequest,
    OrderStatus,
    OrderType,
    Position,
    Side,
    TargetPosition,
)
from wbjp.engine.reconcile import (
    ReconcileSettings,
    effective_quantity,
    make_client_order_id,
    reconcile,
)
from wbjp.portfolio.sizer import (
    AtrRiskSizer,
    EqualWeightSizer,
    FixedNotionalSizer,
    SizingContext,
    build_sizer,
)
from wbjp.risk.limits import RiskContext, RiskManager
from wbjp.risk.stops import Stop, StopBook, apply_stop_priority

D = Decimal
TODAY = dt.date(2026, 8, 25)


def position(symbol: str = "7203", qty: int = 100, cost: str = "2500", last: str = "2500"):  # type: ignore[no-untyped-def]
    return Position(
        symbol=symbol,
        quantity=D(qty),
        available_quantity=D(qty),
        cost_price=D(cost),
        last_price=D(last),
    )


def open_order(symbol: str = "7203", qty: int = 100, side: Side = Side.BUY, filled: int = 0):  # type: ignore[no-untyped-def]
    return Order(
        client_order_id=f"{symbol}-{side.value}",
        broker_order_id="b1",
        symbol=symbol,
        side=side,
        order_type=OrderType.LIMIT,
        quantity=D(qty),
        filled_quantity=D(filled),
        status=OrderStatus.SUBMITTED,
        limit_price=D("2500"),
    )


def signal(symbol: str = "7203", direction: float = 1.0) -> CombinedSignal:
    return CombinedSignal(symbol, direction, {"s": direction}, "test")


# ==========================================================================
# 実効ポジション — 二重発注防止の土台
# ==========================================================================


def test_effective_quantity_without_orders() -> None:
    assert effective_quantity("7203", {"7203": position()}, []) == D(100)


def test_effective_quantity_includes_pending_buys() -> None:
    """未約定の買いは「もうすぐ増える株数」として数える。

    これを忘れると、板に残っている注文をもう一度出す。
    """
    result = effective_quantity("7203", {}, [open_order(qty=100)])
    assert result == D(100)


def test_effective_quantity_subtracts_pending_sells() -> None:
    result = effective_quantity(
        "7203", {"7203": position(qty=200)}, [open_order(qty=100, side=Side.SELL)]
    )
    assert result == D(100)


def test_effective_quantity_uses_the_unfilled_remainder() -> None:
    """部分約定していたら、残っている分だけを数える。"""
    result = effective_quantity("7203", {}, [open_order(qty=100, filled=30)])
    assert result == D(70)


def test_effective_quantity_ignores_terminal_orders() -> None:
    filled = Order(
        client_order_id="x",
        broker_order_id="b",
        symbol="7203",
        side=Side.BUY,
        order_type=OrderType.LIMIT,
        quantity=D(100),
        filled_quantity=D(100),
        status=OrderStatus.FILLED,
    )
    assert effective_quantity("7203", {}, [filled]) == D(0)


def test_effective_quantity_ignores_other_symbols() -> None:
    assert effective_quantity("7203", {}, [open_order(symbol="6758")]) == D(0)


# ==========================================================================
# リコンサイル — 冪等性
# ==========================================================================


def test_reconcile_creates_a_buy_for_a_new_target() -> None:
    plan = reconcile(
        [TargetPosition("7203", D(100))],
        positions={},
        open_orders=[],
        prices={"7203": D(2500)},
        order_id_seed="r1",
    )

    assert len(plan.orders) == 1
    order = plan.orders[0]
    assert (order.side, order.quantity) == (Side.BUY, D(100))


def test_reconcile_emits_nothing_when_already_at_target() -> None:
    """目標と現状が一致していれば何も出さない。"""
    plan = reconcile(
        [TargetPosition("7203", D(100))],
        positions={"7203": position(qty=100)},
        open_orders=[],
        prices={"7203": D(2500)},
        order_id_seed="r1",
    )
    assert plan.orders == []


def test_reconcile_does_not_reorder_what_is_already_pending() -> None:
    """**最重要**: 未約定注文があれば、同じ注文を重ねて出さない。

    プロセスが落ちて再実行されたとき、これが無いと二重に建つ。
    """
    plan = reconcile(
        [TargetPosition("7203", D(100))],
        positions={},
        open_orders=[open_order(qty=100)],
        prices={"7203": D(2500)},
        order_id_seed="r1",
    )
    assert plan.orders == []


def test_reconcile_is_idempotent_across_repeated_runs() -> None:
    """同じ状態から何度実行しても、同じ注文が1回ぶん出るだけ。"""
    targets = [TargetPosition("7203", D(100))]
    prices = {"7203": D(2500)}

    first = reconcile(targets, {}, [], prices, order_id_seed="r1")
    # 1回目の注文が板に残っている状態で再実行
    pending = [
        Order(
            client_order_id=first.orders[0].client_order_id,
            broker_order_id="b",
            symbol="7203",
            side=Side.BUY,
            order_type=OrderType.LIMIT,
            quantity=D(100),
            filled_quantity=D(0),
            status=OrderStatus.SUBMITTED,
        )
    ]
    second = reconcile(targets, {}, pending, prices, order_id_seed="r1")

    assert len(first.orders) == 1
    assert second.orders == []


def test_same_trading_day_produces_the_same_order_id() -> None:
    """同じ取引日の同じ判断からは、必ず同じ注文IDが出る。

    cron が二重に走ったり、失敗後に手で再実行したりしたときに、
    journal の冪等チェック（``was_placed``）が効く条件。IDが実行ごとに
    変わると、そのチェックをすり抜けて二重発注になる。
    """
    targets = [TargetPosition("7203", D(100))]
    prices = {"7203": D(2500)}

    first = reconcile(targets, {}, [], prices, order_id_seed="20260825")
    second = reconcile(targets, {}, [], prices, order_id_seed="20260825")

    assert first.orders[0].client_order_id == second.orders[0].client_order_id


def test_different_trading_days_produce_different_order_ids() -> None:
    """日が変われば別の注文になる（前日と同じIDで弾かれない）。"""
    targets = [TargetPosition("7203", D(100))]
    prices = {"7203": D(2500)}

    monday = reconcile(targets, {}, [], prices, order_id_seed="20260824")
    tuesday = reconcile(targets, {}, [], prices, order_id_seed="20260825")

    assert monday.orders[0].client_order_id != tuesday.orders[0].client_order_id


def test_reconcile_orders_only_the_difference() -> None:
    """目標300株・保有100株なら、200株だけ買う。"""
    plan = reconcile(
        [TargetPosition("7203", D(300))],
        positions={"7203": position(qty=100)},
        open_orders=[],
        prices={"7203": D(2500)},
        order_id_seed="r1",
    )
    assert plan.orders[0].quantity == D(200)


def test_reconcile_sells_down_to_target() -> None:
    plan = reconcile(
        [TargetPosition("7203", D(100))],
        positions={"7203": position(qty=300)},
        open_orders=[],
        prices={"7203": D(2500)},
        order_id_seed="r1",
    )
    assert (plan.orders[0].side, plan.orders[0].quantity) == (Side.SELL, D(200))


def test_reconcile_exits_completely_on_zero_target() -> None:
    plan = reconcile(
        [TargetPosition("7203", D(0))],
        positions={"7203": position(qty=200)},
        open_orders=[],
        prices={"7203": D(2500)},
        order_id_seed="r1",
    )
    assert (plan.orders[0].side, plan.orders[0].quantity) == (Side.SELL, D(200))


def test_reconcile_skips_sub_lot_differences() -> None:
    """単元株に満たない差分は発注できないので見送り、理由を残す。"""
    plan = reconcile(
        [TargetPosition("7203", D(150))],
        positions={"7203": position(qty=100)},
        open_orders=[],
        prices={"7203": D(2500)},
        order_id_seed="r1",
    )
    assert plan.orders == []
    assert "売買単位" in plan.skipped["7203"]


def test_reconcile_never_sells_more_than_held() -> None:
    """空売りは不可。売却可能数量で頭打ちにする。"""
    plan = reconcile(
        [TargetPosition("7203", D(0))],
        positions={"7203": position(qty=100)},
        open_orders=[open_order(qty=100, side=Side.SELL)],
        prices={"7203": D(2500)},
        order_id_seed="r1",
    )
    # 実効ポジションは 0 なので追加の売りは出ない
    assert plan.orders == []


def test_reconcile_blocks_same_day_settlement_violation() -> None:
    """当日買い付けた銘柄は当日売らない（差金決済の防止）。"""
    plan = reconcile(
        [TargetPosition("7203", D(0))],
        positions={"7203": position(qty=100)},
        open_orders=[],
        prices={"7203": D(2500)},
        order_id_seed="r1",
        bought_today={"7203"},
    )
    assert plan.orders == []
    assert "差金決済" in plan.skipped["7203"]


def test_reconcile_snaps_limit_price_to_the_tick() -> None:
    """呼値に乗っていない指値は取引所に弾かれる。"""
    plan = reconcile(
        [TargetPosition("7203", D(100))],
        positions={},
        open_orders=[],
        prices={"7203": D("3333")},
        order_id_seed="r1",
        settings=ReconcileSettings(order_type="limit", limit_offset=D("0.005")),
    )
    limit = plan.orders[0].limit_price
    assert limit is not None
    assert limit % D(5) == 0  # 3,000〜5,000円は呼値5円


def test_reconcile_uses_finer_tick_for_topix500() -> None:
    plan = reconcile(
        [TargetPosition("7203", D(100))],
        positions={},
        open_orders=[],
        prices={"7203": D("2000")},
        order_id_seed="r1",
        topix500={"7203"},
    )
    limit = plan.orders[0].limit_price
    assert limit is not None
    assert limit % D("0.5") == 0  # TOPIX500 の 1,000〜3,000円は 0.5円刻み


def test_reconcile_limit_price_favours_execution() -> None:
    """買いは上に、売りは下にずらして約定しやすくする。"""
    buy_plan = reconcile(
        [TargetPosition("7203", D(100))],
        {},
        [],
        {"7203": D(2500)},
        order_id_seed="r1",
        settings=ReconcileSettings(limit_offset=D("0.01")),
    )
    assert buy_plan.orders[0].limit_price > D(2500)  # type: ignore[operator]

    sell_plan = reconcile(
        [TargetPosition("7203", D(0))],
        {"7203": position(qty=100)},
        [],
        {"7203": D(2500)},
        order_id_seed="r1",
        settings=ReconcileSettings(limit_offset=D("0.01")),
    )
    assert sell_plan.orders[0].limit_price < D(2500)  # type: ignore[operator]


def test_reconcile_market_orders_carry_no_price() -> None:
    plan = reconcile(
        [TargetPosition("7203", D(100))],
        {},
        [],
        {"7203": D(2500)},
        order_id_seed="r1",
        settings=ReconcileSettings(order_type="market"),
    )
    assert plan.orders[0].order_type is OrderType.MARKET
    assert plan.orders[0].limit_price is None


def test_reconcile_skips_when_price_is_unavailable() -> None:
    plan = reconcile([TargetPosition("9999", D(100))], {}, [], {}, order_id_seed="r1")
    assert plan.orders == []
    assert "価格" in plan.skipped["9999"]


def test_reconcile_carries_the_reason_through() -> None:
    """なぜこの注文が出たかを、注文自体に持たせる。"""
    plan = reconcile(
        [TargetPosition("7203", D(100), reason="ゴールデンクロス")],
        {},
        [],
        {"7203": D(2500)},
        order_id_seed="r1",
    )
    assert plan.orders[0].reason == "ゴールデンクロス"


# --------------------------------------------------------------------------
# 注文IDの決定論性
# --------------------------------------------------------------------------


def test_client_order_id_is_deterministic() -> None:
    """同じ判断からは必ず同じID。ブローカー側でも重複が弾ける。"""
    first = make_client_order_id("run1", "7203", Side.BUY, D(100))
    second = make_client_order_id("run1", "7203", Side.BUY, D(100))
    assert first == second


def test_client_order_id_differs_by_input() -> None:
    base = make_client_order_id("run1", "7203", Side.BUY, D(100))
    assert base != make_client_order_id("run2", "7203", Side.BUY, D(100))
    assert base != make_client_order_id("run1", "6758", Side.BUY, D(100))
    assert base != make_client_order_id("run1", "7203", Side.SELL, D(100))
    assert base != make_client_order_id("run1", "7203", Side.BUY, D(200))


def test_client_order_id_fits_the_api_limit() -> None:
    """Webull の上限は32文字。"""
    assert len(make_client_order_id("very-long-run-id" * 5, "7203", Side.BUY, D(100))) == 32


# ==========================================================================
# サイジング
# ==========================================================================


def sizing_ctx(**kwargs) -> SizingContext:  # type: ignore[no-untyped-def]
    defaults = {
        "equity": D(1_000_000),
        "buying_power": D(1_000_000),
        "prices": {"7203": D(2500)},
        "atr": {"7203": D(50)},
    }
    return SizingContext(**{**defaults, **kwargs})  # type: ignore[arg-type]


def test_equal_weight_divides_equity() -> None:
    sizer = EqualWeightSizer(SizingConfig(method="equal_weight", max_positions=4))
    targets = sizer.size({"7203": signal()}, sizing_ctx(), entry_threshold=0.3, exit_threshold=0.1)

    # 1,000,000 / 4 = 250,000 → 2,500円で100株
    assert targets[0].quantity == D(100)


def test_fixed_notional_uses_the_configured_amount() -> None:
    sizer = FixedNotionalSizer(SizingConfig(method="fixed_notional", fixed_notional_jpy=D(500_000)))
    targets = sizer.size({"7203": signal()}, sizing_ctx(), entry_threshold=0.3, exit_threshold=0.1)

    assert targets[0].quantity == D(200)  # 500,000 / 2,500


def test_atr_risk_sizes_by_risk_budget() -> None:
    """リスク許容額 ÷ 損切り幅 で株数が決まる。"""
    sizer = AtrRiskSizer(
        SizingConfig(
            method="atr_risk",
            risk_per_trade=D("0.01"),
            atr_stop_multiple=D(2),
            max_positions=1,
        )
    )
    # 許容損失 10,000円、損切り幅 = ATR 50 × 2 = 100円 → 100株
    targets = sizer.size({"7203": signal()}, sizing_ctx(), entry_threshold=0.3, exit_threshold=0.1)

    assert targets[0].quantity == D(100)


def test_atr_risk_takes_fewer_shares_when_volatile() -> None:
    """値動きが激しい銘柄ほど株数を減らし、リスクを揃える。"""
    sizer = AtrRiskSizer(
        SizingConfig(
            method="atr_risk",
            risk_per_trade=D("0.01"),
            atr_stop_multiple=D(2),
            max_positions=2,
        )
    )
    # 金額上限が先に効くと差が出ないので、資産規模を十分に取る
    ctx = {"equity": D(10_000_000), "buying_power": D(10_000_000)}

    calm = sizer.size(
        {"7203": signal()},
        sizing_ctx(atr={"7203": D(25)}, **ctx),
        entry_threshold=0.3,
        exit_threshold=0.1,
    )
    wild = sizer.size(
        {"7203": signal()},
        sizing_ctx(atr={"7203": D(100)}, **ctx),
        entry_threshold=0.3,
        exit_threshold=0.1,
    )

    # 許容損失10万円 ÷ 損切り幅: 穏やか(50円)→2000株、激しい(200円)→500株
    assert calm[0].quantity == D(2000)
    assert wild[0].quantity == D(500)


def test_atr_risk_skips_symbols_without_atr() -> None:
    """ATR が無いまま当てずっぽうで建てない。"""
    sizer = AtrRiskSizer(SizingConfig(method="atr_risk"))
    targets = sizer.size(
        {"7203": signal()}, sizing_ctx(atr={}), entry_threshold=0.3, exit_threshold=0.1
    )
    assert targets == []


def test_sizer_rounds_down_to_lot_size() -> None:
    sizer = FixedNotionalSizer(SizingConfig(method="fixed_notional", fixed_notional_jpy=D(400_000)))
    # 400,000 / 2,500 = 160株 → 単元100株に切り捨てて100株
    targets = sizer.size({"7203": signal()}, sizing_ctx(), entry_threshold=0.3, exit_threshold=0.1)
    assert targets[0].quantity == D(100)


def test_sizer_exits_on_bearish_signal() -> None:
    """空売りはできないので、弱気は「手仕舞い」を意味する。"""
    sizer = EqualWeightSizer(SizingConfig())
    targets = sizer.size(
        {"7203": signal(direction=-0.9)},
        sizing_ctx(positions={"7203": position()}),
        entry_threshold=0.3,
        exit_threshold=0.1,
    )
    assert targets[0].quantity == D(0)


def test_sizer_never_targets_a_short_position() -> None:
    sizer = EqualWeightSizer(SizingConfig())
    targets = sizer.size(
        {"7203": signal(direction=-1.0)}, sizing_ctx(), entry_threshold=0.3, exit_threshold=0.1
    )
    assert all(t.quantity >= 0 for t in targets)


def test_sizer_holds_between_thresholds() -> None:
    """ヒステリシス: 閾値の間では建玉を動かさない。

    これが無いと、シグナルが閾値付近で揺れるたびに売買して
    手数料で削られる。
    """
    sizer = EqualWeightSizer(SizingConfig())
    targets = sizer.size(
        {"7203": signal(direction=0.2)},
        sizing_ctx(positions={"7203": position(qty=100)}),
        entry_threshold=0.3,
        exit_threshold=0.1,
    )
    assert targets[0].quantity == D(100)  # 現状維持


def test_sizer_does_not_enter_below_entry_threshold() -> None:
    sizer = EqualWeightSizer(SizingConfig())
    targets = sizer.size(
        {"7203": signal(direction=0.2)}, sizing_ctx(), entry_threshold=0.3, exit_threshold=0.1
    )
    assert targets == []


def test_sizer_exits_positions_with_no_signal() -> None:
    """シグナルが消えた保有銘柄を放置しない。"""
    sizer = EqualWeightSizer(SizingConfig())
    targets = sizer.size(
        {}, sizing_ctx(positions={"7203": position()}), entry_threshold=0.3, exit_threshold=0.1
    )
    assert targets[0].quantity == D(0)
    assert "消滅" in targets[0].reason


def test_sizer_respects_max_positions() -> None:
    sizer = EqualWeightSizer(SizingConfig(max_positions=2))
    signals = {s: signal(s, 1.0 - i * 0.1) for i, s in enumerate(["A", "B", "C", "D"])}
    ctx = sizing_ctx(prices={s: D(1000) for s in signals}, atr={s: D(20) for s in signals})

    targets = sizer.size(signals, ctx, entry_threshold=0.3, exit_threshold=0.1)

    assert len([t for t in targets if t.quantity > 0]) == 2


def test_sizer_prefers_the_strongest_signals() -> None:
    sizer = EqualWeightSizer(SizingConfig(max_positions=1))
    signals = {"weak": signal("weak", 0.4), "strong": signal("strong", 0.95)}
    ctx = sizing_ctx(prices={"weak": D(1000), "strong": D(1000)})

    targets = sizer.size(signals, ctx, entry_threshold=0.3, exit_threshold=0.1)

    assert [t.symbol for t in targets if t.quantity > 0] == ["strong"]


def test_build_sizer_from_config() -> None:
    assert isinstance(build_sizer(SizingConfig(method="atr_risk")), AtrRiskSizer)
    assert isinstance(build_sizer(SizingConfig(method="equal_weight")), EqualWeightSizer)


# ==========================================================================
# リスク上限
# ==========================================================================


def risk_ctx(**kwargs) -> RiskContext:  # type: ignore[no-untyped-def]
    defaults = {
        "equity": D(1_000_000),
        "balance": Balance("JPY", D(1_000_000), D(1_000_000)),
        "base_prices": {"7203": D(2500)},
    }
    return RiskContext(**{**defaults, **kwargs})  # type: ignore[arg-type]


def order(symbol: str = "7203", qty: int = 100, price: str = "2500", side: Side = Side.BUY):  # type: ignore[no-untyped-def]
    return OrderRequest(
        client_order_id="o1",
        symbol=symbol,
        side=side,
        order_type=OrderType.LIMIT,
        quantity=D(qty),
        limit_price=D(price),
    )


def test_risk_approves_a_normal_order() -> None:
    manager = RiskManager(RiskConfig(), ["7203"])
    assert manager.check(order(), risk_ctx()).approved


def test_risk_blocks_symbols_outside_the_allowlist() -> None:
    """設定に無い銘柄には、どんな経路でも発注しない。"""
    manager = RiskManager(RiskConfig(), ["7203"])
    decision = manager.check(order(symbol="9999"), risk_ctx())

    assert not decision.approved
    assert "allowlist" in decision.reason


def test_risk_kill_switch_blocks_everything() -> None:
    manager = RiskManager(RiskConfig(kill_switch=True), ["7203"])
    decision = manager.check(order(), risk_ctx())

    assert not decision.approved
    assert "キルスイッチ" in decision.reason


def test_risk_blocks_oversized_orders() -> None:
    manager = RiskManager(RiskConfig(max_order_value_jpy=D(100_000)), ["7203"])
    decision = manager.check(order(qty=100, price="2500"), risk_ctx())

    assert not decision.approved
    assert "上限" in decision.reason


def test_risk_blocks_when_daily_order_count_reached() -> None:
    manager = RiskManager(RiskConfig(max_orders_per_day=5), ["7203"])
    decision = manager.check(order(), risk_ctx(orders_today=5))

    assert not decision.approved
    assert "発注件数" in decision.reason


def test_risk_stops_new_entries_after_a_big_loss() -> None:
    manager = RiskManager(RiskConfig(max_daily_loss_jpy=D(50_000)), ["7203"])
    decision = manager.check(order(), risk_ctx(realized_pnl_today=D(-60_000)))

    assert not decision.approved
    assert "損失" in decision.reason


def test_risk_still_allows_exits_after_a_big_loss() -> None:
    """損失が膨らんでいるときに決済を封じるのは危険。売りは通す。"""
    manager = RiskManager(RiskConfig(max_daily_loss_jpy=D(50_000)), ["7203"])
    ctx = risk_ctx(realized_pnl_today=D(-60_000), positions={"7203": position(qty=100)})
    assert manager.check(order(side=Side.SELL), ctx).approved


def test_risk_blocks_selling_more_than_held() -> None:
    manager = RiskManager(RiskConfig(), ["7203"])
    decision = manager.check(
        order(side=Side.SELL, qty=200), risk_ctx(positions={"7203": position(qty=100)})
    )

    assert not decision.approved
    assert "空売り" in decision.reason


def test_risk_blocks_when_buying_power_is_short() -> None:
    manager = RiskManager(RiskConfig(max_order_value_jpy=D(10_000_000)), ["7203"])
    decision = manager.check(
        order(qty=100, price="2500"), risk_ctx(balance=Balance("JPY", D(1000), D(1000)))
    )

    assert not decision.approved
    assert "買付余力" in decision.reason


def test_risk_blocks_excessive_concentration() -> None:
    manager = RiskManager(
        RiskConfig(max_position_weight=D("0.1"), max_order_value_jpy=D(10_000_000)), ["7203"]
    )
    decision = manager.check(order(qty=100, price="2500"), risk_ctx())

    assert not decision.approved
    assert "比率" in decision.reason


def test_risk_blocks_limit_outside_the_daily_price_band() -> None:
    """ストップ高を超える指値は取引所に弾かれる。"""
    manager = RiskManager(RiskConfig(max_order_value_jpy=D(10_000_000)), ["7203"])
    # 基準値段 2,500円 → 値幅 ±500円。3,500円は範囲外
    decision = manager.check(order(qty=100, price="3500"), risk_ctx())

    assert not decision.approved
    assert "値幅制限" in decision.reason


def test_risk_blocks_when_preview_deviates() -> None:
    """ブローカーの見積りが自前計算と食い違ったら止める。

    数量の桁違いや銘柄の取り違えがここで捕まる。
    """
    manager = RiskManager(RiskConfig(max_preview_deviation=D("0.02")), ["7203"])
    preview = OrderPreview(estimated_cost=D(2_500_000), estimated_fee=D(275))  # 10倍

    decision = manager.check(order(), risk_ctx(), preview=preview)

    assert not decision.approved
    assert "乖離" in decision.reason


def test_risk_accepts_preview_within_tolerance() -> None:
    manager = RiskManager(RiskConfig(max_preview_deviation=D("0.02")), ["7203"])
    preview = OrderPreview(estimated_cost=D(250_100), estimated_fee=D(275))

    assert manager.check(order(), risk_ctx(), preview=preview).approved


def test_check_all_splits_approved_and_rejected() -> None:
    manager = RiskManager(RiskConfig(), ["7203"])
    requests = [order(symbol="7203"), order(symbol="9999")]

    approved, rejected = manager.check_all(requests, risk_ctx())

    assert [r.symbol for r in approved] == ["7203"]
    assert "9999" in rejected


def test_check_all_consumes_buying_power_as_it_approves() -> None:
    """1サイクル内で余力を使い切るのを防ぐ。"""
    manager = RiskManager(
        RiskConfig(max_order_value_jpy=D(10_000_000), max_position_weight=D(1)), ["A", "B"]
    )
    ctx = RiskContext(
        equity=D(10_000_000),
        balance=Balance("JPY", D(300_000), D(300_000)),
        base_prices={"A": D(2500), "B": D(2500)},
    )
    requests = [order(symbol="A"), order(symbol="B")]

    approved, rejected = manager.check_all(requests, ctx)

    assert [r.symbol for r in approved] == ["A"]
    assert "買付余力" in rejected["B"]


def test_check_all_enforces_the_daily_count_within_one_cycle() -> None:
    manager = RiskManager(RiskConfig(max_orders_per_day=2), ["A", "B", "C"])
    ctx = RiskContext(
        equity=D(100_000_000),
        balance=Balance("JPY", D(100_000_000), D(100_000_000)),
        base_prices={s: D(2500) for s in "ABC"},
    )

    approved, rejected = manager.check_all([order(symbol=s) for s in "ABC"], ctx)

    assert len(approved) == 2
    assert "C" in rejected


# ==========================================================================
# ストップ（逆指値の合成）
# ==========================================================================


def test_stop_triggers_at_or_below_the_price() -> None:
    stop = Stop("7203", D(2400), D(2500), TODAY)
    assert stop.is_triggered(D(2400))
    assert stop.is_triggered(D(2399))
    assert not stop.is_triggered(D(2401))


def test_stop_book_creates_stops_for_unprotected_positions() -> None:
    """ストップの無い建玉は危険なので、自動で設定する。"""
    book = StopBook()
    book.ensure({"7203": position(cost="2500")}, {"7203": D(50)}, TODAY, atr_multiple=D(2))

    stop = book.get("7203")
    assert stop is not None
    assert stop.stop_price == D(2400)  # 2500 - 50×2


def test_stop_book_drops_stops_for_closed_positions() -> None:
    book = StopBook({"7203": Stop("7203", D(2400), D(2500), TODAY)})
    book.ensure({}, {}, TODAY)
    assert len(book) == 0


def test_stop_book_skips_symbols_without_atr() -> None:
    book = StopBook()
    book.ensure({"7203": position()}, {}, TODAY)
    assert len(book) == 0


def test_trailing_stop_moves_up_only() -> None:
    """トレーリングは引き上げるだけ。下げると損失が青天井になる。"""
    book = StopBook(
        {"7203": Stop("7203", D(2400), D(2500), TODAY, trailing=True, highest_close=D(2500))}
    )

    book.update_trailing({"7203": D(2800)}, {"7203": D(50)})
    raised = book.get("7203").stop_price  # type: ignore[union-attr]
    assert raised == D(2700)  # 2800 - 50×2

    book.update_trailing({"7203": D(2500)}, {"7203": D(50)})
    assert book.get("7203").stop_price == raised  # type: ignore[union-attr]


def test_non_trailing_stop_stays_put() -> None:
    book = StopBook({"7203": Stop("7203", D(2400), D(2500), TODAY, trailing=False)})
    book.update_trailing({"7203": D(3000)}, {"7203": D(50)})
    assert book.get("7203").stop_price == D(2400)  # type: ignore[union-attr]


def test_stop_book_produces_exit_targets() -> None:
    book = StopBook({"7203": Stop("7203", D(2400), D(2500), TODAY)})
    targets = book.exit_targets({"7203": D(2350)})

    assert len(targets) == 1
    assert targets[0].quantity == D(0)
    assert "ストップ抵触" in targets[0].reason


def test_stop_exit_overrides_strategy_target() -> None:
    """損切りは戦略の意見より優先する。"""
    strategy = [TargetPosition("7203", D(300), reason="買い増し")]
    stops = [TargetPosition("7203", D(0), reason="ストップ抵触")]

    merged = apply_stop_priority(strategy, stops)

    assert len(merged) == 1
    assert merged[0].quantity == D(0)


def test_stop_priority_keeps_unrelated_targets() -> None:
    merged = apply_stop_priority(
        [TargetPosition("7203", D(300)), TargetPosition("6758", D(100))],
        [TargetPosition("7203", D(0))],
    )
    by_symbol = {t.symbol: t.quantity for t in merged}
    assert by_symbol == {"7203": D(0), "6758": D(100)}
