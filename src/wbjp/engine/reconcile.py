"""目標建玉と実建玉の差分を注文に変換する。

**このモジュールが自動売買で一番大事な場所**

素朴な実装は「買いシグナルが出た → 買い注文を出す」と書く。これは
落ちる。プロセスが落ちて再起動したら、同じシグナルからもう一度
買い注文が出る。ネットワークが不安定で応答を取りこぼしたら、
発注済みかどうか分からないまま再送する。

ここでは代わりに、毎回こう考える:

    「**あるべき建玉**は何株か」
    「**今の建玉＋未約定注文**は何株か」
    「その差だけ注文する」

この形にすると、同じ状態から何度実行しても結果が変わらない（冪等）。
再起動しても、応答を取りこぼしても、二重に建つことがない。

未約定注文を必ず足し込むのが要点。これを忘れると、昨日出した指値が
まだ板に残っているのに「まだ建玉が無い」と判断してもう一度買う。
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass
from decimal import Decimal

from wbcore.domain.jp_rules import PriceRounding, violates_same_day_settlement
from wbcore.domain.market_rules import JpMarketRules, MarketRules
from wbcore.domain.models import (
    Order,
    OrderRequest,
    OrderType,
    Position,
    Side,
    TargetPosition,
    TaxAccountType,
    TimeInForce,
    make_client_order_id,
)
from wbcore.logging import get_logger

log = get_logger(__name__)


@dataclass(frozen=True, slots=True)
class ReconcileSettings:
    """差分を注文にするときの決め事。"""

    #: "limit" か "market"
    order_type: str = "limit"
    #: 指値を直近終値からどれだけずらすか（約定しやすい方向へ）
    limit_offset: Decimal = Decimal("0.005")
    tax_type: TaxAccountType = TaxAccountType.SPECIFIC
    time_in_force: TimeInForce = TimeInForce.DAY


@dataclass(frozen=True, slots=True)
class ReconcilePlan:
    """差分の計算結果。

    ``skipped`` に「なぜ注文しなかったか」を残すのが重要。
    注文が出ないときこそ理由を知りたい。
    """

    orders: list[OrderRequest]
    skipped: dict[str, str]

    def __bool__(self) -> bool:
        return bool(self.orders)


def effective_quantity(
    symbol: str,
    positions: dict[str, Position],
    open_orders: Iterable[Order],
) -> Decimal:
    """実効ポジション＝現在の建玉 ＋ 未約定注文の残。

    未約定の買いは「もうすぐ増える株数」なので足し、未約定の売りは
    「もうすぐ減る株数」なので引く。これを勘定に入れないと、
    板に残っている注文をもう一度出すことになる。
    """
    position = positions.get(symbol)
    quantity = position.quantity if position else Decimal(0)

    for order in open_orders:
        if order.symbol != symbol or not order.status.is_open:
            continue
        # 逆指値は「トリガーに達したら売る」保険であって、これから約定する
        # 注文ではない。数えると「建玉が消える予定」と誤認して買い直す。
        if order.order_type.is_stop:
            continue
        quantity += order.signed_remaining

    return quantity


def open_stop_orders(symbol: str, open_orders: Iterable[Order]) -> list[Order]:
    """銘柄に置かれている生きた逆指値。"""
    return [
        o for o in open_orders if o.symbol == symbol and o.status.is_open and o.order_type.is_stop
    ]


def reconcile(
    targets: Iterable[TargetPosition],
    positions: dict[str, Position],
    open_orders: Iterable[Order],
    prices: dict[str, Decimal],
    *,
    order_id_seed: str,
    settings: ReconcileSettings | None = None,
    lot_sizes: dict[str, Decimal] | None = None,
    topix500: set[str] | None = None,
    bought_today: set[str] | None = None,
    rules: MarketRules | None = None,
) -> ReconcilePlan:
    """目標と現状の差だけを注文にする。

    Args:
        targets: あるべき建玉。
        positions: 現在の建玉。
        open_orders: 未約定の注文。**必ず渡すこと。**
        prices: 銘柄 → 直近終値。指値の基準に使う。
        order_id_seed: 注文IDの種。**取引日**を渡すこと。実行ごとに変わる
            値（run_id など）を渡すと、同じ日の再実行が別の注文IDになり
            二重発注になる。
        bought_today: 当日買い付けた銘柄。差金決済の防止に使う。
        rules: 市場の取引ルール。省略時は東証（``topix500`` を反映）。

    Returns:
        発注すべき注文と、見送った理由。
    """
    settings = settings or ReconcileSettings()
    lot_sizes = lot_sizes or {}
    rules = rules or JpMarketRules(topix500)
    bought_today = bought_today or set()
    open_orders = list(open_orders)

    orders: list[OrderRequest] = []
    skipped: dict[str, str] = {}

    for target in targets:
        symbol = target.symbol
        current = effective_quantity(symbol, positions, open_orders)
        delta = target.quantity - current

        if delta == 0:
            continue

        lot = lot_sizes.get(symbol, rules.default_lot_size)
        # 単元株に満たない差分は発注できない。切り捨てて0になるなら見送る。
        tradable = rules.round_to_lot(abs(delta), lot)
        if tradable == 0:
            skipped[symbol] = f"差分 {abs(delta)} 株が売買単位 {lot} 株に満たない"
            continue

        side = Side.BUY if delta > 0 else Side.SELL

        if side is Side.SELL:
            # 現物の差金決済を避ける（同一資金での同日の買い→売り→買い）
            if rules.blocks_same_day_sale and violates_same_day_settlement(
                side, symbol, bought_today
            ):
                skipped[symbol] = "当日買い付けた銘柄のため、差金決済回避で売却を見送り"
                continue

            # 保有を超えて売らない（空売りは不可）
            position = positions.get(symbol)
            available = position.available_quantity if position else Decimal(0)
            if tradable > available:
                tradable = rules.round_to_lot(available, lot)
                if tradable == 0:
                    skipped[symbol] = "売却可能数量が単元株に満たない"
                    continue

        price = prices.get(symbol)
        if settings.order_type == "limit" and (price is None or price <= 0):
            skipped[symbol] = "指値の基準となる価格が取得できない"
            continue

        request = _build_order(
            symbol=symbol,
            side=side,
            quantity=tradable,
            price=price,
            order_id_seed=order_id_seed,
            settings=settings,
            rules=rules,
            reason=target.reason,
        )
        orders.append(request)

        log.debug(
            "差分から注文を生成",
            symbol=symbol,
            target=str(target.quantity),
            current=str(current),
            delta=str(delta),
            side=side.value,
            quantity=str(tradable),
        )

    return ReconcilePlan(orders=orders, skipped=skipped)


def _build_order(
    *,
    symbol: str,
    side: Side,
    quantity: Decimal,
    price: Decimal | None,
    order_id_seed: str,
    settings: ReconcileSettings,
    rules: MarketRules,
    reason: str,
) -> OrderRequest:
    client_order_id = make_client_order_id(order_id_seed, symbol, side, quantity)

    if settings.order_type == "market" or price is None:
        return OrderRequest(
            client_order_id=client_order_id,
            symbol=symbol,
            side=side,
            order_type=OrderType.MARKET,
            quantity=quantity,
            time_in_force=settings.time_in_force,
            tax_type=settings.tax_type,
            reason=reason,
        )

    # 約定しやすい方向にずらす（買いは上、売りは下）
    offset = (
        Decimal(1) + settings.limit_offset
        if side is Side.BUY
        else Decimal(1) - settings.limit_offset
    )
    raw_price = price * offset

    # 呼値に乗っていない指値は取引所に弾かれるため、必ずスナップする
    limit_price = rules.snap_to_tick(
        raw_price,
        side,
        rounding=PriceRounding.AGGRESSIVE,
        symbol=symbol,
    )

    return OrderRequest(
        client_order_id=client_order_id,
        symbol=symbol,
        side=side,
        order_type=OrderType.LIMIT,
        quantity=quantity,
        limit_price=limit_price,
        time_in_force=settings.time_in_force,
        tax_type=settings.tax_type,
        reason=reason,
    )
