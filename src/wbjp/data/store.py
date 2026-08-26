"""足データのローカル保存。

構成:
    ``data/bars/{銘柄コード}.parquet`` に1銘柄1ファイルで置く。
    書き込みと戦略向けの読み出しは polars、横断的な集計・調査は
    DuckDB（Parquet を直接 SQL で読める）を使う。

なぜキャッシュが必須か:
    yfinance は短時間に連投すると IP 単位で遮断される。毎回フル取得
    していると、バックテストを数回回しただけで取得できなくなる。
    :meth:`BarStore.sync` は保存済みの最終日以降だけを取りに行くため、
    日々の運用では1日ぶんしか通信しない。
"""

from __future__ import annotations

import datetime as dt
import re
from pathlib import Path

import polars as pl

from wbjp.data.provider import BAR_SCHEMA, MarketDataProvider, empty_bars, normalize_bars
from wbjp.logging import get_logger

log = get_logger(__name__)

#: ファイル名に使える銘柄コードの形（パストラバーサル対策も兼ねる）。
#: ``^`` を許すのは指数のティッカー（``^N225`` など）のため。区切り文字では
#: ないので経路の脱出には使えない。
_SAFE_SYMBOL = re.compile(r"^[A-Za-z0-9.^_-]+$")

#: 増分取得のとき、保存済み最終日から何日ぶん重ねて取り直すか。
#: 直近の足が後から訂正される場合に備えた重ね幅。
OVERLAP_DAYS = 5


class BarStore:
    """Parquet による足データの保管庫。"""

    def __init__(self, root: Path) -> None:
        self.root = Path(root)

    # -- 場所 ---------------------------------------------------------------

    def path_for(self, symbol: str) -> Path:
        """銘柄のファイルパス。

        Raises:
            ValueError: 銘柄コードにファイル名として使えない文字が
                含まれるとき（``../`` などを弾く）。
        """
        if not _SAFE_SYMBOL.match(symbol):
            raise ValueError(f"銘柄コードに使えない文字が含まれます: {symbol!r}")
        return self.root / f"{symbol}.parquet"

    def symbols(self) -> list[str]:
        """保存済みの銘柄。"""
        if not self.root.exists():
            return []
        return sorted(p.stem for p in self.root.glob("*.parquet"))

    def has(self, symbol: str) -> bool:
        return self.path_for(symbol).exists()

    # -- 読み出し -----------------------------------------------------------

    def read(
        self,
        symbol: str,
        start: dt.date | None = None,
        end: dt.date | None = None,
    ) -> pl.DataFrame:
        """1銘柄の足を読む。未保存なら空フレームを返す。"""
        path = self.path_for(symbol)
        if not path.exists():
            return empty_bars()

        frame = pl.read_parquet(path)
        if "symbol" in frame.columns:
            frame = frame.drop("symbol")

        if start is not None:
            frame = frame.filter(pl.col("date") >= start)
        if end is not None:
            frame = frame.filter(pl.col("date") <= end)

        return frame.sort("date")

    def read_many(
        self,
        symbols: list[str],
        start: dt.date | None = None,
        end: dt.date | None = None,
    ) -> dict[str, pl.DataFrame]:
        """複数銘柄をまとめて読む。空の銘柄は含めない。"""
        result = {}
        for symbol in symbols:
            frame = self.read(symbol, start, end)
            if frame.height > 0:
                result[symbol] = frame
        return result

    def last_date(self, symbol: str) -> dt.date | None:
        """保存済みの最終取引日。未保存なら None。"""
        frame = self.read(symbol)
        if frame.height == 0:
            return None
        value = frame["date"].max()
        return value if isinstance(value, dt.date) else None

    # -- 書き込み -----------------------------------------------------------

    def write(self, symbol: str, frame: pl.DataFrame) -> None:
        """足を保存する（既存を置き換える）。"""
        self.root.mkdir(parents=True, exist_ok=True)
        normalized = normalize_bars(frame)
        # DuckDB で全銘柄を横断する際に必要なので銘柄列を持たせる
        normalized.with_columns(pl.lit(symbol).alias("symbol")).write_parquet(self.path_for(symbol))

    def upsert(self, symbol: str, frame: pl.DataFrame) -> int:
        """既存データに継ぎ足す。同じ日付は新しい方で上書きする。

        Returns:
            保存後の総本数。
        """
        incoming = normalize_bars(frame)
        if incoming.height == 0:
            return self.read(symbol).height

        existing = self.read(symbol)
        merged = (
            pl.concat([existing, incoming], how="vertical") if existing.height > 0 else incoming
        )
        # normalize_bars が日付重複を「後勝ち」で潰すので、新しい方が残る
        merged = normalize_bars(merged)
        self.write(symbol, merged)
        return merged.height

    # -- 取得 ---------------------------------------------------------------

    def sync(
        self,
        provider: MarketDataProvider,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
        *,
        force: bool = False,
    ) -> dict[str, int]:
        """不足している足だけを取得して保存する。

        保存済みの最終日から :data:`OVERLAP_DAYS` 日ぶん重ねて取り直す
        （直近の足が後から訂正されることがあるため）。

        Args:
            force: True なら保存済みを無視して ``start`` から取り直す。

        Returns:
            銘柄 → 保存後の総本数。
        """
        needed: dict[str, dt.date] = {}
        for symbol in symbols:
            if force:
                needed[symbol] = start
                continue
            last = self.last_date(symbol)
            if last is None:
                needed[symbol] = start
            elif last < end:
                needed[symbol] = max(start, last - dt.timedelta(days=OVERLAP_DAYS))

        if not needed:
            log.info("足は最新です", symbols=len(symbols))
            return {s: self.read(s).height for s in symbols}

        # 取得開始日ごとにまとめて問い合わせ、通信回数を減らす
        by_start: dict[dt.date, list[str]] = {}
        for symbol, symbol_start in needed.items():
            by_start.setdefault(symbol_start, []).append(symbol)

        counts: dict[str, int] = {}
        for symbol_start, group in by_start.items():
            fetched = provider.fetch_daily_bars(sorted(group), symbol_start, end)
            for symbol, frame in fetched.items():
                counts[symbol] = self.upsert(symbol, frame)
            for symbol in group:
                if symbol not in fetched:
                    log.warning("足を取得できませんでした", symbol=symbol)
                    counts[symbol] = self.read(symbol).height

        for symbol in symbols:
            counts.setdefault(symbol, self.read(symbol).height)

        log.info("足を更新しました", updated=len(needed), total=len(symbols))
        return counts

    # -- 分析 ---------------------------------------------------------------

    def query(self, sql: str) -> pl.DataFrame:
        """保存済みの全銘柄に SQL を投げる（DuckDB）。

        テーブル名 ``bars`` で全銘柄を横断できる。
        列は :data:`~wbjp.data.provider.BAR_SCHEMA` に ``symbol`` を加えたもの。

        Example:
            >>> store.query(
            ...     "SELECT symbol, count(*) AS n FROM bars GROUP BY symbol"
            ... )  # doctest: +SKIP
        """
        import duckdb

        pattern = str(self.root / "*.parquet")
        connection = duckdb.connect()
        try:
            if not self.symbols():
                return pl.DataFrame()
            connection.execute(f"CREATE VIEW bars AS SELECT * FROM read_parquet('{pattern}')")
            return connection.execute(sql).pl()
        finally:
            connection.close()

    def summary(self) -> pl.DataFrame:
        """保存状況の一覧。データの穴を見つけるのに使う。"""
        if not self.symbols():
            return pl.DataFrame(
                schema={"symbol": pl.String, "bars": pl.Int64, "first": pl.Date, "last": pl.Date}
            )
        return self.query(
            "SELECT symbol, count(*) AS bars, min(date) AS first, max(date) AS last "
            "FROM bars GROUP BY symbol ORDER BY symbol"
        )


__all__ = ["BAR_SCHEMA", "OVERLAP_DAYS", "BarStore"]
