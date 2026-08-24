"""ブローカーの抽象。

同じインターフェースの裏に、実際の証券会社（:class:`~wbjp.broker.webull.WebullBroker`）
とシミュレータ（:class:`~wbjp.broker.paper.PaperBroker`）を並べる。
エンジンはどちらを渡されたか知らないため、バックテスト・UAT・本番で
まったく同じコードが動く。
"""

from __future__ import annotations

from abc import ABC, abstractmethod

from wbjp.domain.models import (
    Balance,
    Order,
    OrderAck,
    OrderPreview,
    OrderRequest,
    Position,
)


class BrokerError(RuntimeError):
    """ブローカー操作の失敗。"""


class OrderRejectedError(BrokerError):
    """注文が受け付けられなかった。"""


class InsufficientFundsError(OrderRejectedError):
    """買付余力が足りない。"""


class RateLimitExceededError(BrokerError):
    """レート制限に当たった。"""


class Broker(ABC):
    """発注と口座照会の窓口。"""

    #: ログ・設定用の識別子。
    name: str = ""

    @property
    @abstractmethod
    def account_id(self) -> str:
        """操作対象の口座ID。"""

    @abstractmethod
    def get_balance(self) -> Balance:
        """円建ての残高と買付余力。"""

    @abstractmethod
    def get_positions(self) -> list[Position]:
        """現在の建玉。"""

    @abstractmethod
    def get_open_orders(self) -> list[Order]:
        """未約定の注文。

        リコンサイル時に「発注済みだがまだ約定していない量」を
        建玉に足し込むために必要。これを見落とすと同じ注文を
        二重に出す。
        """

    @abstractmethod
    def get_order(self, client_order_id: str) -> Order | None:
        """注文1件の状態。見つからなければ None。"""

    @abstractmethod
    def preview(self, request: OrderRequest) -> OrderPreview:
        """発注前の見積り。約定代金と手数料を返す。"""

    @abstractmethod
    def place(self, request: OrderRequest) -> OrderAck:
        """発注する。

        Raises:
            OrderRejectedError: 拒否されたとき。
        """

    @abstractmethod
    def cancel(self, client_order_id: str) -> None:
        """注文を取り消す。"""

    def positions_by_symbol(self) -> dict[str, Position]:
        """建玉を銘柄で引ける形にする。

        同一銘柄が課税区分ごとに分かれて返る場合があるため、
        数量を合算して1件にまとめる。
        """
        merged: dict[str, Position] = {}
        for position in self.get_positions():
            existing = merged.get(position.symbol)
            if existing is None:
                merged[position.symbol] = position
                continue

            total_quantity = existing.quantity + position.quantity
            # 取得単価は数量による加重平均
            cost = (
                (existing.cost_price * existing.quantity + position.cost_price * position.quantity)
                / total_quantity
                if total_quantity
                else existing.cost_price
            )
            merged[position.symbol] = Position(
                symbol=position.symbol,
                quantity=total_quantity,
                available_quantity=existing.available_quantity + position.available_quantity,
                cost_price=cost,
                last_price=position.last_price,
                currency=position.currency,
                tax_type=existing.tax_type,
            )
        return merged

    def __repr__(self) -> str:
        return f"<{type(self).__name__} name={self.name!r}>"
