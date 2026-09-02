"""市場データ取得元の登録簿。設定の ``data_provider`` からクラスを引く。

ブローカー（:mod:`wbcore.broker.registry`）と同じ形。取得元を足すときは
:class:`~wbcore.data.provider.MarketDataProvider` を継承して ``name`` と
``connect`` を書き、ここに登録する。
"""

from __future__ import annotations

from wbcore.credentials import Environment
from wbcore.data.jquants_provider import JQuantsProvider
from wbcore.data.provider import MarketDataProvider
from wbcore.data.yfinance_provider import YFinanceProvider
from wbcore.domain.models import Market
from wbcore.registry import Registry

PROVIDERS = Registry[MarketDataProvider]("データソース")

register = PROVIDERS.register
available = PROVIDERS.available
get = PROVIDERS.get


def connect(name: str, env: Environment, *, market: Market) -> MarketDataProvider:
    """名前で選んだ取得元を組み立てる。

    Raises:
        ValueError: 未知の名前のとき。登録済みの名前を添える。
        MarketDataError: その取得元が市場に対応していないとき。
    """
    return get(name).connect(env, market=market)


#: 設定で取得元を省略したときの既定。日本株は公式の J-Quants、米国株は
#: J-Quants が対応しないので yfinance。
DEFAULT_PROVIDERS: dict[Market, str] = {
    Market.JP: JQuantsProvider.name,
    Market.US: YFinanceProvider.name,
}


def default_provider(market: Market) -> str:
    """市場ごとの既定の取得元の名前。"""
    return DEFAULT_PROVIDERS[market]


for _cls in (JQuantsProvider, YFinanceProvider):
    register(_cls)
