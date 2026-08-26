"""約定シミュレータ。

バックテストと dry-run の両方で使う。実際の板は再現できないので、
**楽観的すぎない**約定モデルにしてある。

約定の考え方（日足運用）:
    その日の終値で判断して発注し、**翌営業日の寄付で約定**する。
    判断した当日の終値で約定させると、実際には取れない価格で
    売買できることになり、バックテストの成績が過大になる。

指値の扱い:
    翌日の寄付が指値より不利なら約定しない。当日中に指値に届いた
    かどうかは日足では分からないため、寄付だけで判定する。
    実運用より保守的（＝約定しにくい）側に倒している。

逆指値の扱い（米国株）:
    寄付がトリガー以下なら寄付で（ギャップダウンをそのまま食らう）、
    そうでなくその日の安値がトリガーに届けばトリガー価格で約定する。
    安値を渡さなければ寄付だけで判定する（＝エンジン合成と同じ遅れ）。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field, replace
from decimal import ROUND_HALF_UP, Decimal

from wbjp.broker.base import Broker, InsufficientFundsError, OrderRejectedError
from wbjp.domain.models import (
    Balance,
    Fill,
    Order,
    OrderAck,
    OrderPreview,
    OrderRequest,
    OrderStatus,
    OrderType,
    Position,
    Side,
    TaxAccountType,
    TimeInForce,
)
from wbjp.logging import get_logger

log = get_logger(__name__)

#: UAT の preview_order で実測した手数料率。
#: 100株 × 2,500円 = 250,000円 に対し手数料 275円 → 0.11%。
#: 実運用では :meth:`Broker.preview` の値を優先すること。
DEFAULT_COMMISSION_RATE = Decimal("0.0011")

#: 成行注文で見込む滑り。寄付の板の薄さを粗く織り込む。
DEFAULT_SLIPPAGE_RATE = Decimal("0.001")


@dataclass(slots=True)
class _Holding:
    quantity: Decimal
    cost_price: Decimal


@dataclass
class PaperBroker(Broker):
    """現金口座のシミュレータ。

    信用取引は扱わない。空売りもできない: 保有数を超える売り注文は拒否する。
    ``currency`` は表示と手数料の丸め単位にだけ効く（円は整数、ドルはセント）。
    """

    initial_cash: Decimal = Decimal(1_000_000)
    commission_rate: Decimal = DEFAULT_COMMISSION_RATE
    slippage_rate: Decimal = DEFAULT_SLIPPAGE_RATE
    tax_type: TaxAccountType = TaxAccountType.SPECIFIC
    currency: str = "JPY"

    name: str = "paper"

    _cash: Decimal = field(init=False)
    _holdings: dict[str, _Holding] = field(init=False, default_factory=dict)
    _orders: dict[str, Order] = field(init=False, default_factory=dict)
    _marks: dict[str, Decimal] = field(init=False, default_factory=dict)
    _fills: list[Fill] = field(init=False, default_factory=list)
    _realized_pnl: Decimal = field(init=False, default=Decimal(0))
    _bought_today: set[str] = field(init=False, default_factory=set)

    def __post_init__(self) -> None:
        self._cash = self.initial_cash

    # -- 口座 ---------------------------------------------------------------

    @property
    def account_id(self) -> str:
        return "PAPER"

    @property
    def _cent(self) -> Decimal:
        return Decimal("1") if self.currency == "JPY" else Decimal("0.01")

    def get_balance(self) -> Balance:
        return Balance(
            currency=self.currency,
            cash_balance=self._cash,
            buying_power=self._cash,
            market_value=self.market_value,
            unrealized_pnl=sum((p.unrealized_pnl for p in self.get_positions()), start=Decimal(0)),
        )

    def get_positions(self) -> list[Position]:
        return [
            Position(
                symbol=symbol,
                quantity=holding.quantity,
                available_quantity=holding.quantity,
                cost_price=holding.cost_price,
                last_price=self._marks.get(symbol, holding.cost_price),
                currency=self.currency,
                tax_type=self.tax_type,
            )
            for symbol, holding in sorted(self._holdings.items())
            if holding.quantity > 0
        ]

    def get_open_orders(self) -> list[Order]:
        return [o for o in self._orders.values() if o.status.is_open]

    def get_order(self, client_order_id: str) -> Order | None:
        return self._orders.get(client_order_id)

    @property
    def market_value(self) -> Decimal:
        return sum(
            (
                holding.quantity * self._marks.get(symbol, holding.cost_price)
                for symbol, holding in self._holdings.items()
            ),
            start=Decimal(0),
        )

    @property
    def equity(self) -> Decimal:
        """総資産（現金 + 評価額）。"""
        return self._cash + self.market_value

    @property
    def realized_pnl(self) -> Decimal:
        return self._realized_pnl

    @property
    def fills(self) -> list[Fill]:
        return list(self._fills)

    @property
    def bought_today(self) -> set[str]:
        """当日買い付けた銘柄。差金決済の判定に使う。"""
        return set(self._bought_today)

    # -- 発注 ---------------------------------------------------------------

    def preview(self, request: OrderRequest) -> OrderPreview:
        price = self._reference_price(request)
        cost = (price * request.quantity).quantize(self._cent, rounding=ROUND_HALF_UP)
        return OrderPreview(estimated_cost=cost, estimated_fee=self._commission(cost))

    def place(self, request: OrderRequest) -> OrderAck:
        if request.client_order_id in self._orders:
            # 冪等性: 同じ ID の再送は新規注文にしない
            existing = self._orders[request.client_order_id]
            log.info("既知の注文IDのため再送を無視", client_order_id=request.client_order_id)
            return OrderAck(request.client_order_id, request.client_order_id, existing.status)

        if request.side is Side.SELL:
            holding = self._holdings.get(request.symbol)
            available = holding.quantity if holding else Decimal(0)
            if request.quantity > available:
                raise OrderRejectedError(
                    f"{request.symbol}: 保有 {available} 株に対し {request.quantity} 株の売り注文"
                )
        elif request.order_type.is_stop:
            raise OrderRejectedError("買いの逆指値はこのシステムでは扱わない")
        else:
            estimate = self.preview(request)
            if estimate.estimated_cost + estimate.estimated_fee > self._cash:
                raise InsufficientFundsError(
                    f"{request.symbol}: 必要 {estimate.estimated_cost + estimate.estimated_fee} "
                    f"に対し買付余力 {self._cash} ({self.currency})"
                )

        order = Order(
            client_order_id=request.client_order_id,
            broker_order_id=request.client_order_id,
            symbol=request.symbol,
            side=request.side,
            order_type=request.order_type,
            quantity=request.quantity,
            filled_quantity=Decimal(0),
            status=OrderStatus.SUBMITTED,
            limit_price=request.limit_price,
            stop_price=request.stop_price,
            time_in_force=request.time_in_force,
        )
        self._orders[order.client_order_id] = order
        return OrderAck(order.client_order_id, order.broker_order_id, order.status)

    def cancel(self, client_order_id: str) -> None:
        order = self._orders.get(client_order_id)
        if order is None:
            raise OrderRejectedError(f"注文が見つかりません: {client_order_id}")
        if order.status.is_terminal:
            return
        self._orders[client_order_id] = _replace_status(order, OrderStatus.CANCELLED)

    # -- シミュレーション ---------------------------------------------------

    def mark(self, prices: dict[str, Decimal]) -> None:
        """時価を更新する。評価額の計算に使う。"""
        self._marks.update(prices)

    def begin_day(self) -> None:
        """新しい取引日を開始する。差金決済の判定に使う当日買付を空にする。

        **注文はここでは失効させない。** 前日の終値で判断して出した注文は、
        当日の寄付で約定する。ここで失効させてしまうと、約定する機会が
        永遠に来ない。失効は約定処理のあとに
        :meth:`expire_open_orders` で行う。
        """
        self._bought_today.clear()

    def expire_open_orders(self) -> None:
        """約定しなかった DAY 注文を失効させる。GTC（逆指値）は残す。

        呼ぶのは **:meth:`settle` のあと**。1日の正しい順序は
        「寄付で約定 → 未約定を失効 → 当日の終値で判断して新規発注」。
        """
        for client_order_id, order in list(self._orders.items()):
            if order.status.is_open and order.time_in_force is not TimeInForce.GTC:
                self._orders[client_order_id] = _replace_status(order, OrderStatus.EXPIRED)

    def settle(
        self,
        open_prices: dict[str, Decimal],
        when: dt.datetime | None = None,
        *,
        low_prices: dict[str, Decimal] | None = None,
    ) -> list[Fill]:
        """未約定の注文を寄付価格で約定させる。

        Args:
            open_prices: 銘柄 → その日の寄付。
            when: 約定時刻（記録用）。
            low_prices: 銘柄 → その日の安値。逆指値の場中トリガー判定に使う。

        Returns:
            この呼び出しで発生した約定。
        """
        filled: list[Fill] = []
        low_prices = low_prices or {}

        for client_order_id, order in list(self._orders.items()):
            if not order.status.is_open:
                continue

            open_price = open_prices.get(order.symbol)
            if open_price is None:
                continue  # その日に値がつかなかった

            price = self._execution_price(order, open_price, low_prices.get(order.symbol))
            if price is None:
                continue  # 指値に届かず約定せず

            try:
                fill = self._execute(order, price, when)
            except OrderRejectedError as exc:
                log.warning("約定できず注文を拒否", client_order_id=client_order_id, error=str(exc))
                self._orders[client_order_id] = _replace_status(order, OrderStatus.REJECTED)
                continue

            filled.append(fill)
            self._orders[client_order_id] = Order(
                client_order_id=order.client_order_id,
                broker_order_id=order.broker_order_id,
                symbol=order.symbol,
                side=order.side,
                order_type=order.order_type,
                quantity=order.quantity,
                filled_quantity=order.quantity,
                status=OrderStatus.FILLED,
                limit_price=order.limit_price,
                stop_price=order.stop_price,
                avg_fill_price=price,
                created_at=order.created_at,
                time_in_force=order.time_in_force,
            )

        self._fills.extend(filled)
        return filled

    # -- 内部 ---------------------------------------------------------------

    def _execution_price(
        self, order: Order, open_price: Decimal, low_price: Decimal | None = None
    ) -> Decimal | None:
        """約定価格。約定しない場合は None。"""
        if order.order_type is OrderType.MARKET:
            # 成行は不利な方向に滑る
            direction = Decimal(1) if order.side is Side.BUY else Decimal(-1)
            return open_price * (Decimal(1) + direction * self.slippage_rate)

        if order.order_type.is_stop:
            return self._stop_execution_price(order, open_price, low_price)

        limit = order.limit_price
        assert limit is not None  # OrderRequest が保証している

        if order.side is Side.BUY:
            # 寄付が指値以下なら、より有利な寄付で約定する
            return open_price if open_price <= limit else None
        return open_price if open_price >= limit else None

    def _stop_execution_price(
        self, order: Order, open_price: Decimal, low_price: Decimal | None
    ) -> Decimal | None:
        """売りの逆指値の約定価格。

        寄付がトリガー以下ならギャップダウンで寄付約定（成行なので滑る）。
        そうでなくても安値がトリガーに届いていれば、トリガー価格で
        成行に変わったとみなす。STOP_LOSS_LIMIT は指値が寄付より
        不利なら約定しない。
        """
        stop = order.stop_price
        assert stop is not None
        if open_price <= stop:
            trigger_price = open_price
        elif low_price is not None and low_price <= stop:
            trigger_price = stop
        else:
            return None

        if order.order_type is OrderType.STOP_LOSS_LIMIT:
            limit = order.limit_price
            assert limit is not None
            return trigger_price if trigger_price >= limit else None
        return trigger_price * (Decimal(1) - self.slippage_rate)

    def _execute(self, order: Order, price: Decimal, when: dt.datetime | None) -> Fill:
        gross = (price * order.quantity).quantize(self._cent, rounding=ROUND_HALF_UP)
        fee = self._commission(gross)

        if order.side is Side.BUY:
            if gross + fee > self._cash:
                raise InsufficientFundsError(
                    f"{order.symbol}: 必要 {gross + fee} に対し残高 {self._cash} ({self.currency})"
                )
            self._cash -= gross + fee
            holding = self._holdings.setdefault(order.symbol, _Holding(Decimal(0), Decimal(0)))
            total = holding.quantity + order.quantity
            holding.cost_price = (
                holding.cost_price * holding.quantity + price * order.quantity
            ) / total
            holding.quantity = total
            self._bought_today.add(order.symbol)
        else:
            existing = self._holdings.get(order.symbol)
            if existing is None or existing.quantity < order.quantity:
                raise OrderRejectedError(f"{order.symbol}: 売却可能数量が不足")
            self._realized_pnl += (price - existing.cost_price) * order.quantity - fee
            existing.quantity -= order.quantity
            holding = existing
            self._cash += gross - fee
            if holding.quantity == 0:
                del self._holdings[order.symbol]

        self._marks[order.symbol] = price
        return Fill(
            client_order_id=order.client_order_id,
            symbol=order.symbol,
            side=order.side,
            quantity=order.quantity,
            price=price,
            fee=fee,
            filled_at=when,
        )

    def _reference_price(self, request: OrderRequest) -> Decimal:
        if request.limit_price is not None:
            return request.limit_price
        mark = self._marks.get(request.symbol)
        if mark is None:
            raise OrderRejectedError(
                f"{request.symbol}: 成行注文の見積りに使う時価がありません。"
                "先に mark() で価格を設定してください"
            )
        return mark

    def _commission(self, gross: Decimal) -> Decimal:
        return (gross * self.commission_rate).quantize(self._cent, rounding=ROUND_HALF_UP)


def _replace_status(order: Order, status: OrderStatus) -> Order:
    return replace(order, status=status)
