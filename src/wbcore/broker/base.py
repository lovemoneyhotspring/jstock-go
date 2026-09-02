"""ブローカーの抽象。

同じインターフェースの裏に、実際の証券会社とシミュレータ
（:class:`~wbcore.broker.paper.PaperBroker`）を並べる。エンジンはどちらを
渡されたか知らないため、バックテスト・UAT・本番でまったく同じコードが動く。

**取引所を足すには**

1. :class:`Broker` を継承し、``name`` と :meth:`Broker.connect` を書く
2. :data:`wbcore.broker.registry.BROKERS` に登録する

設定の ``execution.broker = "<name>"`` で切り替わる。売買側（``wbjp``）も
積立側（``accum``）も :func:`wbcore.broker.registry.connect` 経由で
ブローカーを得るので、CLI には手を入れない。
"""

from __future__ import annotations

import datetime as dt
from abc import ABC, abstractmethod
from collections.abc import Callable, Iterable
from decimal import Decimal
from typing import ClassVar, Self

from wbcore.credentials import Environment
from wbcore.domain.models import (
    Balance,
    Market,
    Order,
    OrderAck,
    OrderPreview,
    OrderRequest,
    Position,
    TaxAccountType,
)


class BrokerError(RuntimeError):
    """ブローカー操作の失敗。"""


class OrderRejectedError(BrokerError):
    """注文が受け付けられなかった。"""


class InsufficientFundsError(OrderRejectedError):
    """買付余力が足りない。"""


class RateLimitExceededError(BrokerError):
    """レート制限に当たった。"""


class OrderNotFoundError(BrokerError):
    """照会した注文がブローカーに無い（404）。認証エラーや通信断とは区別する。"""


class Broker(ABC):
    """発注と口座照会の窓口。"""

    #: 設定ファイル（``execution.broker``）とログで使う識別子。サブクラスで必ず定義する。
    name: ClassVar[str] = ""

    @classmethod
    @abstractmethod
    def connect(
        cls,
        env: Environment,
        *,
        market: Market,
        tax_type: TaxAccountType = TaxAccountType.SPECIFIC,
        notify: Callable[[str], None] | None = None,
    ) -> Self:
        """環境と市場から、接続済みのブローカーを組み立てる。

        認証情報の解決・接続先の選択・キー失効の確認など「その証券会社
        固有の準備」をここに閉じ込める。呼び出し側は名前と環境しか知らない。

        Args:
            env: uat / prod。認証情報と接続先の切り替えに使う。
            market: この接続が扱う市場。1接続1市場。
            notify: 利用者に見せたい注意（共有テスト口座を使っている等）を渡す先。
                省略時はログに警告する。
        """

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
    def get_order(self, client_order_id: str, *, broker_order_id: str | None = None) -> Order | None:
        """注文1件の状態。見つからなければ None。

        ``broker_order_id`` は :class:`~wbcore.domain.models.OrderAck` で返した
        ブローカー側の注文IDを台帳から渡すためのヒント。client_order_id で照会できる
        ブローカーは無視してよい。できないブローカー（立花証券）は
        これが無いと別プロセスからの照会ができない。
        """

    def get_order_history(self, start: dt.date, end: dt.date) -> list[Order]:
        """期間内に出した注文（約定・失効を含む）。

        積立の台帳とブローカーの記録を突き合わせるために使う。
        対応していないブローカーは :class:`NotImplementedError`。
        """
        raise NotImplementedError(f"{self.name}: 注文履歴の照会に対応していません")

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

    def lot_sizes(self, symbols: Iterable[str]) -> dict[str, Decimal]:
        """銘柄ごとの売買単位（ブローカー表記の銘柄 → 単元株数）。

        分かる銘柄だけ返す（無い銘柄は含めない）。既定は何も分からない。
        ブローカーが銘柄マスタを持つなら上書きする。
        """
        return {}

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
