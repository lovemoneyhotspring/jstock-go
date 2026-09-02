"""FRED（セントルイス連銀）から米国の指数を取る（日足の終値のみ）。

**用途**: 東証の判断に使う米国の材料——デイトレの「前夜の S&P500 の騰落・VIX」
（:mod:`daytrade.usmarket`）と、積立の倍率判定に使う指数（``signal_symbol``）。
米国株を売買するためのものではない。

**なぜ FRED か**: 公式・無料・API キー不要で、``fredgraph.csv`` に ``id`` と期間を
付けるだけで CSV が返る。依存は標準の :mod:`csv` と :mod:`requests` だけで、
pandas は使わない。

制約:
    - 取れるのは **終値だけ**。四本値は無いので ``open`` / ``high`` / ``low`` は
      終値で埋め、``volume`` は 0 にする。日足の終値しか見ない判断（騰落率・
      移動平均・高値からの下落率）には十分だが、値幅を使う指標には使えない
    - ``SP500`` は直近 10 年ぶんしか公開されない（S&P 社のライセンス）。
      ``NASDAQCOM`` / ``VIXCLS`` / ``DJIA`` は全期間
    - 休場日は行が無く、欠損日は ``.`` で来る（落とす）

銘柄コードは従来どおり ``^GSPC`` のような表記で受け、:data:`SERIES` で FRED の
系列 ID に読み替える。設定ファイルを書き換えずに済ませるため。
"""

from __future__ import annotations

import csv
import datetime as dt
import io
from typing import Any, ClassVar, Self

import polars as pl
import requests

from wbcore.credentials import Environment
from wbcore.data.provider import BAR_SCHEMA, MarketDataError, MarketDataProvider, normalize_bars
from wbcore.domain.models import Market
from wbcore.logging import get_logger

log = get_logger(__name__)

BASE_URL = "https://fred.stlouisfed.org/graph/fredgraph.csv"

#: 銘柄コード → FRED の系列 ID。
SERIES: dict[str, str] = {
    "^GSPC": "SP500",  # S&P 500（直近 10 年のみ）
    "^IXIC": "NASDAQCOM",  # NASDAQ 総合
    "^NDX": "NASDAQ100",  # NASDAQ 100
    "^DJI": "DJIA",  # ダウ平均
    "^VIX": "VIXCLS",  # VIX（終値）
}


def series_id(symbol: str) -> str:
    """銘柄コードを FRED の系列 ID にする。表に無ければ FRED の ID をそのまま受ける。"""
    key = symbol.strip()
    if key in SERIES:
        return SERIES[key]
    if key.startswith("^"):
        raise MarketDataError(f"FRED に対応していない指数です: {symbol}（対応: {sorted(SERIES)}）")
    return key


def fetch_closes(
    series: str,
    start: dt.date,
    end: dt.date,
    *,
    session: Any | None = None,
    timeout: int = 30,
) -> pl.DataFrame:
    """1 系列の終値を取る。``date`` / ``close`` の 2 列、日付昇順。欠損日（``.``）は落とす。

    Raises:
        MarketDataError: 通信に失敗した、または系列が無い（404）とき。
    """
    http = session or requests
    try:
        response = http.get(
            BASE_URL,
            params={"id": series, "cosd": start.isoformat(), "coed": end.isoformat()},
            timeout=timeout,
        )
    except requests.RequestException as exc:
        raise MarketDataError(f"FRED からの取得に失敗しました（{series}）: {exc}") from exc
    if response.status_code == 404:
        raise MarketDataError(f"FRED に系列がありません: {series}")
    if response.status_code != 200:
        raise MarketDataError(f"FRED からの取得に失敗しました（{series}）: HTTP {response.status_code}")
    return parse_csv(response.text)


def parse_csv(text: str) -> pl.DataFrame:
    """``observation_date,<ID>`` 形式の CSV を ``date`` / ``close`` にする。"""
    dates: list[dt.date] = []
    closes: list[float] = []
    reader = csv.reader(io.StringIO(text))
    header = next(reader, None)
    if not header or len(header) < 2 or header[0] != "observation_date":
        raise MarketDataError(f"FRED の応答が CSV ではありません: {text[:80]!r}")
    for row in reader:
        if len(row) < 2 or row[1] in ("", "."):
            continue
        try:
            dates.append(dt.date.fromisoformat(row[0]))
            closes.append(float(row[1]))
        except ValueError:
            continue
    return pl.DataFrame(
        {"date": pl.Series(dates, dtype=pl.Date), "close": pl.Series(closes, dtype=pl.Float64)}
    ).sort("date")


def closes_to_bars(closes: pl.DataFrame) -> pl.DataFrame:
    """終値だけの表を正規の日足スキーマにする（四本値は終値で埋め、出来高は 0）。"""
    frame = closes.select(
        pl.col("date"),
        pl.col("close").alias("open"),
        pl.col("close").alias("high"),
        pl.col("close").alias("low"),
        pl.col("close"),
        pl.lit(0.0, dtype=pl.Float64).alias("volume"),
    )
    return normalize_bars(frame.cast(BAR_SCHEMA))  # type: ignore[arg-type]


class FredProvider(MarketDataProvider):
    """FRED 実装。米国の指数の日足（終値のみ）。"""

    name: ClassVar[str] = "fred"

    def __init__(
        self, *, market: Market = Market.US, session: Any | None = None, timeout: int = 30
    ) -> None:
        if market is not Market.US:
            raise MarketDataError(f"{self.name} は米国の指数（market = US）専用です: {market.value}")
        self.market = market
        self._session = session
        self._timeout = timeout

    @classmethod
    def connect(cls, env: Environment, *, market: Market) -> Self:
        """認証情報は要らない。環境（uat / prod）にも依存しない。"""
        return cls(market=market)

    def fetch_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
    ) -> dict[str, pl.DataFrame]:
        out: dict[str, pl.DataFrame] = {}
        for symbol in dict.fromkeys(s.strip() for s in symbols if s.strip()):
            closes = fetch_closes(
                series_id(symbol), start, end, session=self._session, timeout=self._timeout
            )
            if closes.height == 0:
                log.warning("FRED から足が返りませんでした", symbol=symbol, start=start, end=end)
                continue
            out[symbol] = closes_to_bars(closes)
        return out
