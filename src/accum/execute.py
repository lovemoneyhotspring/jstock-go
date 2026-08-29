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
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass, field
from decimal import Decimal

import polars as pl

from accum.config import AccumConfig
from accum.ledger import Ledger
from accum.plan import AccumulationSettings, build_plan
from accum.tactics import Tactic
from wbcore.broker.base import Broker
from wbcore.clock import to_zone
from wbcore.domain.market_rules import PriceRounding, rules_for
from wbcore.domain.models import (
    Market,
    OrderRequest,
    OrderStatus,
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
    #: どの月の積立か（月初の日付）。台帳の月次集計に使う。計画の確認用では None。
    month: dt.date | None = None
    #: 今月の目標（今日まで）と発注済み。ログに数値で残すため。計画の確認用では None。
    target: Decimal | None = None
    placed: Decimal | None = None

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
    """設定と足から、最新の足の投下を銘柄ごとに取り出す（計画の確認用）。

    ライブの発注には :func:`pending_contributions` を使う（確定足で判断し、
    未発注ぶんを繰り越す）。こちらは「最後の足でいくらか」をそのまま返す。

    投下額が 0 の銘柄（入金日でも増額日でもない）は含めない。
    足の無い銘柄は黙って飛ばす——呼び出し側が先に警告している前提。
    判定用の銘柄（``signal_symbol``）の足が無ければ、その戦略は倍率 1 で動く。
    """
    out: list[Contribution] = []
    for entry in config.active:
        for symbol in entry.symbols:
            frame = bars.get(symbol)
            if frame is None or frame.height == 0:
                continue
            tactic = entry.build()
            signal = bars.get(entry.signal_symbol) if entry.signal_symbol else None
            plan = build_plan(
                frame,
                AccumulationSettings(config.monthly_budget, tactic),
                signal_bars=signal,
                signal_strict=entry.signal_lags,
            )
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


@dataclass(frozen=True, slots=True)
class Pending:
    """ライブで今日出すべき投下と、判定しなかった銘柄。"""

    contributions: list[Contribution]
    #: 足が古すぎて判定を見送った銘柄 → 最終足の日付
    stale: dict[str, dt.date] = field(default_factory=dict)


#: 銘柄と月（月初の日付）から、その月に発注済みの額を返す。台帳が実装する。
PlacedLookup = Callable[[str, dt.date], Decimal]


def pending_contributions(
    config: AccumConfig,
    bars: Mapping[str, pl.DataFrame],
    *,
    now: dt.datetime,
    placed: PlacedLookup | None = None,
    max_stale_days: int = 4,
) -> Pending:
    """ライブの規則で「今日出すべき投下」を取り出す。

    **今月の目標（今日まで） − 今月の発注済み** を銘柄ごとに計算し、正なら
    その差額を 1 件の投下にする。目標は計画（:func:`~accum.plan.build_plan`）の
    今月ぶんの投下額の合計で、入金日を過ぎていれば基本予算を含み、そこに
    今日までの増額分が積み上がる。発注済みは台帳から ``placed`` で引く。

    この 1 本の規則で次がすべて同じ扱いになる:

    - 月の途中から始めた → 目標に基本予算が含まれる → 最初に注文を出せる日に全額
    - 月の途中で予算を増やした → 目標が増える → 差額だけ追加
    - cron が動かなかった日があった → 差が残る → 次の実行で埋まる
    - 同じ日に 2 回走った → 1 回目で発注済みが目標に達する → 2 回目は 0

    判断はバックテストと同じく**前日までの確定足**で行い（当日の足は途中経過）、
    買うのは当日の価格。判定用の銘柄も確定足だけ。
    最終足が ``max_stale_days`` 日より古い銘柄は判定しない（``stale`` に入れる）。
    """
    out: list[Contribution] = []
    stale: dict[str, dt.date] = {}
    lookup: PlacedLookup = placed or (lambda _symbol, _month: Decimal(0))
    for entry in config.active:
        tactic = entry.build()
        signal_all = bars.get(entry.signal_symbol) if entry.signal_symbol else None
        for symbol in entry.symbols:
            frame = bars.get(symbol)
            if frame is None or frame.height == 0:
                continue
            today_local = to_zone(now, entry.market.timezone).date()
            latest_date = frame["date"].max()
            assert isinstance(latest_date, dt.date)
            if (today_local - latest_date).days > max_stale_days:
                stale[symbol] = latest_date
                continue
            completed = frame.filter(pl.col("date") < today_local)
            if completed.height == 0:
                continue
            signal = (
                signal_all.filter(pl.col("date") < today_local) if signal_all is not None else None
            )
            plan = build_plan(
                completed,
                AccumulationSettings(config.monthly_budget, tactic),
                signal_bars=signal,
                signal_strict=entry.signal_lags,
            )
            month = today_local.replace(day=1)
            this_month = plan.filter(pl.col("date") >= month)
            if this_month.height == 0:
                continue  # 今月の確定足がまだ無い（月初の初日）
            target = Decimal(str(int(this_month["amount"].sum())))
            already = lookup(symbol, month)
            due = target - already
            if due <= 0:
                continue
            last = this_month.row(-1, named=True)
            extras = int(this_month["extra"].sum())
            reason = (
                f"今月の目標 {target:,.0f}（基本 {int(this_month['base'].sum()):,}"
                f"＋増額 {extras:,}）− 発注済み {already:,.0f}"
            )
            out.append(
                Contribution(
                    symbol=symbol,
                    market=entry.market,
                    date=today_local,
                    close=Decimal(str(frame["close"][-1])),
                    amount=due,
                    multiplier=float(last["multiplier"]),
                    reason=reason,
                    tactic=tactic,
                    month=month,
                    target=target,
                    placed=already,
                )
            )
    return Pending(out, stale)


@dataclass(frozen=True, slots=True)
class StatusChange:
    """照会で分かった注文の変化。"""

    client_order_id: str
    symbol: str
    before: str
    after: OrderStatus
    filled_quantity: Decimal
    quantity: Decimal

    @property
    def lost_amount_ratio(self) -> Decimal:
        """未約定のまま終わった割合（0 なら全部約定）。"""
        if self.after not in {OrderStatus.CANCELLED, OrderStatus.REJECTED, OrderStatus.EXPIRED}:
            return Decimal(0)
        if self.quantity <= 0:
            return Decimal(1)
        return (self.quantity - self.filled_quantity) / self.quantity

    def describe(self) -> str:
        filled, total = _qty(self.filled_quantity), _qty(self.quantity)
        if self.lost_amount_ratio == 0:
            return f"{self.symbol}: {self.after.value}（{filled}/{total} 約定）"
        return (
            f"{self.symbol}: {self.after.value}（{filled}/{total} 約定、"
            f"未約定 {self.lost_amount_ratio:.0%} は次回に持ち越し）"
        )


def _qty(value: Decimal) -> str:
    """数量の表示。``30.000000`` ではなく ``30``、端数があればそのまま。"""
    text = f"{value.normalize():f}"
    return text.rstrip("0").rstrip(".") if "." in text else text


def sync_order_status(ledger: Ledger, broker_for: Callable[[Market], Broker]) -> list[StatusChange]:
    """結果が確定していない注文をブローカーに照会し、台帳を更新する。

    見つからない注文（ブローカー側に無い）は UNKNOWN のまま残す。
    勝手に「失効」にすると、実は板に残っていた注文と二重になる。
    """
    changes: list[StatusChange] = []
    for row in ledger.open_orders():
        if row.market is None:
            continue
        order = broker_for(row.market).get_order(row.client_order_id)
        if order is None:
            continue
        if order.status.value == row.status and order.filled_quantity == row.filled_quantity:
            continue
        ledger.update_status(
            row.client_order_id,
            order.status,
            filled_quantity=order.filled_quantity,
            avg_fill_price=order.avg_fill_price,
            broker_order_id=order.broker_order_id,
        )
        changes.append(
            StatusChange(
                row.client_order_id,
                row.symbol,
                row.status,
                order.status,
                order.filled_quantity,
                order.quantity,
            )
        )
    return changes


#: 成行が「気配値が無い」で拒否されたときのエラーコード（Webull）。
QUOTE_NOT_FOUND = "QUOTE_NOT_FOUND"


def to_order(
    contribution: Contribution,
    *,
    tax_type: TaxAccountType,
    lot_size: int | None = None,
    order_type: OrderType = OrderType.MARKET,
    limit_offset: Decimal = Decimal("0.01"),
    seed: str = "accum",
) -> OrderRequest:
    """投下額を買い注文にする。

    株数は ``金額 ÷ 価格`` を単元に切り捨てる。切り上げると予算を超える。
    指値は ``価格 × (1 + limit_offset)`` を呼値に乗せる（約定しやすい側に丸める）。

    Args:
        seed: 注文IDの種。同じ判断からは同じIDになる。成行を指値で出し直す
            ときは別の種にして、拒否された注文と区別する。

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
    limit_price: Decimal | None = None
    if order_type is OrderType.LIMIT:
        limit_price = rules.snap_to_tick(
            contribution.close * (Decimal(1) + limit_offset),
            Side.BUY,
            rounding=PriceRounding.AGGRESSIVE,
            symbol=symbol,
        )
    return OrderRequest(
        client_order_id=make_client_order_id(
            f"{seed}|{contribution.date}", symbol, Side.BUY, quantity
        ),
        symbol=symbol,
        side=Side.BUY,
        order_type=order_type,
        quantity=quantity,
        limit_price=limit_price,
        tax_type=tax_type,
        reason=f"積立 {contribution.date} ×{contribution.multiplier:g} {contribution.reason}",
    )


def should_fallback_to_limit(request: OrderRequest, error: Exception) -> bool:
    """成行が「気配値が無い」で拒否された注文か。指値で出し直す対象。"""
    return request.order_type is OrderType.MARKET and QUOTE_NOT_FOUND in str(error)


def build_orders(
    contributions: Iterable[Contribution],
    *,
    tax_type: TaxAccountType,
    lot_sizes: Mapping[str, int] | None = None,
    moment: dt.datetime | None = None,
    ignore_window: bool = False,
    order_type: OrderType = OrderType.MARKET,
    limit_offset: Decimal = Decimal("0.01"),
) -> list[PlannedOrder]:
    """投下の一覧を注文にする。作れないものは理由付きで残す。

    Args:
        lot_sizes: 設定上の銘柄コード → 単元株数。
        moment: 発注時間帯の判定に使う時刻。省略時は現在。
        ignore_window: 時間帯の外でも注文を作る（手動で流すとき）。
        order_type: 成行か指値か。``limit_offset`` は指値のときの上乗せ率。
    """
    planned: list[PlannedOrder] = []
    for c in contributions:
        if not ignore_window and not c.tactic.allows_order(moment):
            planned.append(PlannedOrder(c, None, f"発注時間帯の外（{c.tactic.window.describe()}）"))
            continue
        try:
            request = to_order(
                c,
                tax_type=tax_type,
                lot_size=(lot_sizes or {}).get(c.symbol),
                order_type=order_type,
                limit_offset=limit_offset,
            )
        except ValueError as exc:
            planned.append(PlannedOrder(c, None, str(exc)))
            continue
        planned.append(PlannedOrder(c, request))
    return planned
