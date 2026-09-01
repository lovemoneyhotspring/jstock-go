"""ブローカーの登録簿。設定の ``execution.broker`` からクラスを引く。

戦略の登録簿と同じ部品（:class:`wbcore.registry.Registry`）。取引所を
足すときは :class:`~wbcore.broker.base.Broker` を継承して ``name`` と
``connect`` を書き、ここに登録する。売買側・積立側の CLI はどちらも
:func:`connect` を通るので、切り替えは設定ファイルだけで済む。
"""

from __future__ import annotations

from collections.abc import Callable

from wbcore.broker.base import Broker
from wbcore.broker.paper import PaperBroker
from wbcore.broker.tachibana import TachibanaBroker
from wbcore.broker.webull import WebullBroker
from wbcore.credentials import Environment
from wbcore.domain.models import Market, TaxAccountType
from wbcore.registry import Registry

BROKERS = Registry[Broker]("ブローカー")

register = BROKERS.register
available = BROKERS.available
get = BROKERS.get


def connect(
    name: str,
    env: Environment,
    *,
    market: Market,
    tax_type: TaxAccountType = TaxAccountType.SPECIFIC,
    extended_hours: bool = False,
    notify: Callable[[str], None] | None = None,
) -> Broker:
    """名前で選んだブローカーに接続する。

    Raises:
        ValueError: 未知の名前のとき。登録済みの名前を添える。
    """
    return get(name).connect(
        env, market=market, tax_type=tax_type, extended_hours=extended_hours, notify=notify
    )


for _cls in (WebullBroker, TachibanaBroker, PaperBroker):
    register(_cls)
