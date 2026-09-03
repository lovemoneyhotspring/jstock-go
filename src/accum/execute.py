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
from wbcore import execution
from wbcore.broker.base import Broker
from wbcore.clock import now_utc, to_zone
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

        設定には足の取得に合わせて ``1305.T`` と書くが、発注では ``1305``。
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
    if market is not Market.JP:
        raise ValueError(f"{symbol}: {market.value} 市場は発注できません（日本株のみ）")
    return symbol.removesuffix(".T")


def ledger_symbol(config: AccumConfig, symbol: str) -> str:
    """設定上の銘柄コード（``1305.T``）を台帳の表記（ブローカーの ``1305``）にする。

    台帳は :class:`~wbcore.domain.models.OrderRequest` の銘柄で記録されるため
    ブローカーの表記になる。設定の表記のまま照会すると発注済みが常に 0 に見え、
    同じ月の予算を毎回買い直す（実際に起きかけた）。
    """
    market = config.market_of(symbol)
    return broker_symbol(symbol, market) if market else symbol


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
                AccumulationSettings(entry.monthly_budget, tactic),
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
    #: 判定用の銘柄（``signal_symbol``）の足が古い → 最終足の日付。
    #: 投下は止めない（基本分まで止まる方が損）が、倍率は古い足のままなので警告する
    stale_signals: dict[str, dt.date] = field(default_factory=dict)
    #: リリース日ではないので持ち越した差額 → 銘柄ごとの額。小口注文を避けるため
    deferred: dict[str, Decimal] = field(default_factory=dict)


#: 銘柄と月（月初の日付）から、その月に発注済みの額を返す。台帳が実装する。
#: 銘柄は**設定上の表記**（``1305.T``）で受ける。台帳の表記への変換は呼び出し側。
PlacedLookup = Callable[[str, dt.date], Decimal]

#: 銘柄と月から、その月に実際の発注記録があるかを返す。前月の残りを繰り越す条件。
ActiveLookup = Callable[[str, dt.date], bool]

#: 銘柄から積立の開始日を返す。None なら日割りしない（開始日を管理しない呼び出し）。
StartedLookup = Callable[[str], dt.date | None]


def pending_contributions(
    config: AccumConfig,
    bars: Mapping[str, pl.DataFrame],
    *,
    now: dt.datetime,
    placed: PlacedLookup | None = None,
    active: ActiveLookup | None = None,
    started: StartedLookup | None = None,
    max_stale_days: int = 6,
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

    **差額を出す日は増額と同じ規則に揃える。** 単元に丸めて買えなかった端数や
    小さな予算増は、そのままだと株価が下がった日に 1 単元だけの小口注文になる。
    手数料をまとめるため、差額は次のどちらかのときだけ出す:

    - 直前の確定足が入金日か増額のリリース日（計画の ``amount > 0``）——
      その日はどのみち注文が出るので同じ注文に乗せる
    - 差額が今月の基本目標以上——入金日の注文が丸ごと通らなかった、
      cron が止まっていた、月の途中で始めた、など。リリース日を待たず埋める

    それ以外の日は差額を持ち越す（``deferred`` に入れる）。

    判断はバックテストと同じく**前日までの確定足**で行い（当日の足は途中経過）、
    買うのは当日の価格。判定用の銘柄も確定足だけ。
    最終足が ``max_stale_days`` 日より古い銘柄は判定しない（``stale`` に入れる）。

    **前月の残りは当月に繰り越す。** 単元に届かず買えなかった端数や、月末に
    積み上がった増額は「前月の目標 − 前月の発注済み」として残る。``active`` が
    前月に実際の発注があったと言うときだけ、その残りを当月の目標に足す
    （動いていなかった月や dry-run だけの月の分まで買わないため）。

    **開始月は残り日数で日割りする。** ``started`` が返す開始日がその月の途中なら、
    基本予算 × （開始日から月末までの暦日数 ÷ その月の暦日数）を目標にする
    （25,000 円で残り 15/30 日なら 12,500 円）。開始日より前に積み上がった増額も
    数えない。翌月からは通常どおり全額。
    """
    out: list[Contribution] = []
    stale: dict[str, dt.date] = {}
    stale_signals: dict[str, dt.date] = {}
    deferred: dict[str, Decimal] = {}
    lookup: PlacedLookup = placed or (lambda _symbol, _month: Decimal(0))
    for entry in config.active:
        tactic = entry.build()
        signal_all = bars.get(entry.signal_symbol) if entry.signal_symbol else None
        if entry.signal_symbol and signal_all is not None and signal_all.height:
            signal_today = to_zone(now, entry.signal_market_resolved.timezone).date()
            signal_latest = signal_all["date"].max()
            assert isinstance(signal_latest, dt.date)
            if (signal_today - signal_latest).days > max_stale_days:
                stale_signals[entry.signal_symbol] = signal_latest
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
            budget = entry.monthly_budget
            plan = build_plan(
                completed,
                AccumulationSettings(budget, tactic),
                signal_bars=signal,
                signal_strict=entry.signal_lags,
            )
            month = today_local.replace(day=1)
            this_month = plan.filter(pl.col("date") >= month)
            if this_month.height == 0:
                continue  # 今月の確定足がまだ無い（月初の初日）
            base_target, extras, prorated = _month_target(
                this_month, budget, month, started(symbol) if started else None
            )
            target = base_target + extras
            carried = _carry_over(
                plan, symbol, month, budget, lookup, active, started(symbol) if started else None
            )
            already = lookup(symbol, month)
            due = target + carried - already
            if due <= 0:
                continue
            last = this_month.row(-1, named=True)
            release_day = int(last["amount"]) > 0
            if not release_day and not (base_target > 0 and due >= base_target):
                deferred[symbol] = due
                continue
            reason = (
                f"今月の目標 {target:,.0f}（基本 {base_target:,.0f}"
                + (f"〔{prorated}〕" if prorated else "")
                + f"＋増額 {extras:,.0f}）"
                + (f"＋前月からの繰り越し {carried:,.0f}" if carried else "")
                + f"− 発注済み {already:,.0f}"
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
                    target=target + carried,
                    placed=already,
                )
            )
    return Pending(out, stale, stale_signals, deferred)


def _month_target(
    this_month: pl.DataFrame, budget: Decimal, month: dt.date, started: dt.date | None
) -> tuple[Decimal, Decimal, str]:
    """今月の目標を（基本, 増額, 日割りの説明）で返す。

    基本は入金日の足が確定していれば予算の全額。開始日が今月の途中なら
    残り暦日数で日割りする（開始日を含む）。増額は開始日以降に出す分だけ。
    """
    payday_passed = int(this_month["base"].sum()) > 0
    in_start_month = started is not None and started.replace(day=1) == month and started.day > 1
    scoped = this_month.filter(pl.col("date") >= started) if in_start_month else this_month
    extras = Decimal(str(int(scoped["extra"].sum())))
    if not payday_passed:
        return Decimal(0), extras, ""
    if not in_start_month:
        return Decimal(str(int(budget))), extras, ""
    assert started is not None
    days = _days_in_month(month)
    remaining = days - started.day + 1
    base = Decimal(int(budget * remaining / days))
    return base, extras, f"{started:%-m/%-d} 開始、残り {remaining}/{days} 日で日割り"


def _days_in_month(month: dt.date) -> int:
    following = (month.replace(day=28) + dt.timedelta(days=4)).replace(day=1)
    return (following - month.replace(day=1)).days


def _carry_over(
    plan: pl.DataFrame,
    symbol: str,
    month: dt.date,
    budget: Decimal,
    placed: PlacedLookup,
    active: ActiveLookup | None,
    started: dt.date | None,
) -> Decimal:
    """前月の「目標 − 発注済み」の残り。前月に発注記録が無ければ 0。

    前月の目標は当月と同じ規則（:func:`_month_target`）で出す。前月が開始月なら
    日割り後の額が目標。満額で計算すると、日割りで買わなかった分まで
    「買い残し」として当月に上乗せされ、二重買付になる。
    """
    if active is None:
        return Decimal(0)
    previous = (month - dt.timedelta(days=1)).replace(day=1)
    if not active(symbol, previous):
        return Decimal(0)
    last_month = plan.filter((pl.col("date") >= previous) & (pl.col("date") < month))
    if last_month.height == 0:
        return Decimal(0)
    base, extras, _ = _month_target(last_month, budget, previous, started)
    return max(Decimal(0), base + extras - placed(symbol, previous))


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


#: 送信中（PENDING）のままブローカーに無い注文を「届かなかった」と見なすまでの時間。
#: 送った直後は照会に反映されていないことがあるので、翌日まで待つ。
UNCONFIRMED_GRACE = dt.timedelta(days=1)


def sync_order_status(
    ledger: Ledger, broker_for: Callable[[Market], Broker], *, now: dt.datetime | None = None
) -> list[StatusChange]:
    """結果が確定していない注文をブローカーに照会し、台帳を更新する。

    ブローカーに無い注文は原則そのまま残す（勝手に「失効」にすると、実は
    板に残っていた注文と二重になる）。例外は**送信中（PENDING）のまま
    :data:`UNCONFIRMED_GRACE` を過ぎても無い**注文——応答が返らず記録だけが
    残ったもので、届いていれば翌日には照会できる。これは REJECTED に落とし、
    次の実行で差額として埋め直す。

    約定単価が分かった注文は「発注済み」の額を **株数 × 約定単価** に置き換える。
    判断時の価格のままだと、実際に払った額との差だけ差額の計算がずれる。
    """
    now = now or now_utc()
    changes: list[StatusChange] = []
    for row in ledger.open_orders():
        if row.market is None:
            continue
        # broker_order_id は client_order_id で引けないブローカー（立花証券）のためのヒント
        order = broker_for(row.market).get_order(
            row.client_order_id, broker_order_id=row.broker_order_id
        )
        if order is None:
            placed_at = dt.datetime.fromisoformat(row.placed_at)
            if row.status != OrderStatus.PENDING.value or now - placed_at < UNCONFIRMED_GRACE:
                continue
            ledger.update_status(row.client_order_id, OrderStatus.REJECTED)
            changes.append(
                StatusChange(
                    row.client_order_id,
                    row.symbol,
                    row.status,
                    OrderStatus.REJECTED,
                    Decimal(0),
                    row.quantity,
                )
            )
            continue
        if order.status.value == row.status and order.filled_quantity == row.filled_quantity:
            continue
        # amount はこの直後に約定額で上書きされる。想定はここでしか取れない
        execution.collect(
            event="fill",
            app="accum",
            symbol=row.symbol,
            side="BUY",
            client_order_id=row.client_order_id,
            broker_order_id=order.broker_order_id,
            live=True,
            quantity=row.quantity,
            intent_price=(row.amount / row.quantity) if row.amount and row.quantity else None,
            fill_quantity=order.filled_quantity,
            fill_price=order.avg_fill_price,
            reason=execution.ReasonCode.FILLED
            if order.filled_quantity
            else execution.ReasonCode.EXPIRED,
        )
        ledger.update_status(
            row.client_order_id,
            order.status,
            filled_quantity=order.filled_quantity,
            avg_fill_price=order.avg_fill_price,
            broker_order_id=order.broker_order_id,
            amount=row.quantity * order.avg_fill_price if order.avg_fill_price else None,
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


def unrecorded_fills(
    ledger: Ledger,
    broker_for: Callable[[Market], Broker],
    contributions: Iterable[Contribution],
    *,
    today: dt.date,
) -> dict[str, Decimal]:
    """台帳に無いのにブローカーには約定がある、当月の買い注文を探す。

    台帳を失った（消した・別の環境で動かした）状態で走ると、当月の予算を
    もう一度買う。ブローカーの当月の買い履歴に、台帳に無い注文 ID の約定が
    あればそれで、呼び出し側は発注を止めて人に知らせる。

    Returns:
        設定上の銘柄コード → 台帳に無い約定額（株数 × 約定単価）。
    """
    known = ledger.recorded_ids()
    month_start = today.replace(day=1)
    found: dict[str, Decimal] = {}
    by_market: dict[Market, list[Contribution]] = {}
    for c in contributions:
        by_market.setdefault(c.market, []).append(c)
    for market, items in by_market.items():
        symbols = {c.broker_symbol: c.symbol for c in items}
        for order in broker_for(market).get_order_history(month_start, today):
            if order.side is not Side.BUY or order.filled_quantity <= 0:
                continue
            if order.client_order_id in known or order.symbol not in symbols:
                continue
            price = order.avg_fill_price or Decimal(0)
            found[symbols[order.symbol]] = found.get(symbols[order.symbol], Decimal(0)) + (
                order.filled_quantity * price
            )
    return found


@dataclass(frozen=True)
class LotSizes:
    """発注に使う売買単位と、決め方の記録。"""

    #: 設定上の銘柄コード → 単元株数。API で分かった分は API、他は設定値。
    sizes: dict[str, int]
    #: 設定と API が食い違った銘柄 → (設定値, API 値)。API を採用済み
    mismatches: dict[str, tuple[int, int]] = field(default_factory=dict)
    #: API で確かめられなかった理由（市場ごと）。設定値のまま進んだ
    failures: dict[Market, str] = field(default_factory=dict)


def resolve_lot_sizes(
    contributions: Iterable[Contribution],
    overrides: Mapping[str, int],
    broker_for: Callable[[Market], Broker],
) -> LotSizes:
    """売買単位を決める。ブローカーの銘柄マスタ → 設定 → 市場の既定の順。

    設定は人が書くので間違える（1591 を 10 と書いていた実例）。マスタで
    分かる銘柄はマスタを信じ、食い違いは記録して呼び出し側に知らせる。
    マスタに無い銘柄（新規上場、UAT）は設定値のまま。照会自体が失敗しても
    発注は止めない（単位が違えば発注時にブローカーが拒否する）。
    """
    by_market: dict[Market, list[Contribution]] = {}
    for c in contributions:
        by_market.setdefault(c.market, []).append(c)
    sizes = dict(overrides)
    mismatches: dict[str, tuple[int, int]] = {}
    failures: dict[Market, str] = {}
    for market, items in by_market.items():
        try:
            found = broker_for(market).lot_sizes(c.broker_symbol for c in items)
        except Exception as exc:
            failures[market] = str(exc)
            continue
        for c in items:
            lot = found.get(c.broker_symbol)
            if lot is None:
                continue
            api = int(lot)
            configured = overrides.get(c.symbol)
            if configured is not None and configured != api:
                mismatches[c.symbol] = (configured, api)
            sizes[c.symbol] = api
    return LotSizes(sizes, mismatches, failures)


#: 成行が「気配値が無い」で拒否されたときのエラーコード。
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
    """成行が「気配値が無い／成行禁止」で拒否された注文か。指値で出し直す対象。

    立花証券は「当該銘柄の成行注文はできません」（11142 等）や「前日終値なし(成行禁止)」
    （11109）と返す。``QUOTE_NOT_FOUND`` は同じ意味の一般的なコード。どちらも指値なら通る。
    """
    if request.order_type is not OrderType.MARKET:
        return False
    text = str(error)
    return QUOTE_NOT_FOUND in text or "成行" in text


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
