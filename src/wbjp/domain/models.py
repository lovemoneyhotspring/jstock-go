"""ドメインモデル。

金額・価格・数量はすべて ``Decimal`` を使う。float を使うと丸め誤差が
そのまま発注数量や約定金額の誤りになるため、この境界は厳格に守る。

シグナルの強さ（direction / confidence）だけは金銭的意味を持たない
無次元量なので float を使う。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field
from decimal import Decimal
from enum import StrEnum
from typing import Any


class Side(StrEnum):
    """売買方向。値は Webull API がそのまま受け取る文字列。"""

    BUY = "BUY"
    SELL = "SELL"


class OrderType(StrEnum):
    """注文種別。

    **発注できるのは MARKET と LIMIT だけ。** 日本株の STOP_LOSS 系は
    Webull JP がサポートしていないため、損切りは
    :mod:`wbjp.risk.stops` がエンジン側で合成する。

    ``OTHER`` は**読み取り専用**。口座に他の経路で置かれた注文
    （UAT の共有テスト口座には他人の STOP_LOSS が並ぶ）を読んだときに、
    未知の種別で落ちないための受け皿。これを発注に使ってはいけない。
    """

    MARKET = "MARKET"
    LIMIT = "LIMIT"
    OTHER = "OTHER"

    @property
    def is_placeable(self) -> bool:
        """このシステムが発注してよい種別か。"""
        return self in {OrderType.MARKET, OrderType.LIMIT}


class TimeInForce(StrEnum):
    DAY = "DAY"
    GTC = "GTC"


class TaxAccountType(StrEnum):
    """日本の証券口座の課税区分。発注時に指定が要る。"""

    GENERAL = "GENERAL"
    SPECIFIC = "SPECIFIC"
    NISA = "NISA"


class OrderStatus(StrEnum):
    PENDING = "PENDING"
    SUBMITTED = "SUBMITTED"
    PARTIALLY_FILLED = "PARTIALLY_FILLED"
    FILLED = "FILLED"
    CANCELLED = "CANCELLED"
    REJECTED = "REJECTED"
    EXPIRED = "EXPIRED"
    UNKNOWN = "UNKNOWN"

    @property
    def is_terminal(self) -> bool:
        """これ以上状態が変化しないか。"""
        return self in {
            OrderStatus.FILLED,
            OrderStatus.CANCELLED,
            OrderStatus.REJECTED,
            OrderStatus.EXPIRED,
        }

    @property
    def is_open(self) -> bool:
        """まだ板に残っている（＝建玉の計算に含めるべき）か。"""
        return not self.is_terminal


# --------------------------------------------------------------------------
# 市場データ
# --------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class Bar:
    """日足1本。

    ``date`` は取引日（JST）。日足なのでタイムゾーン付き日時ではなく
    ``date`` を使い、タイムゾーン起因のズレを構造的に排除する。
    """

    symbol: str
    date: dt.date
    open: Decimal
    high: Decimal
    low: Decimal
    close: Decimal
    volume: Decimal

    def __post_init__(self) -> None:
        if self.high < self.low:
            raise ValueError(f"{self.symbol} {self.date}: high < low")


# --------------------------------------------------------------------------
# シグナル
# --------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class Signal:
    """戦略が出す売買意見。注文ではない。

    Attributes:
        direction: -1.0（強い売り）〜 +1.0（強い買い）。0.0 は中立。
        confidence: 0.0〜1.0。その意見にどれだけ自信があるか。
            合成時の重み付けに使う。意見がない銘柄は Signal を
            返さないのが正しく、confidence=0 で返すのは避ける。
        reason: 人間がログを読んで判断を追えるようにするための説明。
            省略しないこと。デバッグの生命線になる。
    """

    strategy: str
    symbol: str
    direction: float
    confidence: float = 1.0
    reason: str = ""
    meta: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if not -1.0 <= self.direction <= 1.0:
            raise ValueError(f"direction は -1.0〜1.0: {self.direction}")
        if not 0.0 <= self.confidence <= 1.0:
            raise ValueError(f"confidence は 0.0〜1.0: {self.confidence}")

    @property
    def score(self) -> float:
        """合成に使う実効スコア。"""
        return self.direction * self.confidence


@dataclass(frozen=True, slots=True)
class CombinedSignal:
    """複数戦略のシグナルを合成した結果。

    ``contributions`` に各戦略の寄与を残すことで、
    「なぜこの判断になったか」を後から必ず追える。
    """

    symbol: str
    direction: float
    contributions: dict[str, float] = field(default_factory=dict)
    reason: str = ""

    def __post_init__(self) -> None:
        if not -1.0 <= self.direction <= 1.0:
            raise ValueError(f"direction は -1.0〜1.0: {self.direction}")


# --------------------------------------------------------------------------
# ポジション・残高
# --------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class TargetPosition:
    """あるべき建玉。サイジングの出力であり、リコンサイルの入力。"""

    symbol: str
    quantity: Decimal
    reason: str = ""


@dataclass(frozen=True, slots=True)
class Position:
    """現在の建玉。

    ``available_quantity`` は売却可能数量。約定前の買付や貸株などで
    保有数量より小さくなることがあるため、売り注文はこちらを見る。
    """

    symbol: str
    quantity: Decimal
    available_quantity: Decimal
    cost_price: Decimal
    last_price: Decimal
    currency: str = "JPY"
    tax_type: TaxAccountType = TaxAccountType.GENERAL

    @property
    def market_value(self) -> Decimal:
        return self.quantity * self.last_price

    @property
    def unrealized_pnl(self) -> Decimal:
        return (self.last_price - self.cost_price) * self.quantity


@dataclass(frozen=True, slots=True)
class Balance:
    """口座残高（単一通貨ぶん）。"""

    currency: str
    cash_balance: Decimal
    buying_power: Decimal
    market_value: Decimal = Decimal(0)
    unrealized_pnl: Decimal = Decimal(0)


# --------------------------------------------------------------------------
# 注文
# --------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class OrderRequest:
    """発注リクエスト。

    ``client_order_id`` は決定論的に生成する（:mod:`wbjp.engine.reconcile`）。
    同じ判断からは必ず同じIDが出るため、再実行しても二重発注にならない。
    """

    client_order_id: str
    symbol: str
    side: Side
    order_type: OrderType
    quantity: Decimal
    limit_price: Decimal | None = None
    time_in_force: TimeInForce = TimeInForce.DAY
    tax_type: TaxAccountType = TaxAccountType.GENERAL
    reason: str = ""

    def __post_init__(self) -> None:
        if self.quantity <= 0:
            raise ValueError(f"quantity は正の数: {self.quantity}")
        if self.order_type is OrderType.LIMIT and self.limit_price is None:
            raise ValueError("指値注文には limit_price が必要")
        if self.order_type is OrderType.MARKET and self.limit_price is not None:
            raise ValueError("成行注文に limit_price は指定できない")
        if len(self.client_order_id) > 32:
            raise ValueError(f"client_order_id は32文字以内: {self.client_order_id!r}")

    @property
    def notional(self) -> Decimal | None:
        """概算約定代金。成行では価格が未定なので None。"""
        if self.limit_price is None:
            return None
        return self.quantity * self.limit_price


@dataclass(frozen=True, slots=True)
class OrderPreview:
    """発注前の見積り。自前計算との乖離チェックに使う。"""

    estimated_cost: Decimal
    estimated_fee: Decimal


@dataclass(frozen=True, slots=True)
class OrderAck:
    """発注の受付結果。"""

    client_order_id: str
    broker_order_id: str | None
    status: OrderStatus


@dataclass(frozen=True, slots=True)
class Order:
    """注文の現在の状態。"""

    client_order_id: str
    broker_order_id: str | None
    symbol: str
    side: Side
    order_type: OrderType
    quantity: Decimal
    filled_quantity: Decimal
    status: OrderStatus
    limit_price: Decimal | None = None
    avg_fill_price: Decimal | None = None
    created_at: dt.datetime | None = None

    @property
    def remaining_quantity(self) -> Decimal:
        return self.quantity - self.filled_quantity

    @property
    def signed_remaining(self) -> Decimal:
        """未約定残を符号付きで返す。買いは正、売りは負。

        リコンサイル時に「未約定注文を含めた実効ポジション」を
        計算するために使う。
        """
        sign = Decimal(1) if self.side is Side.BUY else Decimal(-1)
        return sign * self.remaining_quantity


@dataclass(frozen=True, slots=True)
class Fill:
    """約定1件。"""

    client_order_id: str
    symbol: str
    side: Side
    quantity: Decimal
    price: Decimal
    fee: Decimal = Decimal(0)
    filled_at: dt.datetime | None = None
