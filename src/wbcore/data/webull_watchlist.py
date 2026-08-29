"""Webull のマイウォッチリストを読む。

用途は「アプリで気になった銘柄をウォッチリストに入れておけば、収集用の
銘柄リスト（``config/collect/universe.txt``）に流し込める」こと。
売買の判断には使わない。

接続先について:
    ウォッチリストの API は ``/openapi/market-data/watchlist/...`` だが、
    UAT では市場データ用ホスト（``data-api.uat...``）が SDK の初期化で
    接続を切り、**取引用ホスト**（``jp-openapi-alb.uat...``）経由なら通る
    ことを実機で確認した（2026-08-29、HTTP 200）。本番も同じ構成と仮定して
    取引用ホストに繋ぐ。違えば :meth:`WebullWatchlists.connect` の
    ``endpoint`` を変える。

応答の項目名は SDK の docstring（``watchlist_id`` / ``name`` /
``symbol`` / ``exchange_code``）に合わせてあるが、銘柄一覧の端点は
UAT の公開口座にリストが無く未検証。snake_case / camelCase の両方と、
``{"data": [...]}`` 包みを受け付けるようにしてある。
"""

from __future__ import annotations

import datetime as dt
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, ClassVar, Self

from wbcore.credentials import ENDPOINTS, Credentials, Environment, load_credentials
from wbcore.domain.models import Market
from wbcore.logging import get_logger, harden_third_party_logging, register_secret

log = get_logger(__name__)


@dataclass(frozen=True, slots=True)
class WatchlistItem:
    symbol: str
    name: str = ""
    exchange: str = ""
    instrument_id: str = ""


@dataclass(frozen=True, slots=True)
class Watchlist:
    id: str
    name: str
    sort: int = 0
    items: list[WatchlistItem] = field(default_factory=list)

    @property
    def symbols(self) -> list[str]:
        return [item.symbol for item in self.items]


def _get(row: dict[str, Any], *keys: str, default: str = "") -> str:
    """snake_case と camelCase のどちらでも引く。"""
    for key in keys:
        if key in row and row[key] is not None:
            return str(row[key])
    return default


def _unwrap(payload: Any) -> list[dict[str, Any]]:
    """``[...]`` / ``{"data": [...]}`` / ``{"instruments": [...]}`` を配列に揃える。"""
    if isinstance(payload, dict):
        for key in ("data", "instruments", "watchlists", "result", "list"):
            if isinstance(payload.get(key), list):
                return [r for r in payload[key] if isinstance(r, dict)]
        return []
    if isinstance(payload, list):
        return [r for r in payload if isinstance(r, dict)]
    return []


def parse_watchlists(payload: Any) -> list[Watchlist]:
    """一覧の応答 → :class:`Watchlist`（中身は空）。"""
    out = []
    for row in _unwrap(payload):
        list_id = _get(row, "watchlist_id", "watchlistId", "id")
        if not list_id:
            continue
        sort_text = _get(row, "sort", "sort_order", "sortOrder", default="0")
        out.append(Watchlist(list_id, _get(row, "name"), int(float(sort_text or 0))))
    return out


def parse_instruments(payload: Any) -> list[WatchlistItem]:
    """銘柄一覧の応答 → :class:`WatchlistItem`。"""
    out = []
    for row in _unwrap(payload):
        symbol = _get(row, "symbol", "ticker", "code")
        if not symbol:
            continue
        out.append(
            WatchlistItem(
                symbol=symbol,
                name=_get(row, "name", "instrument_name", "instrumentName"),
                exchange=_get(row, "exchange_code", "exchangeCode", "exchange"),
                instrument_id=_get(row, "instrument_id", "instrumentId"),
            )
        )
    return out


#: 取引所コード → 市場。実機の値が分かり次第ここに足す。
_JP_EXCHANGES = frozenset({"TSE", "TYO", "JPX", "XJPX", "XTKS", "TKS", "SPR", "NGO", "FKA"})
_US_EXCHANGES = frozenset(
    {
        "NASDAQ",
        "NYSE",
        "AMEX",
        "ARCA",
        "NYSEARCA",
        "BATS",
        "XNAS",
        "XNYS",
        "XASE",
        "NMS",
        "NGM",
        "NCM",
    }
)


def market_of(item: WatchlistItem) -> Market | None:
    """銘柄がどの市場のものか。

    取引所コードで決め、未知のコードなら銘柄コードの形で推定する
    （東証は数字始まりの 4 桁英数字、米国は英字）。どちらでもなければ None。
    """
    code = item.exchange.upper()
    if code in _JP_EXCHANGES:
        return Market.JP
    if code in _US_EXCHANGES:
        return Market.US
    symbol = item.symbol.upper().removesuffix(".T")
    if re.fullmatch(r"[0-9][0-9A-Z]{3}", symbol):
        return Market.JP
    if re.fullmatch(r"[A-Z]{1,5}([.\-][A-Z]{1,2})?", symbol):
        return Market.US
    return None


class WebullWatchlists:
    """ウォッチリストの読み取り。書き込みはしない（アプリ側の操作を上書きしないため）。"""

    credential_namespace: ClassVar[str] = "WBJP"

    def __init__(self, credentials: Credentials, env: Environment, endpoint: str) -> None:
        self._credentials = credentials
        self._env = env
        self._endpoint = endpoint
        self._client: Any = None
        register_secret(credentials.app_key, credentials.app_secret, credentials.account_id)

    @classmethod
    def connect(cls, env: Environment, *, endpoint: str | None = None) -> Self:
        credentials = load_credentials(env, namespace=cls.credential_namespace)
        return cls(credentials, env, endpoint or ENDPOINTS[env].trade)

    @property
    def client(self) -> Any:
        if self._client is None:
            from webull.core.client import ApiClient
            from webull.data.data_client import DataClient

            harden_third_party_logging()
            api = ApiClient(
                self._credentials.app_key, self._credentials.app_secret, self._env.value
            )
            api.add_endpoint(self._env.value, self._endpoint)
            self._client = DataClient(api)
            harden_third_party_logging()
        return self._client

    @staticmethod
    def _body(response: Any) -> Any:
        status = getattr(response, "status_code", 200)
        if status != 200:
            raise RuntimeError(f"ウォッチリスト API がエラーを返しました (HTTP {status})")
        return response.json() if hasattr(response, "json") else response

    def lists(self, *, with_items: bool = True) -> list[Watchlist]:
        """全ウォッチリスト。``with_items`` なら中の銘柄も読む（リストごとに 1 リクエスト）。"""
        lists = parse_watchlists(self._body(self.client.watchlist.get_watchlist()))
        if not with_items:
            return lists
        return [Watchlist(w.id, w.name, w.sort, self.instruments(w.id)) for w in lists]

    def instruments(self, watchlist_id: str) -> list[WatchlistItem]:
        return parse_instruments(self._body(self.client.watchlist.get_instruments(watchlist_id)))


def write_universe(
    path: Path,
    items: list[WatchlistItem],
    *,
    source: str,
    merge_with: Path | None = None,
) -> list[str]:
    """ウォッチリストの銘柄を ``universe.txt`` の形式で書く。

    Args:
        merge_with: 既存のファイル。渡すとそこにある銘柄を残し、新しい銘柄を足す
            （並びは既存 → 新規）。渡さなければ上書き。

    Returns:
        書いた銘柄（順序どおり）。
    """
    from wbjp.config import read_symbols_file  # 循環を避けるため遅延 import

    existing: list[str] = []
    if merge_with is not None and merge_with.is_file():
        existing = read_symbols_file(merge_with)
    fresh = [i for i in items if i.symbol not in existing]
    lines = [
        f"# Webull のウォッチリスト「{source}」から {dt.date.today()} に書き出し",
        "# 1行1銘柄、# 以降はコメント。手で書き足してよい",
        "",
        *existing,
        *(f"{i.symbol}{f'    # {i.name}' if i.name else ''}" for i in fresh),
    ]
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return existing + [i.symbol for i in fresh]
