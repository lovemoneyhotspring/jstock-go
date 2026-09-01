"""9:00 の気配・現在値の取得元。

**Webull OpenAPI の市場データは公式ドキュメント上 US 株のみ**（README「設計の前提」）。
日本株のスナップショットが返るかは実機でしか分からないので、取得元を差し替え可能に
しておく。``daytrade quotes 7203 9984`` で疎通を確かめる。

- ``webull``: 市場データ API のスナップショット（``category=JP_STOCK`` を試す）
- ``yfinance``: Yahoo Finance。東証は **20 分遅れ**なので寄付の判断には使えない。
  ``delayed=True`` を付けて返し、``open`` は既定で拒否する（検証・dry-run 用）
- ``csv``: ``symbol,price[,at]`` のファイル。手で入れた気配や、別経路で取った値を流す
"""

from __future__ import annotations

import csv
import datetime as dt
import logging
from collections.abc import Iterable
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any, ClassVar, Protocol

from daytrade.select import Quote
from wbcore.clock import ensure_utc, now_utc
from wbcore.credentials import ENDPOINTS, Credentials, Environment, load_credentials
from wbcore.logging import (
    get_logger,
    harden_third_party_logging,
    register_secret,
    suppress_sdk_own_logging,
)

log = get_logger(__name__)


class QuoteError(RuntimeError):
    """気配を取れなかった。"""


class QuoteSource(Protocol):
    @property
    def name(self) -> str: ...

    def fetch(self, symbols: Iterable[str]) -> dict[str, Quote]:
        """銘柄 → 気配。取れなかった銘柄は含めない。"""


def _decimal(value: Any) -> Decimal | None:
    if value in (None, "", "null"):
        return None
    try:
        result = Decimal(str(value))
    except InvalidOperation:
        return None
    return result if result > 0 else None


def _batches(items: list[str], size: int) -> Iterable[list[str]]:
    for start in range(0, len(items), size):
        yield items[start : start + size]


class WebullQuotes:
    """Webull 市場データ API のスナップショット。1 リクエスト最大 100 銘柄。"""

    name: ClassVar[str] = "webull"
    credential_namespace: ClassVar[str] = "WBJP"
    #: 日本株の category。銘柄マスタでは通る（実測）。スナップショットで通るかは未確認。
    category: ClassVar[str] = "JP_STOCK"
    batch_size: ClassVar[int] = 100

    def __init__(self, credentials: Credentials, env: Environment, endpoint: str) -> None:
        self._credentials = credentials
        self._env = env
        self._endpoint = endpoint
        self._client: Any = None
        register_secret(credentials.app_key, credentials.app_secret, credentials.account_id)

    @classmethod
    def connect(cls, env: Environment) -> WebullQuotes:
        credentials = load_credentials(env, namespace=cls.credential_namespace)
        return cls(credentials, env, ENDPOINTS[env].market_data)

    @property
    def client(self) -> Any:
        if self._client is None:
            from webull.core.client import ApiClient
            from webull.data.data_client import DataClient

            harden_third_party_logging()
            api_client = ApiClient(
                self._credentials.app_key, self._credentials.app_secret, self._env.value
            )
            api_client.add_endpoint(self._env.value, self._endpoint)
            suppress_sdk_own_logging(api_client)
            self._client = DataClient(api_client)
            harden_third_party_logging()
        return self._client

    def fetch(self, symbols: Iterable[str]) -> dict[str, Quote]:
        wanted = list(dict.fromkeys(s for s in symbols if s))
        found: dict[str, Quote] = {}
        for batch in _batches(wanted, self.batch_size):
            try:
                response = self.client.market_data.get_snapshot(batch, self.category)
            except Exception as exc:
                raise QuoteError(f"Webull のスナップショット取得に失敗: {exc}") from exc
            status = getattr(response, "status_code", 200)
            if status != 200:
                text = getattr(response, "text", "")
                raise QuoteError(f"Webull のスナップショット取得に失敗 (HTTP {status}): {text}")
            payload = response.json() if hasattr(response, "json") else response
            for row in parse_snapshot(payload, source=self.name):
                found[row.symbol] = row
        return found


def parse_snapshot(payload: Any, *, source: str = "webull") -> list[Quote]:
    """スナップショットの応答（``symbol`` / ``price`` / ``open`` / ``last_trade_time`` …）を読む。

    ``price`` が無ければ ``open``、それも無ければ ``bid``/``ask`` の中値。
    時刻はミリ秒の epoch（無ければ現在）。
    """
    rows = payload if isinstance(payload, list) else (payload or {}).get("data") or []
    quotes: list[Quote] = []
    for row in rows:
        if not isinstance(row, dict):
            continue
        symbol = str(row.get("symbol") or "")
        price = _decimal(row.get("price")) or _decimal(row.get("open"))
        if price is None:
            bid, ask = _decimal(row.get("bid")), _decimal(row.get("ask"))
            if bid and ask:
                price = (bid + ask) / 2
        if not symbol or price is None:
            continue
        stamp = row.get("last_trade_time") or row.get("timestamp")
        at = now_utc()
        if stamp not in (None, ""):
            try:
                millis = int(stamp)
                at = dt.datetime.fromtimestamp(millis / 1000, tz=dt.UTC)
            except TypeError, ValueError, OverflowError:
                pass
        quotes.append(Quote(symbol=symbol, price=price, at=at, source=source))
    return quotes


class YFinanceQuotes:
    """Yahoo Finance（20 分遅れ）。``delayed=True`` を付けて返す。検証・dry-run 用。"""

    name: ClassVar[str] = "yfinance"

    def fetch(self, symbols: Iterable[str]) -> dict[str, Quote]:
        import yfinance as yf

        wanted = list(dict.fromkeys(s for s in symbols if s))
        if not wanted:
            return {}
        tickers = [f"{s}.T" for s in wanted]
        # yfinance は取れない銘柄ごとに error を吐く（上場廃止など）。こちらで missing として
        # 数えるので、code の無い error でログを埋めないよう黙らせる
        logging.getLogger("yfinance").setLevel(logging.CRITICAL)
        try:
            frame = yf.download(
                tickers, period="1d", interval="1m", group_by="ticker", progress=False, threads=True
            )
        except Exception as exc:
            raise QuoteError(f"yfinance からの取得に失敗: {exc}") from exc
        found: dict[str, Quote] = {}
        for symbol, ticker in zip(wanted, tickers, strict=True):
            try:
                series = frame[ticker]["Close"] if len(tickers) > 1 else frame["Close"]
            except KeyError, TypeError:
                continue
            series = series.dropna()
            if series.empty:
                continue
            price = _decimal(series.iloc[-1])
            if price is None:
                continue
            stamp = series.index[-1].to_pydatetime()
            at = ensure_utc(stamp)
            found[symbol] = Quote(symbol=symbol, price=price, at=at, source=self.name, delayed=True)
        return found


class CsvQuotes:
    """``symbol,price[,at]`` の CSV。``at`` は ISO 8601（無ければ今）。"""

    name: ClassVar[str] = "csv"

    def __init__(self, path: Path) -> None:
        self.path = Path(path)

    def fetch(self, symbols: Iterable[str]) -> dict[str, Quote]:
        wanted = set(symbols)
        if not self.path.is_file():
            raise QuoteError(f"気配の CSV がありません: {self.path}")
        found: dict[str, Quote] = {}
        with self.path.open(encoding="utf-8", newline="") as handle:
            for row in csv.DictReader(handle):
                symbol = (row.get("symbol") or "").strip()
                price = _decimal(row.get("price"))
                if symbol not in wanted or price is None:
                    continue
                raw_at = (row.get("at") or "").strip()
                at = ensure_utc(dt.datetime.fromisoformat(raw_at)) if raw_at else now_utc()
                found[symbol] = Quote(symbol=symbol, price=price, at=at, source=self.name)
        return found


class TachibanaQuotes:
    """立花証券 e支店 API の時価問合（``CLMMfdsGetMarketPrice``、1 リクエスト最大 120 銘柄）。

    寄付後は始値（``pDOP``）、無ければ現在値（``pDPP``）。ブローカーの接続（ログイン・
    仮想URL）をそのまま使う。
    """

    name: ClassVar[str] = "tachibana"

    def __init__(self, broker: Any) -> None:
        self._broker = broker

    @classmethod
    def connect(cls, env: Environment) -> TachibanaQuotes:
        from wbcore.broker.tachibana import TachibanaBroker
        from wbcore.domain.models import Market

        return cls(TachibanaBroker.connect(env, market=Market.JP))

    def fetch(self, symbols: Iterable[str]) -> dict[str, Quote]:
        from wbcore.broker.base import BrokerError

        wanted = list(dict.fromkeys(s for s in symbols if s))
        try:
            rows = self._broker.market_prices(wanted)
        except BrokerError as exc:
            raise QuoteError(f"立花証券の時価取得に失敗: {exc}") from exc
        found: dict[str, Quote] = {}
        for symbol, row in rows.items():
            price = _decimal(row.get("open")) or _decimal(row.get("last"))
            if price is None:
                continue
            found[symbol] = Quote(symbol=symbol, price=price, at=row["at"], source=self.name)
        return found


def quote_source(name: str, env: Environment, *, quote_file: Path | None = None) -> QuoteSource:
    """設定の名前から取得元を組み立てる。"""
    if name == "webull":
        return WebullQuotes.connect(env)
    if name == "tachibana":
        return TachibanaQuotes.connect(env)
    if name == "yfinance":
        return YFinanceQuotes()
    if name == "csv":
        if quote_file is None:
            raise ValueError('quote_source = "csv" には quote_file が必要です')
        return CsvQuotes(quote_file)
    raise ValueError(f"未知の quote_source: {name!r}（webull / tachibana / yfinance / csv）")
