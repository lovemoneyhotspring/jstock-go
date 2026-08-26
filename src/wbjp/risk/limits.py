"""リスク上限。発注の直前に置く最後の関門。

設計方針:
    - **既定は拒否**。判断に必要な情報が欠けている場合は通さない。
      「たぶん大丈夫」で通すと、その一回で資産を失う。
    - **理由を必ず残す**。拒否した注文はログと DB に残し、後から
      「なぜ発注されなかったか」を追えるようにする。
    - **allowlist は絶対**。設定に無い銘柄には、どんな経路でも発注しない。
      戦略のバグで変な銘柄コードが出ても、ここで止まる。
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass, field
from decimal import Decimal

from wbjp.config import RiskConfig
from wbjp.domain.jp_rules import is_within_price_limit
from wbjp.domain.models import Balance, OrderPreview, OrderRequest, Position, Side
from wbjp.logging import get_logger

log = get_logger(__name__)


@dataclass(frozen=True, slots=True)
class RiskDecision:
    """1件の注文に対する判定。"""

    approved: bool
    reason: str = ""

    def __bool__(self) -> bool:
        return self.approved


@dataclass(frozen=True, slots=True)
class RiskContext:
    """判定に必要な材料。

    Attributes:
        equity: 総資産。建玉比率の判定に使う。
        balance: 残高と買付余力。
        positions: 現在の建玉。
        base_prices: 銘柄 → 前日終値。値幅制限の基準値段。
        pending_value: 銘柄 → **未約定の買い注文**の想定約定代金。
            建玉比率は「約定分＋発注中」で見る必要がある。約定分だけを
            数えると、板に残っている注文が無視され、全部約定した瞬間に
            上限を突破する。
        orders_today: 当日すでに発注した件数。
        realized_pnl_today: 当日の実現損益。マイナスが大きいと停止する。
    """

    equity: Decimal
    balance: Balance
    positions: dict[str, Position] = field(default_factory=dict)
    base_prices: dict[str, Decimal] = field(default_factory=dict)
    pending_value: dict[str, Decimal] = field(default_factory=dict)
    orders_today: int = 0
    realized_pnl_today: Decimal = Decimal(0)


class RiskManager:
    """発注前の検査。"""

    def __init__(self, config: RiskConfig, allowlist: Iterable[str]) -> None:
        self.config = config
        self.allowlist = set(allowlist)
        if not self.allowlist:
            log.warning(
                "銘柄 allowlist が空です。すべての発注が拒否されます。"
                "config の universe.symbols を設定してください"
            )

    def check(
        self,
        request: OrderRequest,
        ctx: RiskContext,
        preview: OrderPreview | None = None,
    ) -> RiskDecision:
        """1件の注文を検査する。

        Args:
            preview: ブローカーの見積り。渡すと自前計算との乖離も見る。
        """
        for rule in (
            self._kill_switch,
            self._allowlist,
            self._daily_order_count,
            self._daily_loss,
            self._order_value,
            self._price_limit,
            self._sellable_quantity,
            self._buying_power,
            self._position_weight,
        ):
            decision = rule(request, ctx)
            if not decision.approved:
                return decision

        if preview is not None:
            return self._preview_deviation(request, preview)

        return RiskDecision(True, "全項目を通過")

    def check_all(
        self,
        requests: Iterable[OrderRequest],
        ctx: RiskContext,
    ) -> tuple[list[OrderRequest], dict[str, str]]:
        """まとめて検査し、通ったものと拒否理由に分ける。

        当日の発注件数・買付余力・**銘柄ごとの発注中金額**は、承認する
        たびに消費したものとして数える。1回のサイクルで上限を超えるのを
        防ぐため。同じ銘柄に2件出るような場合、1件目を勘定に入れずに
        2件目を判定すると、合計で上限を超える。
        """
        approved: list[OrderRequest] = []
        rejected: dict[str, str] = {}
        orders_today = ctx.orders_today
        remaining_power = ctx.balance.buying_power
        pending_value = dict(ctx.pending_value)

        for request in requests:
            running = RiskContext(
                equity=ctx.equity,
                balance=Balance(
                    currency=ctx.balance.currency,
                    cash_balance=ctx.balance.cash_balance,
                    buying_power=remaining_power,
                    market_value=ctx.balance.market_value,
                    unrealized_pnl=ctx.balance.unrealized_pnl,
                ),
                positions=ctx.positions,
                base_prices=ctx.base_prices,
                pending_value=pending_value,
                orders_today=orders_today,
                realized_pnl_today=ctx.realized_pnl_today,
            )

            decision = self.check(request, running)
            if decision.approved:
                approved.append(request)
                orders_today += 1
                if request.side is Side.BUY:
                    # _notional は基準値段が無いと None を返す。承認済みの
                    # 注文は _order_value を通っているので通常 None には
                    # ならないが、None を引くと実行時に落ちるため明示的に守る。
                    spent = self._notional(request, ctx)
                    if spent is not None:
                        remaining_power -= spent
                        pending_value[request.symbol] = (
                            pending_value.get(request.symbol, Decimal(0)) + spent
                        )
            else:
                rejected[request.symbol] = decision.reason
                log.warning(
                    "リスク判定で発注を見送り",
                    symbol=request.symbol,
                    side=request.side.value,
                    quantity=str(request.quantity),
                    reason=decision.reason,
                )

        return approved, rejected

    # -- 個別のルール -------------------------------------------------------

    def _kill_switch(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        if self.config.kill_switch:
            return RiskDecision(False, "キルスイッチが有効（risk.kill_switch = true）")
        return RiskDecision(True)

    def _allowlist(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        if request.symbol not in self.allowlist:
            return RiskDecision(
                False, f"{request.symbol} は allowlist（universe.symbols）に含まれていない"
            )
        return RiskDecision(True)

    def _daily_order_count(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        if ctx.orders_today >= self.config.max_orders_per_day:
            return RiskDecision(
                False,
                f"当日の発注件数が上限 {self.config.max_orders_per_day} 件に達している",
            )
        return RiskDecision(True)

    def _daily_loss(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        """当日の損失が上限を超えたら、新規建てを止める。

        手仕舞い（売り）は止めない。損失が膨らんでいる状況で
        決済を封じるのは危険なため。
        """
        if request.side is Side.SELL:
            return RiskDecision(True)
        loss = -ctx.realized_pnl_today
        if loss >= self.config.max_daily_loss_jpy:
            return RiskDecision(
                False,
                f"当日の損失 {loss:,.0f}円 が上限 "
                f"{self.config.max_daily_loss_jpy:,.0f}円 に達している",
            )
        return RiskDecision(True)

    def _order_value(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        notional = self._notional(request, ctx)
        if notional is None:
            return RiskDecision(False, "約定代金を見積れないため見送り")
        if notional > self.config.max_order_value_jpy:
            return RiskDecision(
                False,
                f"約定代金 {notional:,.0f}円 が1注文の上限 "
                f"{self.config.max_order_value_jpy:,.0f}円 を超える",
            )
        return RiskDecision(True)

    def _price_limit(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        """指値が値幅制限（ストップ高・安）の内側にあるか。"""
        if request.limit_price is None:
            return RiskDecision(True)
        base = ctx.base_prices.get(request.symbol)
        if base is None or base <= 0:
            return RiskDecision(True)  # 基準値段が無ければ判定しない
        if not is_within_price_limit(request.limit_price, base):
            return RiskDecision(
                False,
                f"指値 {request.limit_price} が基準値段 {base} の値幅制限を外れている",
            )
        return RiskDecision(True)

    def _sellable_quantity(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        if request.side is not Side.SELL:
            return RiskDecision(True)
        position = ctx.positions.get(request.symbol)
        available = position.available_quantity if position else Decimal(0)
        if request.quantity > available:
            return RiskDecision(
                False,
                f"売却可能 {available} 株に対し {request.quantity} 株の売り注文（空売り不可）",
            )
        return RiskDecision(True)

    def _buying_power(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        if request.side is not Side.BUY:
            return RiskDecision(True)
        notional = self._notional(request, ctx)
        if notional is None:
            return RiskDecision(False, "約定代金を見積れないため見送り")
        if notional > ctx.balance.buying_power:
            return RiskDecision(
                False,
                f"必要 {notional:,.0f}円 に対し買付余力 {ctx.balance.buying_power:,.0f}円",
            )
        return RiskDecision(True)

    def _position_weight(self, request: OrderRequest, ctx: RiskContext) -> RiskDecision:
        """1銘柄がポートフォリオを占めすぎないようにする。

        **約定分と発注中を合算して見る。** 板に残っている買い注文は
        「もうすぐ建玉になる株数」なので、これを無視すると、上限ぎりぎりの
        注文を出したあとに同額をもう一度出せてしまい、両方約定した時点で
        上限を大きく超える。
        """
        if request.side is not Side.BUY or ctx.equity <= 0:
            return RiskDecision(True)

        notional = self._notional(request, ctx)
        if notional is None:
            return RiskDecision(True)

        position = ctx.positions.get(request.symbol)
        existing = position.market_value if position else Decimal(0)
        pending = ctx.pending_value.get(request.symbol, Decimal(0))
        weight = (existing + pending + notional) / ctx.equity

        if weight > self.config.max_position_weight:
            detail = f"{request.symbol} の比率 {weight:.1%}"
            if pending > 0:
                detail += (
                    f"（建玉 {existing:,.0f}円 + 発注中 {pending:,.0f}円 + 今回 {notional:,.0f}円）"
                )
            return RiskDecision(
                False,
                f"{detail} が上限 {self.config.max_position_weight:.1%} を超える",
            )
        return RiskDecision(True)

    def _preview_deviation(self, request: OrderRequest, preview: OrderPreview) -> RiskDecision:
        """ブローカーの見積りが自前計算と大きく違うなら止める。

        想定と違う建玉ができかけている合図。数量の桁を間違えた、
        銘柄を取り違えた、といった事故がここで捕まる。
        """
        expected = request.notional
        if expected is None or expected <= 0:
            return RiskDecision(True, "成行のため見積り照合は省略")

        deviation = abs(preview.estimated_cost - expected) / expected
        if deviation > self.config.max_preview_deviation:
            return RiskDecision(
                False,
                f"ブローカー見積り {preview.estimated_cost:,.0f}円 が "
                f"自前計算 {expected:,.0f}円 と {deviation:.1%} 乖離している",
            )
        return RiskDecision(True, "全項目を通過")

    def _notional(self, request: OrderRequest, ctx: RiskContext) -> Decimal | None:
        """概算約定代金。成行は基準値段で見積る。"""
        if request.limit_price is not None:
            return request.quantity * request.limit_price
        base = ctx.base_prices.get(request.symbol)
        if base is None or base <= 0:
            return None
        return request.quantity * base
