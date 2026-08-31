"""9:00 の判断: 気配からギャップを出し、下位 N 銘柄を予算に合わせて株数にする。

純粋関数だけ。``plan`` の候補（前夜）と気配（当日）を受け取り、注文の元になる
:class:`Pick` を返す。バックテストは同じ順位付けをパネルに対して行う。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from decimal import ROUND_DOWN, Decimal

import polars as pl

from daytrade.config import SignalConfig
from daytrade.fees import commission
from wbcore.domain.jp_rules import DEFAULT_LOT_SIZE, price_limit_range


def limit_down_price(prev_close: Decimal) -> Decimal:
    """前日終値を基準値段とするストップ安の値段。"""
    return price_limit_range(prev_close)[0]


@dataclass(frozen=True, slots=True)
class Quote:
    """気配・現在値。``price`` は寄付値（板寄せ後）か、寄り前なら気配値。"""

    symbol: str
    price: Decimal
    at: dt.datetime
    #: 取得元。ログと「遅延の気配を使っていないか」の判断に使う。
    source: str = ""
    delayed: bool = False


@dataclass(frozen=True, slots=True)
class Pick:
    """買う銘柄 1 つ。"""

    symbol: str
    code: str
    name: str
    prev_close: Decimal
    price: Decimal
    gap: Decimal
    quantity: Decimal
    rank: int

    @property
    def amount(self) -> Decimal:
        return self.price * self.quantity

    @property
    def fee(self) -> Decimal:
        """片道の手数料（見込み）。"""
        return commission(self.amount)


def gap_rank_expr(config: SignalConfig, over: str | None = "Date") -> pl.Expr:
    """``gap`` 列を条件で絞り、小さい順に順位を付ける（条件外は null）。パネルと 1 日で共用。"""
    ok = (pl.col("gap") < float(config.max_gap)) & (pl.col("gap") >= float(config.min_gap))
    rank = pl.when(ok).then(pl.col("gap")).rank("ordinal")
    return rank.over(over) if over else rank


def shares_for(budget: Decimal, price: Decimal, lot: Decimal = DEFAULT_LOT_SIZE) -> Decimal:
    """予算内で買える株数（単元に切り捨て）。1 単元に届かなければ 0。"""
    if price <= 0 or budget <= 0:
        return Decimal(0)
    lots = (budget / (price * lot)).to_integral_value(rounding=ROUND_DOWN)
    return lots * lot


def pick(
    candidates: pl.DataFrame,
    quotes: dict[str, Quote],
    *,
    n: int,
    budget: Decimal,
    config: SignalConfig,
    lot_sizes: dict[str, Decimal] | None = None,
) -> list[Pick]:
    """候補（``eligible`` が真）に気配を当て、ギャップ下位 N 銘柄を選ぶ。

    予算で 1 単元に届かない銘柄は飛ばして次点を繰り上げる（研究と同じ）。
    気配が無い銘柄は対象外（ログは呼び出し側で）。
    """
    if n < 1:
        return []
    rows = candidates.filter(pl.col("eligible")).select("Code", "symbol", "name", "prev_close")
    lots = lot_sizes or {}
    scored: list[tuple[Decimal, str, str, str, Decimal, Decimal]] = []
    for code, symbol, name, prev_close in rows.iter_rows():
        quote = quotes.get(symbol)
        if quote is None or quote.price <= 0 or prev_close is None or prev_close <= 0:
            continue
        prev = Decimal(str(prev_close))
        gap = quote.price / prev - Decimal(1)
        if not (gap < config.max_gap and gap >= config.min_gap):
            continue
        if config.skip_limit_down and quote.price <= limit_down_price(prev):
            continue
        scored.append((gap, symbol, code, name or "", prev, quote.price))
    scored.sort(key=lambda row: (row[0], row[1]))
    picks: list[Pick] = []
    rank = 0
    for gap, symbol, code, name, prev, price in scored:
        rank += 1
        quantity = shares_for(budget, price, lots.get(symbol, DEFAULT_LOT_SIZE))
        if quantity <= 0:
            continue
        picks.append(
            Pick(
                symbol=symbol,
                code=code,
                name=name,
                prev_close=prev,
                price=price,
                gap=gap.quantize(Decimal("0.0001")),
                quantity=quantity,
                rank=rank,
            )
        )
        if len(picks) >= n:
            break
    return picks
