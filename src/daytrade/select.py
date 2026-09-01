"""9:00 の判断: 気配からギャップを出し、下位 N 銘柄を予算に合わせて株数にする。

純粋関数だけ。``plan`` の候補（前夜）と気配（当日）を受け取り、注文の元になる
:class:`Pick` を返す。バックテストは同じ順位付けをパネルに対して行う。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from decimal import ROUND_DOWN, Decimal

import polars as pl

from daytrade.config import MarginConfig, SignalConfig
from daytrade.fees import commission
from wbcore.domain.jp_rules import DEFAULT_LOT_SIZE, price_limit_range


def limit_down_price(prev_close: Decimal) -> Decimal:
    """前日終値を基準値段とするストップ安の値段。"""
    return price_limit_range(prev_close)[0]


def limit_up_price(prev_close: Decimal) -> Decimal:
    """前日終値を基準値段とするストップ高の値段。"""
    return price_limit_range(prev_close)[1]


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


def short_rank_expr(config: MarginConfig, over: str | None = "Date") -> pl.Expr:
    """信用売り側の順位付け。``gap_rank_expr`` と対称（ギャップの大きい順、条件外は null）。

    ``shortable`` 列（貸借銘柄=true）を持つフレームに適用すること。ここでは
    貸借の絞り込みはしない（呼び出し側で ``pl.col("shortable")`` と組み合わせる）。
    """
    ok = (pl.col("gap") >= float(config.min_gap)) & (pl.col("gap") < float(config.max_gap))
    rank = pl.when(ok).then(-pl.col("gap")).rank("ordinal")
    return rank.over(over) if over else rank


def shares_for(budget: Decimal, price: Decimal, lot: Decimal = DEFAULT_LOT_SIZE) -> Decimal:
    """予算内で買える株数（単元に切り捨て）。1 単元に届かなければ 0。"""
    if price <= 0 or budget <= 0:
        return Decimal(0)
    lots = (budget / (price * lot)).to_integral_value(rounding=ROUND_DOWN)
    return lots * lot


@dataclass(frozen=True, slots=True)
class Ranked:
    """ギャップ順に並べた候補 1 つ（数量はまだ決めていない）。"""

    rank: int
    symbol: str
    code: str
    name: str
    prev_close: Decimal
    price: Decimal
    gap: Decimal
    #: 20 日の日次ボラ（無ければ None）
    vol: float | None


#: ボラが取れない・極端に小さい銘柄に使う下限（日次 2%）。重みが暴れないため。
VOL_FLOOR = 0.02


def rank(candidates: pl.DataFrame, quotes: dict[str, Quote], config: SignalConfig) -> list[Ranked]:
    """候補（``eligible`` が真）に気配を当て、ギャップの小さい順に並べる。

    気配が無い・ギャップが条件外・ストップ安の銘柄は入らない。
    """
    columns = ["Code", "symbol", "name", "prev_close"]
    has_vol = "vol20" in candidates.columns
    if has_vol:
        columns.append("vol20")
    rows = candidates.filter(pl.col("eligible")).select(columns)
    scored: list[tuple[Decimal, str, str, str, Decimal, Decimal, float | None]] = []
    for row in rows.iter_rows():
        code, symbol, name, prev_close = row[:4]
        vol = row[4] if has_vol else None
        quote = quotes.get(symbol)
        if quote is None or quote.price <= 0 or prev_close is None or prev_close <= 0:
            continue
        prev = Decimal(str(prev_close))
        gap = quote.price / prev - Decimal(1)
        if not (gap < config.max_gap and gap >= config.min_gap):
            continue
        if config.skip_limit_down and quote.price <= limit_down_price(prev):
            continue
        scored.append((gap, symbol, code, name or "", prev, quote.price, vol))
    scored.sort(key=lambda row: (row[0], row[1]))
    return [
        Ranked(
            rank=i,
            symbol=symbol,
            code=code,
            name=name,
            prev_close=prev,
            price=price,
            gap=gap.quantize(Decimal("0.0001")),
            vol=float(vol) if vol is not None else None,
        )
        for i, (gap, symbol, code, name, prev, price, vol) in enumerate(scored, start=1)
    ]


def weights(rows: list[Ranked], weighting: str) -> list[Decimal]:
    """N 銘柄への配分比（合計 1）。``inverse_vol`` は 20 日ボラの逆数、``equal`` は等分。"""
    if not rows:
        return []
    if weighting == "inverse_vol":
        raw = [1.0 / max(r.vol if r.vol is not None else VOL_FLOOR, VOL_FLOOR) for r in rows]
    else:
        raw = [1.0 for _ in rows]
    total = sum(raw)
    return [Decimal(str(w / total)) for w in raw]


def pick(
    candidates: pl.DataFrame,
    quotes: dict[str, Quote],
    *,
    n: int,
    budget: Decimal,
    config: SignalConfig,
    lot_sizes: dict[str, Decimal] | None = None,
    weighting: str = "equal",
    ranked: list[Ranked] | None = None,
) -> list[Pick]:
    """ギャップ下位 N 銘柄を選び、株数を決める。

    まず「1 単元が ``budget`` に収まる」銘柄を順位順に N 個取る（届かない銘柄は次点を繰り上げ）。
    次に ``weighting`` で総予算 ``budget × N`` を按分し、単元に切り捨てる。
    ``inverse_vol`` で按分が小さすぎて 1 単元に届かない銘柄は落ちる（N が減る）。
    """
    if n < 1:
        return []
    lots = lot_sizes or {}
    rows = ranked if ranked is not None else rank(candidates, quotes, config)
    chosen = [
        r for r in rows if shares_for(budget, r.price, lots.get(r.symbol, DEFAULT_LOT_SIZE)) > 0
    ][:n]
    picks: list[Pick] = []
    total = budget * n
    for r, w in zip(chosen, weights(chosen, weighting), strict=True):
        quantity = shares_for(total * w, r.price, lots.get(r.symbol, DEFAULT_LOT_SIZE))
        if quantity <= 0:
            continue
        picks.append(
            Pick(
                symbol=r.symbol,
                code=r.code,
                name=r.name,
                prev_close=r.prev_close,
                price=r.price,
                gap=r.gap,
                quantity=quantity,
                rank=r.rank,
            )
        )
    return picks
