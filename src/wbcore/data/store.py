"""足データのローカル保存。

構成:
    日足は ``data/bars/{銘柄コード}.parquet``、日中足は
    ``data/bars/{間隔}/{銘柄コード}.parquet`` に1銘柄1ファイルで置く
    （日足を直下に置くのは、分足対応より前からある配置を壊さないため）。
    書き込みと戦略向けの読み出しは polars、横断的な集計・調査は
    DuckDB（Parquet を直接 SQL で読める）を使う。

    1つの :class:`BarStore` は1つの間隔だけを扱う。日足と5分足を両方
    使うなら、間隔ごとにインスタンスを作る。

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

from wbcore.data.provider import (
    BAR_SCHEMA,
    Interval,
    MarketDataProvider,
    empty_bars,
    normalize_bars,
)
from wbcore.logging import get_logger

log = get_logger(__name__)

#: ファイル名に使える銘柄コードの形（パストラバーサル対策も兼ねる）。
#: ``^`` を許すのは指数のティッカー（``^N225`` など）のため。区切り文字では
#: ないので経路の脱出には使えない。
_SAFE_SYMBOL = re.compile(r"^[A-Za-z0-9.^_-]+$")

#: 増分取得のとき、保存済み最終日から何日ぶん重ねて取り直すか。
#: 直近の足が後から訂正される場合に備えた重ね幅。
OVERLAP_DAYS = 5

#: 日中足の重ね幅。当日ぶんを取り直せば十分。
INTRADAY_OVERLAP_DAYS = 1


class BarStore:
    """Parquet による足データの保管庫。1インスタンス1間隔。"""

    def __init__(self, root: Path, interval: Interval = Interval.D1) -> None:
        self.root = Path(root)
        self.interval = interval

    @property
    def directory(self) -> Path:
        """この間隔のファイルを置く場所。"""
        return self.root if self.interval is Interval.D1 else self.root / self.interval.value

    @property
    def _key(self) -> str:
        return self.interval.time_column

    # -- 場所 ---------------------------------------------------------------

    def path_for(self, symbol: str) -> Path:
        """銘柄のファイルパス。

        Raises:
            ValueError: 銘柄コードにファイル名として使えない文字が
                含まれるとき（``../`` などを弾く）。
        """
        if not _SAFE_SYMBOL.match(symbol):
            raise ValueError(f"銘柄コードに使えない文字が含まれます: {symbol!r}")
        return self.directory / f"{symbol}.parquet"

    def symbols(self) -> list[str]:
        """保存済みの銘柄。"""
        if not self.directory.exists():
            return []
        return sorted(p.stem for p in self.directory.glob("*.parquet"))

    def has(self, symbol: str) -> bool:
        return self.path_for(symbol).exists()

    # -- 読み出し -----------------------------------------------------------

    def read(
        self,
        symbol: str,
        start: dt.date | None = None,
        end: dt.date | None = None,
    ) -> pl.DataFrame:
        """1銘柄の足を読む。未保存なら空フレームを返す。

        ``start`` / ``end`` は暦日。日中足でも日で切る。
        """
        path = self.path_for(symbol)
        if not path.exists():
            return empty_bars(self.interval)

        frame = pl.read_parquet(path)
        if "symbol" in frame.columns:
            frame = frame.drop("symbol")

        if start is not None:
            frame = frame.filter(pl.col("date") >= start)
        if end is not None:
            frame = frame.filter(pl.col("date") <= end)

        return frame.sort(self._key)

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

    def last_timestamp(self, symbol: str) -> dt.datetime | None:
        """保存済みの最終足の時刻（UTC）。日足では None。"""
        if not self.interval.is_intraday:
            return None
        frame = self.read(symbol)
        if frame.height == 0:
            return None
        value = frame["ts"].max()
        return value if isinstance(value, dt.datetime) else None

    # -- 書き込み -----------------------------------------------------------

    def write(self, symbol: str, frame: pl.DataFrame) -> None:
        """足を保存する（既存を置き換える）。"""
        self.directory.mkdir(parents=True, exist_ok=True)
        normalized = normalize_bars(frame, self.interval)
        # DuckDB で全銘柄を横断する際に必要なので銘柄列を持たせる
        normalized.with_columns(pl.lit(symbol).alias("symbol")).write_parquet(self.path_for(symbol))

    def upsert(self, symbol: str, frame: pl.DataFrame) -> int:
        """既存データに継ぎ足す。同じ時刻は新しい方で上書きする。

        Returns:
            保存後の総本数。
        """
        incoming = normalize_bars(frame, self.interval)
        if incoming.height == 0:
            return self.read(symbol).height

        existing = self.read(symbol)
        merged = (
            pl.concat([existing, incoming], how="vertical") if existing.height > 0 else incoming
        )
        # normalize_bars が重複を「後勝ち」で潰すので、新しい方が残る
        merged = normalize_bars(merged, self.interval)
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

        保存済みの最終日から :data:`OVERLAP_DAYS`（日中足は
        :data:`INTRADAY_OVERLAP_DAYS`）日ぶん重ねて取り直す
        （直近の足が後から訂正されることがあるため）。

        Args:
            force: True なら保存済みを無視して ``start`` から取り直す。

        Returns:
            銘柄 → 保存後の総本数。
        """
        overlap = INTRADAY_OVERLAP_DAYS if self.interval.is_intraday else OVERLAP_DAYS
        needed: dict[str, dt.date] = {}
        for symbol in symbols:
            if force:
                needed[symbol] = start
                continue
            last = self.last_date(symbol)
            if last is None:
                needed[symbol] = start
            elif last < end or self.interval.is_intraday:
                # 日中足は最終日が今日でも、その日の続きがまだ来る
                needed[symbol] = max(start, last - dt.timedelta(days=overlap))

        if not needed:
            log.info("足は最新です", symbols=len(symbols), interval=self.interval.value)
            return {s: self.read(s).height for s in symbols}

        # 取得開始日ごとにまとめて問い合わせ、通信回数を減らす
        by_start: dict[dt.date, list[str]] = {}
        for symbol, symbol_start in needed.items():
            by_start.setdefault(symbol_start, []).append(symbol)

        counts: dict[str, int] = {}
        for symbol_start, group in by_start.items():
            fetched = provider.fetch_bars(sorted(group), symbol_start, end, interval=self.interval)
            for symbol, frame in fetched.items():
                counts[symbol] = self.upsert(symbol, frame)
            for symbol in group:
                if symbol not in fetched:
                    log.warning("足を取得できませんでした", symbol=symbol)
                    counts[symbol] = self.read(symbol).height

        for symbol in symbols:
            counts.setdefault(symbol, self.read(symbol).height)

        log.info(
            "足を更新しました",
            updated=len(needed),
            total=len(symbols),
            interval=self.interval.value,
        )
        return counts

    # -- 分析 ---------------------------------------------------------------

    def query(self, sql: str) -> pl.DataFrame:
        """保存済みの全銘柄に SQL を投げる（DuckDB）。

        テーブル名 ``bars`` で全銘柄を横断できる。
        列は :func:`~wbcore.data.provider.bar_schema` に ``symbol`` を加えたもの。

        Example:
            >>> store.query(
            ...     "SELECT symbol, count(*) AS n FROM bars GROUP BY symbol"
            ... )  # doctest: +SKIP
        """
        import duckdb

        pattern = str(self.directory / "*.parquet")
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


__all__ = ["BAR_SCHEMA", "INTRADAY_OVERLAP_DAYS", "OVERLAP_DAYS", "BarStore"]
