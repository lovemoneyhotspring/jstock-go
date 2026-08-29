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
from dataclasses import dataclass, field
from decimal import Decimal

import polars as pl

from accum.config import AccumConfig
from accum.plan import AccumulationSettings, build_plan
from accum.tactics import Tactic
from wbcore.clock import to_zone
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


def pending_contributions(
    config: AccumConfig,
    bars: Mapping[str, pl.DataFrame],
    *,
    now: dt.datetime,
    lookback_days: int = 7,
    max_stale_days: int = 4,
) -> Pending:
    """ライブの規則で「今日出すべき投下」を取り出す。

    バックテスト（:mod:`accum.simulate`）と同じ規則にする:

    - **判断は前日までの確定足**で行う。当日の足はザラ場中の途中経過で、
      同じ日でも実行時刻で判定が変わる。判定用の銘柄も同様に確定足だけ。
    - **買うのは当日の価格**（最新の終値。ザラ場中ならその時点の価格）。
    - 投下の日付は**計画の日付**（入金日なら月初の営業日）。注文IDはこれから
      作るので、翌日以降に出しても同じ注文として扱われ、二重にならない。
    - 直近 ``lookback_days`` 日以内の未発注の投下は**繰り越す**。入金日に cron が
      動かなかった月でも、次の実行で買う。発注済みかは台帳（呼び出し側）が見る。
    - 最終足が ``max_stale_days`` 日より古い銘柄は**判定しない**。取得元の障害で
      古い足のまま増額判定するのを防ぐ。``stale`` に入れて呼び出し側が知らせる。
    """
    out: list[Contribution] = []
    stale: dict[str, dt.date] = {}
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
            since = today_local - dt.timedelta(days=lookback_days)
            due = plan.filter((pl.col("amount") > 0) & (pl.col("date") >= since))
            latest_close = Decimal(str(frame["close"][-1]))
            for row in due.iter_rows(named=True):
                out.append(
                    Contribution(
                        symbol=symbol,
                        market=entry.market,
                        date=row["date"],
                        close=latest_close,
                        amount=Decimal(str(row["amount"])),
                        multiplier=float(row["multiplier"]),
                        reason=str(row["reason"]),
                        tactic=tactic,
                    )
                )
    return Pending(out, stale)


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
