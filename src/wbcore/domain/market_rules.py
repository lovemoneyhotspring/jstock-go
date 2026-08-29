"""市場ごとの取引ルール。

エンジン（サイジング・リコンサイル・リスク）は「呼値に乗せる」「単元に
丸める」「値幅制限の内側か」「逆指値を置けるか」を**この抽象を通して**
問い合わせる。東証の具体的な表は :mod:`wbcore.domain.jp_rules` に、
米国市場の規則はこのモジュールの :class:`UsMarketRules` に置く。

新しい市場を足すときは :class:`MarketRules` を実装して
:func:`rules_for` に登録するだけで、エンジン側は触らない。
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from decimal import ROUND_DOWN, ROUND_HALF_UP, ROUND_UP, Decimal

from wbcore.domain import jp_rules
from wbcore.domain.jp_rules import PriceRounding
from wbcore.domain.models import Market, OrderType, Side

__all__ = ["JpMarketRules", "MarketRules", "PriceRounding", "UsMarketRules", "rules_for"]


class MarketRules(ABC):
    """1つの市場の取引制約。"""

    market: Market

    @property
    def currency(self) -> str:
        return self.market.currency

    @property
    @abstractmethod
    def default_lot_size(self) -> Decimal:
        """売買単位。米国株は1株。"""

    @property
    @abstractmethod
    def broker_stop_order_type(self) -> OrderType | None:
        """ブローカーに預けられる逆指値の種別。無ければ None。

        None の市場では :mod:`wbjp.risk.stops` がエンジン側で合成する。
        """

    @property
    def supports_broker_stops(self) -> bool:
        return self.broker_stop_order_type is not None

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
    def broker_stop_order_type(self) -> OrderType | None:
        # Webull JP の日本株 API は逆指値に対応していない（README 参照）
        return None

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


#: 米国株の呼値。1ドル以上は 0.01、1ドル未満は 0.0001（SEC Rule 612）。
US_TICK_ABOVE_ONE_DOLLAR = Decimal("0.01")
US_TICK_BELOW_ONE_DOLLAR = Decimal("0.0001")


class UsMarketRules(MarketRules):
    """米国市場（NYSE / NASDAQ）。

    東証との違い:
        - 1株単位で売買できる。単元株の丸めで見送りになることが無い。
        - 呼値は 1ドル以上で 0.01ドル。価格帯による段階は無い。
        - 制限値幅（ストップ高・安）は無い。代わりに個別銘柄の
          サーキットブレーカー（LULD）があるが、指値の受付は拒否されない。
        - 逆指値（STOP_LOSS）をブローカーに置ける。損切りを日足の判定に
          頼らなくてよいのが最大の利点。
        - 差金決済の規制は無い。現金口座には未決済資金の再利用制限
          （Good Faith Violation）があるが、日足スイングでは実質関係ない。
    """

    market = Market.US

    @property
    def default_lot_size(self) -> Decimal:
        return Decimal(1)

    @property
    def broker_stop_order_type(self) -> OrderType | None:
        return OrderType.STOP_LOSS

    @property
    def blocks_same_day_sale(self) -> bool:
        return False

    def snap_to_tick(
        self,
        price: Decimal,
        side: Side,
        *,
        rounding: PriceRounding = PriceRounding.CONSERVATIVE,
        symbol: str | None = None,
    ) -> Decimal:
        if price <= 0:
            raise ValueError(f"price は正の数: {price}")
        match rounding:
            case PriceRounding.NEAREST:
                mode = ROUND_HALF_UP
            case PriceRounding.CONSERVATIVE:
                mode = ROUND_DOWN if side is Side.BUY else ROUND_UP
            case PriceRounding.AGGRESSIVE:
                mode = ROUND_UP if side is Side.BUY else ROUND_DOWN

        tick = US_TICK_ABOVE_ONE_DOLLAR if price >= 1 else US_TICK_BELOW_ONE_DOLLAR
        snapped = (price / tick).quantize(Decimal(1), rounding=mode) * tick
        if snapped <= 0:
            snapped = tick
        # 1ドル未満から切り上げて 1ドル以上になった場合も 0.01 刻みに乗っている
        return jp_rules._tidy(snapped)

    def is_within_price_limit(self, price: Decimal, base_price: Decimal) -> bool:
        return True


def rules_for(market: Market, *, topix500: set[str] | None = None) -> MarketRules:
    """市場の識別子から取引ルールを組み立てる。"""
    match market:
        case Market.JP:
            return JpMarketRules(topix500)
        case Market.US:
            return UsMarketRules()
    raise ValueError(f"未対応の市場: {market!r}")  # pragma: no cover
