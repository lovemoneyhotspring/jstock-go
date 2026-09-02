"""9:00 の気配・現在値の取得元。

日本株のリアルタイム気配を返す取得元は今のところ無い。取得元は差し替え可能に
してあるので、証券会社の市場データ API を使うならここに足す。
``daytrade quotes 7203 9984`` で疎通を確かめる。

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
from wbcore.credentials import Environment
from wbcore.logging import get_logger

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
    if name == "tachibana":
        return TachibanaQuotes.connect(env)
    if name == "yfinance":
        return YFinanceQuotes()
    if name == "csv":
        if quote_file is None:
            raise ValueError('quote_source = "csv" には quote_file が必要です')
        return CsvQuotes(quote_file)
    raise ValueError(f"未知の quote_source: {name!r}（tachibana / yfinance / csv）")
