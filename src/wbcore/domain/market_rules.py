"""市場ごとの取引ルール。

エンジン（サイジング・リコンサイル・リスク）は「呼値に乗せる」「単元に
丸める」「値幅制限の内側か」を**この抽象を通して**問い合わせる。
東証の具体的な表は :mod:`wbcore.domain.jp_rules` に置く。

売買するのは東証だけ（:data:`Market.US` は判断材料の指数のためにある）。
別の市場を足すなら :class:`MarketRules` を実装して :func:`rules_for` に
登録する。
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from decimal import Decimal

from wbcore.domain import jp_rules
from wbcore.domain.jp_rules import PriceRounding
from wbcore.domain.models import Market, Side

__all__ = ["JpMarketRules", "MarketRules", "PriceRounding", "rules_for"]


class MarketRules(ABC):
    """1つの市場の取引制約。"""

    market: Market

    @property
    def currency(self) -> str:
        return self.market.currency

    @property
    @abstractmethod
    def default_lot_size(self) -> Decimal:
        """売買単位。"""

    @property
    @abstractmethod
    def blocks_same_day_sale(self) -> bool:
        """当日買い付けた銘柄の当日売却を止めるか（差金決済の回避）。"""

    @abstractmethod
    def snap_to_tick(
        self,
        price: Decimal,
        side: Side,
        *,
        rounding: PriceRounding = PriceRounding.CONSERVATIVE,
        symbol: str | None = None,
    ) -> Decimal:
        """指値を有効な呼値に乗せる。"""

    @abstractmethod
    def is_within_price_limit(self, price: Decimal, base_price: Decimal) -> bool:
        """値幅制限（ストップ高・安）の内側か。制限の無い市場は常に True。"""

    def round_to_lot(self, quantity: Decimal, lot_size: Decimal | None = None) -> Decimal:
        """売買単位に切り捨てる。"""
        return jp_rules.round_to_lot(quantity, lot_size or self.default_lot_size)

    def __repr__(self) -> str:
        return f"<{type(self).__name__} market={self.market.value}>"


class JpMarketRules(MarketRules):
    """東証。表の実体は :mod:`wbcore.domain.jp_rules`。"""

    market = Market.JP

    def __init__(self, topix500: set[str] | None = None) -> None:
        self.topix500 = set(topix500 or ())

    @property
    def default_lot_size(self) -> Decimal:
        return jp_rules.DEFAULT_LOT_SIZE

    @property
    def blocks_same_day_sale(self) -> bool:
        return True

    def snap_to_tick(
        self,
        price: Decimal,
        side: Side,
        *,
        rounding: PriceRounding = PriceRounding.CONSERVATIVE,
        symbol: str | None = None,
    ) -> Decimal:
        return jp_rules.snap_to_tick(
            price, side, topix500=symbol in self.topix500, rounding=rounding
        )

    def is_within_price_limit(self, price: Decimal, base_price: Decimal) -> bool:
        return jp_rules.is_within_price_limit(price, base_price)


def rules_for(market: Market, *, topix500: set[str] | None = None) -> MarketRules:
    """市場の識別子から取引ルールを組み立てる。売買できるのは東証だけ。"""
    if market is Market.JP:
        return JpMarketRules(topix500)
    raise ValueError(f"{market.value} 市場は売買に対応していません（日本株のみ）")
