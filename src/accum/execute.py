"""積立計画を注文に変える。ここから先は :mod:`wbcore.broker` の仕事。

計画（:mod:`accum.plan`）は「その日いくら投下するか」を金額で出す。
発注には株数が要るので、終値で割って単元に丸める。それだけの層だが、
独立させておくと計画器を差し替えても発注の経路は変わらない。

**注文IDは決定論的**

同じ日・同じ銘柄・同じ株数からは必ず同じ ``client_order_id`` が出る
（:func:`wbcore.domain.models.make_client_order_id`）。cron が二重に
走っても、ブローカーが同じ注文と認識して二重買付にならない。
"""

from __future__ import annotations

import datetime as dt
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from decimal import Decimal

import polars as pl

from accum.config import AccumConfig
from accum.plan import AccumulationSettings, build_plan
from accum.tactics import Tactic
from wbcore.domain.market_rules import rules_for
from wbcore.domain.models import (
    Market,
    OrderRequest,
    OrderType,
    Side,
    TaxAccountType,
    make_client_order_id,
)


@dataclass(frozen=True, slots=True)
class Contribution:
    """ある日の1銘柄への投下。計画表の最終行を取り出したもの。"""

    symbol: str
    market: Market
    date: dt.date
    close: Decimal
    amount: Decimal
    multiplier: float
    reason: str
    tactic: Tactic

    @property
    def broker_symbol(self) -> str:
        """ブローカーに渡す銘柄コード。

        設定には足の取得に合わせて ``1305.T`` と書くが、Webull は ``1305``。
        指数（``^N225`` など）は買えないので、ここで弾く。
        """
        return broker_symbol(self.symbol, self.market)


@dataclass(frozen=True, slots=True)
class PlannedOrder:
    """投下と、それから作った注文。作れなかったときは理由を持つ。"""

    contribution: Contribution
    request: OrderRequest | None
    note: str = ""


def broker_symbol(symbol: str, market: Market) -> str:
    """設定上の銘柄コードをブローカーの表記に直す。"""
    if symbol.startswith("^"):
        raise ValueError(f"{symbol}: 指数は発注できません。連動する ETF に置き換えてください")
    if market is Market.JP:
        return symbol.removesuffix(".T")
    return symbol


def todays_contributions(
    config: AccumConfig, bars: Mapping[str, pl.DataFrame]
) -> list[Contribution]:
    """設定と足から、最新日の投下を銘柄ごとに取り出す。

    投下額が 0 の銘柄（入金日でも増額日でもない）は含めない。
    足の無い銘柄は黙って飛ばす——呼び出し側が先に警告している前提。
    """
    out: list[Contribution] = []
    for entry in config.active:
        for symbol in entry.symbols:
            frame = bars.get(symbol)
            if frame is None or frame.height == 0:
                continue
            tactic = entry.build()
            plan = build_plan(frame, AccumulationSettings(config.monthly_budget, tactic))
            last = plan.row(-1, named=True)
            amount = Decimal(str(last["amount"]))
            if amount <= 0:
                continue
            out.append(
                Contribution(
                    symbol=symbol,
                    market=entry.market,
                    date=last["date"],
                    close=Decimal(str(last["close"])),
                    amount=amount,
                    multiplier=float(last["multiplier"]),
                    reason=str(last["reason"]),
                    tactic=tactic,
                )
            )
    return out


def to_order(
    contribution: Contribution,
    *,
    tax_type: TaxAccountType,
    lot_size: int | None = None,
) -> OrderRequest:
    """投下額を成行の買い注文にする。

    株数は ``金額 ÷ 終値`` を単元に切り捨てる。切り上げると予算を超える。

    Raises:
        ValueError: 指数など発注できない銘柄、または1単元に届かないとき。
    """
    symbol = contribution.broker_symbol
    rules = rules_for(contribution.market)
    lot = Decimal(lot_size) if lot_size else rules.default_lot_size
    quantity = rules.round_to_lot(contribution.amount / contribution.close, lot)
    if quantity <= 0:
        needed = lot * contribution.close
        raise ValueError(
            f"{contribution.symbol}: {contribution.amount:,.0f} では1単元（{lot:g}株 ≈ "
            f"{needed:,.0f}）に届きません。lot_size_overrides か予算を見直してください"
        )
    return OrderRequest(
        client_order_id=make_client_order_id(
            f"accum|{contribution.date}", symbol, Side.BUY, quantity
        ),
        symbol=symbol,
        side=Side.BUY,
        order_type=OrderType.MARKET,
        quantity=quantity,
        tax_type=tax_type,
        reason=f"積立 {contribution.date} ×{contribution.multiplier:g} {contribution.reason}",
    )


def build_orders(
    contributions: Iterable[Contribution],
    *,
    tax_type: TaxAccountType,
    lot_sizes: Mapping[str, int] | None = None,
    moment: dt.datetime | None = None,
    ignore_window: bool = False,
) -> list[PlannedOrder]:
    """投下の一覧を注文にする。作れないものは理由付きで残す。

    Args:
        lot_sizes: 設定上の銘柄コード → 単元株数。
        moment: 発注時間帯の判定に使う時刻。省略時は現在。
        ignore_window: 時間帯の外でも注文を作る（手動で流すとき）。
    """
    planned: list[PlannedOrder] = []
    for c in contributions:
        if not ignore_window and not c.tactic.allows_order(moment):
            planned.append(PlannedOrder(c, None, f"発注時間帯の外（{c.tactic.window.describe()}）"))
            continue
        try:
            request = to_order(c, tax_type=tax_type, lot_size=(lot_sizes or {}).get(c.symbol))
        except ValueError as exc:
            planned.append(PlannedOrder(c, None, str(exc)))
            continue
        planned.append(PlannedOrder(c, request))
    return planned
