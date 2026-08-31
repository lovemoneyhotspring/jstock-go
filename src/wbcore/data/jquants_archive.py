"""J-Quants（Standard）の全データをローカルに蓄積する。設計は ``docs/JQUANTS_ARCHIVE.md``。

方針（要約）:
    - **生のまま残す。** 列名は応答どおり、値はすべて文字列（日付列だけ ``Date``）。
      API は数値を文字列で返すことがあり、一括 CSV は全部文字列なので、
      型を揃えようとすると経路で食い違う。整形は読むとき（:func:`typed`）。
    - **端点 × 月の Parquet。** ``data/jquants/<端点>/<YYYY-MM>.parquet``。
    - **鍵で後勝ち。** 同じ鍵の行は新しい取り込みが勝つ。速報→確報、過誤訂正、
      取り直しがすべてこの規則で片付き、何度実行しても同じになる。
    - **台帳（SQLite）** に「端点・対象・取得時刻・件数・変化した行数」を残す。
      どこまで取れているか、欠けはどこかは台帳で答える。
"""

from __future__ import annotations

import datetime as dt
import gzip
import hashlib
import io
import json
import os
import sqlite3
from collections.abc import Iterable
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any
from zoneinfo import ZoneInfo

import polars as pl

from wbcore.clock import now_utc
from wbcore.data.provider import MarketDataError
from wbcore.logging import get_logger

log = get_logger(__name__)

JST = ZoneInfo("Asia/Tokyo")

#: 取引カレンダーの ``HolDiv`` で「株式の取引がある日」とみなす値。
#: 1 = 営業日、2 = 半日（大納会・大発会）。0 = 非営業日、3 = 祝日取引（先物のみ）。
TRADING_DAY_DIVISIONS = frozenset({"1", "2"})


class Mode(StrEnum):
    """増分の取り方。"""

    #: 1 日 1 リクエスト（``date=`` など日付の引数で全銘柄）。対象は暦日。
    DATE = "date"
    #: ``from`` / ``to`` の範囲で 1 リクエスト。対象は範囲の終端日。
    RANGE = "range"
    #: 引数無しで全件。対象は取得日。
    ALL = "all"


@dataclass(frozen=True, slots=True)
class Endpoint:
    """蓄積する端点 1 つぶんの定義。"""

    path: str
    #: 1 行を一意にする列。上書きの単位。
    key: tuple[str, ...]
    #: 月のファイルを切る日付列。鍵の先頭であることが多い。
    date_column: str
    mode: Mode = Mode.DATE
    #: 日付を渡す引数名（``date`` / ``disc_date``）。
    date_param: str = "date"
    #: その日ぶんが API に乗る時刻（JST）。これより前には取りに行かない。
    available_at: dt.time = dt.time(16, 30)
    #: 訂正に備え、対象日から何日ぶん取り直しを続けるか。
    settle_days: int = 5
    #: 取り直しの最短間隔（時間）。cron が 20 分おきでも 1 日 1 回に抑える。
    min_interval_hours: int = 20
    #: 取引日だけ対象にする（False なら平日すべて。EDINET の提出日など）。
    trading_days_only: bool = True
    #: 一括ダウンロード（``/bulk``）にあるか。
    bulk: bool = True
    #: ``RANGE`` のとき、何日ぶん重ねて取るか。
    range_days: int = 0
    #: 日付列以外にも日付として扱う列。
    extra_date_columns: tuple[str, ...] = ()

    @property
    def name(self) -> str:
        """ディレクトリ名。``/equities/bars/daily`` → ``equities_bars_daily``。"""
        return self.path.strip("/").replace("/", "_").replace("-", "_")

    @property
    def date_columns(self) -> tuple[str, ...]:
        return (self.date_column, *self.extra_date_columns)


# --------------------------------------------------------------------------
# 端点の目録（Standard プランで取れるもの）
# --------------------------------------------------------------------------

_T1630 = dt.time(16, 30)
_T1730 = dt.time(17, 30)
_T1800 = dt.time(18, 0)

ENDPOINTS: tuple[Endpoint, ...] = (
    Endpoint(
        "/markets/calendar",
        key=("Date",),
        date_column="Date",
        mode=Mode.ALL,
        available_at=dt.time(0, 0),
        settle_days=0,
        min_interval_hours=24 * 7,
        bulk=False,
    ),
    Endpoint(
        "/equities/master",
        key=("Date", "Code"),
        date_column="Date",
        available_at=_T1730,
        settle_days=1,
    ),
    Endpoint("/equities/bars/daily", key=("Date", "Code"), date_column="Date"),
    Endpoint("/indices/bars/daily", key=("Date", "Code"), date_column="Date"),
    Endpoint(
        "/indices/bars/daily/topix",
        key=("Date",),
        date_column="Date",
        mode=Mode.RANGE,
        range_days=10,
    ),
    Endpoint(
        "/fins/summary",
        key=("DiscDate", "DiscTime", "Code", "DiscNo"),
        date_column="DiscDate",
        available_at=_T1800,
        settle_days=2,
        extra_date_columns=("CurPerSt", "CurPerEn", "CurFYSt", "CurFYEn", "NxtFYSt", "NxtFYEn"),
    ),
    Endpoint(
        "/fins/earnings-date",
        key=("PubDate", "Code"),
        date_column="PubDate",
        available_at=dt.time(10, 5),
        settle_days=1,
        extra_date_columns=("SchDate",),
    ),
    Endpoint(
        "/equities/earnings-calendar",
        key=("Date", "Code"),
        date_column="Date",
        mode=Mode.ALL,
        available_at=dt.time(19, 0),
        settle_days=0,
        bulk=False,
    ),
    Endpoint(
        "/equities/investor-types",
        key=("PubDate", "StDate", "EnDate", "Section"),
        date_column="PubDate",
        mode=Mode.RANGE,
        available_at=_T1800,
        range_days=56,  # 過誤訂正は公表の翌営業日に反映される。8 週ぶん重ねる
        extra_date_columns=("StDate", "EnDate"),
    ),
    # 週次（金曜日付、第 2 営業日に公開）。API は date か code が必須で from/to
    # だけでは呼べない（400）ため、毎営業日 date= で叩く。残高の無い日は 0 行
    Endpoint(
        "/markets/margin-interest",
        key=("Date", "Code"),
        date_column="Date",
        settle_days=7,
    ),
    Endpoint(
        "/markets/margin-alert",
        key=("PubDate", "Code", "AppDate"),
        date_column="PubDate",
        extra_date_columns=("AppDate",),
    ),
    Endpoint("/markets/short-ratio", key=("Date", "S33"), date_column="Date"),
    Endpoint(
        "/markets/short-sale-report",
        key=("DiscDate", "CalcDate", "Code", "SSName", "FundName"),
        date_column="DiscDate",
        date_param="disc_date",
        available_at=_T1730,
        extra_date_columns=("CalcDate", "PrevRptDate"),
    ),
    Endpoint("/derivatives/bars/daily/options/225", key=("Date", "Code"), date_column="Date"),
    Endpoint(
        "/edinet/major-shareholders",
        key=("DocId",),
        date_column="SubDate",
        available_at=_T1800,
        settle_days=1,
        trading_days_only=False,
        bulk=False,
        extra_date_columns=("PerSt", "PerEn"),
    ),
    Endpoint(
        "/edinet/cross-shareholdings",
        key=("DocId",),
        date_column="SubDate",
        available_at=_T1800,
        settle_days=1,
        trading_days_only=False,
        bulk=False,
        extra_date_columns=("PerSt", "PerEn"),
    ),
    Endpoint(
        "/edinet/large-volume-shareholders",
        key=("DocId",),
        date_column="SubDate",
        available_at=_T1800,
        settle_days=1,
        trading_days_only=False,
        bulk=False,
    ),
)

BY_NAME: dict[str, Endpoint] = {e.name: e for e in ENDPOINTS}
BY_PATH: dict[str, Endpoint] = {e.path: e for e in ENDPOINTS}


def endpoint(name_or_path: str) -> Endpoint:
    """名前（``equities_bars_daily``）かパス（``/equities/bars/daily``）で引く。"""
    found = BY_NAME.get(name_or_path) or BY_PATH.get(name_or_path)
    if found is None:
        raise ValueError(f"未知の端点 {name_or_path!r}。利用可能: {sorted(BY_NAME)}")
    return found


# --------------------------------------------------------------------------
# 行の正規化（文字列化）
# --------------------------------------------------------------------------


def _stringify(value: Any) -> str | None:
    """値を文字列に。数値は表記を変えない（``1.0`` を ``1`` にしない）。"""
    if value is None:
        return None
    if isinstance(value, str):
        return value
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (int, float)):
        return repr(value) if isinstance(value, float) else str(value)
    return json.dumps(value, ensure_ascii=False, sort_keys=True)


def rows_to_frame(rows: Iterable[dict[str, Any]], ep: Endpoint) -> pl.DataFrame:
    """API の行（dict）を保存形にする。全列 String、日付列だけ Date。

    入れ子（EDINET の ``Hldrs`` など）は JSON 文字列で残す。
    """
    records = [{k: _stringify(v) for k, v in row.items()} for row in rows]
    if not records:
        return pl.DataFrame()
    columns: dict[str, list[str | None]] = {}
    for record in records:
        for name in record:
            columns.setdefault(name, [])
    for record in records:
        for name, values in columns.items():
            values.append(record.get(name))
    frame = pl.DataFrame(columns, schema={name: pl.String for name in columns})
    return _with_dates(frame, ep)


def csv_to_frame(payload: bytes, ep: Endpoint) -> pl.DataFrame:
    """一括ダウンロードの ``csv.gz`` を保存形にする。列名は API と同じ前提。"""
    raw = gzip.decompress(payload) if payload[:2] == b"\x1f\x8b" else payload
    frame = pl.read_csv(io.BytesIO(raw), infer_schema=False, encoding="utf8")
    frame = frame.with_columns(
        [pl.when(pl.col(c) == "").then(None).otherwise(pl.col(c)).alias(c) for c in frame.columns]
    )
    return _with_dates(frame, ep)


def _with_dates(frame: pl.DataFrame, ep: Endpoint) -> pl.DataFrame:
    exprs = []
    for name in ep.date_columns:
        if name in frame.columns:
            exprs.append(
                pl.col(name)
                .str.strip_chars()
                .str.replace_all("/", "-")
                .str.to_date("%Y-%m-%d", strict=False)
                .alias(name)
            )
    frame = frame.with_columns(exprs) if exprs else frame
    if ep.date_column not in frame.columns:
        raise ValueError(f"{ep.path} の応答に日付列 {ep.date_column} がありません: {frame.columns}")
    return frame


def typed(frame: pl.DataFrame | pl.LazyFrame, *, exclude: Iterable[str] = ()) -> pl.LazyFrame:
    """研究用に数値へ寄せる。数値に解釈できる列だけ Float64 にする。

    保存形は文字列なので、読むときにこれを通す。``Code`` のような
    「数字だけだが数値ではない」列は ``exclude`` で残す。
    """
    lazy = frame.lazy()
    schema = lazy.collect_schema()
    skip = {"Code", "S33", "S17", "DocId", "DiscNo", "Section", *exclude}
    sample = lazy.limit(2000).collect()
    exprs = []
    for name, dtype in schema.items():
        if dtype != pl.String or name in skip:
            continue
        column = sample[name].drop_nulls()
        if column.len() == 0:
            continue
        parsed = column.cast(pl.Float64, strict=False)
        if parsed.null_count() == 0:
            exprs.append(pl.col(name).cast(pl.Float64, strict=False).alias(name))
    return lazy.with_columns(exprs) if exprs else lazy


# --------------------------------------------------------------------------
# 台帳
# --------------------------------------------------------------------------

_SCHEMA = """
CREATE TABLE IF NOT EXISTS ingest (
  endpoint    TEXT NOT NULL,
  target      TEXT NOT NULL,
  source      TEXT NOT NULL,
  fetched_utc TEXT NOT NULL,
  rows        INTEGER NOT NULL,
  changed     INTEGER NOT NULL,
  digest      TEXT NOT NULL,
  run_id      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (endpoint, target, fetched_utc)
);
CREATE INDEX IF NOT EXISTS ingest_latest ON ingest (endpoint, target, fetched_utc DESC);
"""


@dataclass(frozen=True, slots=True)
class IngestRecord:
    endpoint: str
    target: str
    source: str
    fetched_utc: dt.datetime
    rows: int
    changed: int
    digest: str


class Ledger:
    """取り込みの記録。「いつ・何を・何件」だけを持つ。データ本体は Parquet。"""

    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._conn = sqlite3.connect(self.path)
        self._conn.executescript(_SCHEMA)

    def close(self) -> None:
        self._conn.close()

    def record(
        self,
        ep: Endpoint,
        target: str,
        *,
        source: str,
        rows: int,
        changed: int,
        digest: str,
        run_id: str = "",
        fetched: dt.datetime | None = None,
    ) -> None:
        moment = (fetched or now_utc()).isoformat()
        with self._conn:
            self._conn.execute(
                "INSERT OR REPLACE INTO ingest VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                (ep.path, target, source, moment, rows, changed, digest, run_id),
            )

    def last(self, ep: Endpoint, target: str) -> IngestRecord | None:
        row = self._conn.execute(
            "SELECT endpoint, target, source, fetched_utc, rows, changed, digest FROM ingest "
            "WHERE endpoint = ? AND target = ? ORDER BY fetched_utc DESC LIMIT 1",
            (ep.path, target),
        ).fetchone()
        return _record(row) if row else None

    def targets(self, ep: Endpoint) -> list[str]:
        """一度でも取った対象。"""
        rows = self._conn.execute(
            "SELECT DISTINCT target FROM ingest WHERE endpoint = ? ORDER BY target", (ep.path,)
        ).fetchall()
        return [r[0] for r in rows]

    def history(self, ep: Endpoint, *, limit: int = 50) -> list[IngestRecord]:
        rows = self._conn.execute(
            "SELECT endpoint, target, source, fetched_utc, rows, changed, digest FROM ingest "
            "WHERE endpoint = ? ORDER BY fetched_utc DESC LIMIT ?",
            (ep.path, limit),
        ).fetchall()
        return [_record(r) for r in rows]

    def summary(self) -> pl.DataFrame:
        rows = self._conn.execute(
            "SELECT endpoint, COUNT(*) AS fetches, MIN(target) AS first_target, "
            "MAX(target) AS last_target, MAX(fetched_utc) AS last_fetched, SUM(rows) AS rows "
            "FROM ingest GROUP BY endpoint ORDER BY endpoint"
        ).fetchall()
        return pl.DataFrame(
            rows,
            schema=["endpoint", "fetches", "first_target", "last_target", "last_fetched", "rows"],
            orient="row",
        )


def _record(row: tuple[Any, ...]) -> IngestRecord:
    return IngestRecord(
        endpoint=row[0],
        target=row[1],
        source=row[2],
        fetched_utc=dt.datetime.fromisoformat(row[3]),
        rows=int(row[4]),
        changed=int(row[5]),
        digest=row[6],
    )


# --------------------------------------------------------------------------
# Parquet の保管庫
# --------------------------------------------------------------------------


class Archive:
    """``data/jquants/`` の読み書き。1 端点 = 1 ディレクトリ、1 月 = 1 ファイル。"""

    def __init__(self, root: Path) -> None:
        self.root = Path(root)

    @property
    def ledger_path(self) -> Path:
        return self.root / "ledger.db"

    def raw_dir(self, ep: Endpoint) -> Path:
        """一括ダウンロードの CSV を残す場所（変換にバグがあっても取り直さずに済む保険）。"""
        return self.root / "_raw" / ep.name

    def directory(self, ep: Endpoint) -> Path:
        return self.root / ep.name

    def path_for(self, ep: Endpoint, month: str) -> Path:
        return self.directory(ep) / f"{month}.parquet"

    def months(self, ep: Endpoint) -> list[str]:
        directory = self.directory(ep)
        if not directory.is_dir():
            return []
        return sorted(p.stem for p in directory.glob("*.parquet"))

    # -- 読む ---------------------------------------------------------------

    def scan(self, ep: Endpoint) -> pl.LazyFrame:
        """端点の全期間。無ければ空。列は ``diagonal`` で合わせる（仕様変更で列が増えても読める）。"""
        paths = [self.path_for(ep, m) for m in self.months(ep)]
        if not paths:
            return pl.LazyFrame()
        return pl.concat([pl.scan_parquet(p) for p in paths], how="diagonal_relaxed")

    def read(
        self, ep: Endpoint, start: dt.date | None = None, end: dt.date | None = None
    ) -> pl.DataFrame:
        """期間で絞って読む。月ファイル単位で必要なものだけ開く。"""
        months = self.months(ep)
        if start:
            months = [m for m in months if m >= start.strftime("%Y-%m")]
        if end:
            months = [m for m in months if m <= end.strftime("%Y-%m")]
        if not months:
            return pl.DataFrame()
        lazy = pl.concat(
            [pl.scan_parquet(self.path_for(ep, m)) for m in months], how="diagonal_relaxed"
        )
        if start:
            lazy = lazy.filter(pl.col(ep.date_column) >= start)
        if end:
            lazy = lazy.filter(pl.col(ep.date_column) <= end)
        return lazy.collect()

    def dates(self, ep: Endpoint) -> list[dt.date]:
        """保存済みの日付（``date_column`` の値）。"""
        lazy = self.scan(ep)
        if ep.date_column not in lazy.collect_schema():
            return []
        return sorted(
            lazy.select(ep.date_column).unique().collect()[ep.date_column].drop_nulls().to_list()
        )

    # -- 書く ---------------------------------------------------------------

    def upsert(self, ep: Endpoint, frame: pl.DataFrame) -> int:
        """鍵で後勝ちに合流する。返り値は増えた・変わった行数。

        月ごとに: 既存を読む → 新しい行を後ろに足す → 鍵で最後を残す → 一時ファイルに
        書いて rename。途中で落ちても壊れたファイルは残らない。
        """
        if frame.height == 0:
            return 0
        missing = [k for k in ep.key if k not in frame.columns]
        if missing:
            raise ValueError(f"{ep.path} の行に鍵の列がありません: {missing}")
        frame = frame.filter(pl.col(ep.date_column).is_not_null())
        changed = 0
        months = frame.with_columns(pl.col(ep.date_column).dt.strftime("%Y-%m").alias("__month"))
        for (month,), part in months.group_by(["__month"], maintain_order=True):
            part = part.drop("__month")
            changed += self._upsert_month(ep, str(month), part)
        return changed

    def _upsert_month(self, ep: Endpoint, month: str, new: pl.DataFrame) -> int:
        path = self.path_for(ep, month)
        key = list(ep.key)
        new = new.unique(subset=key, keep="last", maintain_order=True)
        if path.exists():
            old = pl.read_parquet(path)
            merged = pl.concat([old, new], how="diagonal_relaxed")
            changed = _count_changed(old, new, key)
        else:
            merged = new
            changed = new.height
        merged = merged.unique(subset=key, keep="last", maintain_order=True).sort(key)
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(".parquet.tmp")
        merged.write_parquet(tmp, compression="zstd")
        os.replace(tmp, path)
        return changed


def _count_changed(old: pl.DataFrame, new: pl.DataFrame, key: list[str]) -> int:
    """新しい行のうち、既存に無いか内容が違うものの数。"""
    columns = [c for c in new.columns if c in old.columns]
    old_hash = old.select([*key, pl.struct(columns).hash().alias("__h")])
    new_hash = new.select([*key, pl.struct(columns).hash().alias("__h")])
    joined = new_hash.join(old_hash, on=key, how="left", suffix="_old")
    return int(
        joined.filter(pl.col("__h_old").is_null() | (pl.col("__h") != pl.col("__h_old"))).height
    )


def digest_of(frame: pl.DataFrame) -> str:
    """応答の内容のハッシュ。同じ内容の取り直しを台帳で見分ける。"""
    if frame.height == 0:
        return hashlib.sha256(b"").hexdigest()
    h = hashlib.sha256()
    for value in frame.sort(frame.columns).hash_rows().to_list():
        h.update(int(value).to_bytes(8, "little"))
    return h.hexdigest()


# --------------------------------------------------------------------------
# 取り込み
# --------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class Ingest:
    """1 回の取り込みの結果。"""

    endpoint: str
    target: str
    source: str
    rows: int
    changed: int


@dataclass(frozen=True, slots=True)
class Failure:
    """失敗した取り込み 1 つぶん。"""

    endpoint: str
    target: str
    error: str


@dataclass(slots=True)
class SyncResult:
    """1 回の ``sync`` の結果。"""

    ingests: list[Ingest] = field(default_factory=list)
    failures: list[Failure] = field(default_factory=list)


class Ingestor:
    """API / 一括ファイルから取り、保管庫と台帳に書く。"""

    def __init__(self, client: Any, archive: Archive, ledger: Ledger, *, run_id: str = "") -> None:
        self.client = client
        self.archive = archive
        self.ledger = ledger
        self.run_id = run_id

    # -- 1 回ぶん -----------------------------------------------------------

    def ingest(self, ep: Endpoint, target: str, params: dict[str, str]) -> Ingest:
        rows = self.client.get_all(ep.path, params)
        frame = rows_to_frame(rows, ep) if rows else pl.DataFrame()
        return self._store(ep, target, frame, source="api")

    def ingest_date(self, ep: Endpoint, day: dt.date) -> Ingest:
        return self.ingest(ep, day.isoformat(), {ep.date_param: day.isoformat()})

    def ingest_range(self, ep: Endpoint, start: dt.date, end: dt.date) -> Ingest:
        return self.ingest(ep, end.isoformat(), {"from": start.isoformat(), "to": end.isoformat()})

    def ingest_all(self, ep: Endpoint, today: dt.date) -> Ingest:
        return self.ingest(ep, today.isoformat(), {})

    def _store(self, ep: Endpoint, target: str, frame: pl.DataFrame, *, source: str) -> Ingest:
        changed = self.archive.upsert(ep, frame) if frame.height else 0
        self.ledger.record(
            ep,
            target,
            source=source,
            rows=frame.height,
            changed=changed,
            digest=digest_of(frame),
            run_id=self.run_id,
        )
        log.info(
            "取り込み",
            code="jquants.ingest",
            endpoint=ep.path,
            target=target,
            source=source,
            rows=frame.height,
            changed=changed,
        )
        return Ingest(ep.path, target, source, frame.height, changed)

    # -- 一括（初回） ---------------------------------------------------------

    def backfill(
        self, ep: Endpoint, *, since: str | None = None, keep_raw: bool = True
    ) -> list[Ingest]:
        """一括ダウンロードで全期間を取り込む。``since`` は ``YYYY-MM``。

        ファイル名に年月が入っている（``equities_bars_daily_202501.csv.gz``）ので、
        それで絞る。台帳に同じ ``Key`` と同じ ``LastModified`` があれば飛ばす。
        """
        if not ep.bulk:
            raise ValueError(f"{ep.path} は一括ダウンロードに無い。`sync --days` で遡ってください")
        results = []
        for item in self.client.bulk_list(ep.path):
            key = str(item.get("Key", ""))
            month = _month_in(key)
            if since and month and month < since:
                continue
            target = f"bulk:{key}"
            previous = self.ledger.last(ep, target)
            stamp = str(item.get("LastModified", ""))
            if previous is not None and previous.digest == stamp:
                continue
            payload = self.client.bulk_download(key)
            if keep_raw:
                raw = self.archive.raw_dir(ep) / Path(key).name
                raw.parent.mkdir(parents=True, exist_ok=True)
                raw.write_bytes(payload)
            frame = csv_to_frame(payload, ep)
            changed = self.archive.upsert(ep, frame)
            # 一括は LastModified を digest に入れ、変わらなければ次回飛ばす
            self.ledger.record(
                ep,
                target,
                source="bulk",
                rows=frame.height,
                changed=changed,
                digest=stamp,
                run_id=self.run_id,
            )
            log.info(
                "一括取り込み",
                code="jquants.ingest",
                endpoint=ep.path,
                target=target,
                source="bulk",
                rows=frame.height,
                changed=changed,
            )
            results.append(Ingest(ep.path, target, "bulk", frame.height, changed))
        return results

    # -- 日次（増分） --------------------------------------------------------

    def trading_days(self, start: dt.date, end: dt.date) -> list[dt.date]:
        """保存済みの取引カレンダーから営業日を引く。無ければ平日で代用する。"""
        cal = endpoint("/markets/calendar")
        frame = self.archive.read(cal, start, end)
        if frame.height and "HolDiv" in frame.columns:
            days = frame.filter(pl.col("HolDiv").is_in(list(TRADING_DAY_DIVISIONS)))["Date"]
            return sorted(d for d in days.to_list() if d is not None)
        log.warning("取引カレンダーが無いので平日で代用します", code="jquants.no_calendar")
        return [d for d in _each_day(start, end) if d.weekday() < 5]

    def plan(
        self, now: dt.datetime, *, lookback_days: int | None = None
    ) -> list[tuple[Endpoint, str, dict[str, str]]]:
        """今やるべき取り込みを列挙する（実行はしない）。

        端点ごとに、対象日 D を「D の ``available_at``（JST）を過ぎている」かつ
        「まだ取っていない、または訂正の猶予（``settle_days``）内で前回から
        ``min_interval_hours`` 過ぎた」なら対象にする。
        """
        now = now.astimezone(dt.UTC)
        today = now.astimezone(JST).date()
        jobs: list[tuple[Endpoint, str, dict[str, str]]] = []
        for ep in ENDPOINTS:
            if ep.mode is Mode.ALL:
                if self._due(ep, today, now, target=today.isoformat()) or self._never_fetched(ep):
                    jobs.append((ep, today.isoformat(), {}))
                continue
            if ep.mode is Mode.RANGE:
                start = today - dt.timedelta(days=ep.range_days)
                if self._due(ep, today, now, target=today.isoformat()) or self._never_fetched(ep):
                    jobs.append(
                        (
                            ep,
                            today.isoformat(),
                            {"from": start.isoformat(), "to": today.isoformat()},
                        )
                    )
                continue
            back = lookback_days if lookback_days is not None else ep.settle_days
            first = today - dt.timedelta(days=back)
            days = (
                self.trading_days(first, today)
                if ep.trading_days_only
                else [d for d in _each_day(first, today) if d.weekday() < 5]
            )
            backfilling = lookback_days is not None
            covered_months = self.bulk_months(ep) if backfilling else set()
            for day in days:
                if backfilling and day.strftime("%Y-%m") in covered_months:
                    continue  # 一括で取り込み済みの月を日付で叩き直さない
                if self._due(ep, day, now, target=day.isoformat(), backfilling=backfilling):
                    jobs.append((ep, day.isoformat(), {ep.date_param: day.isoformat()}))
        return jobs

    def _never_fetched(self, ep: Endpoint) -> bool:
        """この端点をまだ一度も取っていない。初回だけは公開時刻を待たずに取る
        （公開待ちは「当日ぶんが乗る時刻」の話で、過去ぶんしか無い初回には関係ない）。"""
        return not self.ledger.targets(ep)

    def _due(
        self,
        ep: Endpoint,
        day: dt.date,
        now: dt.datetime,
        *,
        target: str,
        backfilling: bool = False,
    ) -> bool:
        available = dt.datetime.combine(day, ep.available_at, tzinfo=JST).astimezone(dt.UTC)
        if now < available:
            return False
        last = self.ledger.last(ep, target)
        if last is None:
            return True
        if backfilling:
            return False  # 遡りは「一度も取っていない日」だけ
        final = available + dt.timedelta(days=ep.settle_days)
        if last.fetched_utc >= final:
            return False
        return now - last.fetched_utc >= dt.timedelta(hours=ep.min_interval_hours)

    def sync(
        self,
        now: dt.datetime | None = None,
        *,
        lookback_days: int | None = None,
        only: Iterable[str] = (),
    ) -> SyncResult:
        """やるべき取り込みを順に実行する。冪等。

        1 端点の失敗で全体を止めない。落とすと後続の端点がその日ぶんを
        永久に取り損ねるわけではない（次回また対象になる）が、蓄積が
        1 日遅れる。失敗は集めて返し、呼び出し側が非 0 で終了する。
        """
        moment = now or now_utc()
        wanted = {endpoint(n).path for n in only}
        result = SyncResult()
        # 取引カレンダーは他の端点の「営業日」判定に使うので、期限が来ていれば先に取る
        cal = endpoint("/markets/calendar")
        if (not wanted or cal.path in wanted) and self._due(
            cal,
            moment.astimezone(JST).date(),
            moment.astimezone(dt.UTC),
            target=moment.astimezone(JST).date().isoformat(),
        ):
            self._try(result, cal, moment.astimezone(JST).date().isoformat(), {})
        for ep, target, params in self.plan(moment, lookback_days=lookback_days):
            if ep is cal or (wanted and ep.path not in wanted):
                continue
            self._try(result, ep, target, params)
        return result

    def _try(self, result: SyncResult, ep: Endpoint, target: str, params: dict[str, str]) -> None:
        try:
            result.ingests.append(self.ingest(ep, target, params))
        except MarketDataError as exc:
            log.error(
                "取り込みに失敗",
                code="jquants.ingest_failed",
                endpoint=ep.path,
                target=target,
                error=str(exc),
            )
            result.failures.append(Failure(ep.path, target, str(exc)))

    # -- 確認 ---------------------------------------------------------------

    def bulk_months(self, ep: Endpoint) -> set[str]:
        """一括ダウンロードで取り込み済みの月（``YYYY-MM``）。

        一括の月ファイルはその月の全営業日を含むので、この月に属する日は
        日次の取り込み記録が無くても「取得済み」とみなす。遡り（``--days``）と
        欠け判定が、一括で埋まった 10 年ぶんを日付で叩き直すのを防ぐ。
        """
        months = set()
        for target in self.ledger.targets(ep):
            if target.startswith("bulk:"):
                month = _month_in(target)
                if month:
                    months.add(month)
        return months

    def gaps(
        self, ep: Endpoint, start: dt.date, end: dt.date, now: dt.datetime | None = None
    ) -> list[dt.date]:
        """期間内の営業日のうち、取れているはずなのに無い日。

        「無い」の判定は 2 段:
        - データにその日付の行がある（bars のような毎日必ず行があるもの）か、
        - 台帳にその日の取り込み記録がある（EDINET のように提出が無い日は
          行が 0 件のもの。取ったが空だったのは欠けではない）。
        公開時刻（``available_at``）がまだ来ていない日は数えない。
        """
        if ep.mode is not Mode.DATE:
            return []
        moment = (now or now_utc()).astimezone(dt.UTC)
        have = set(self.archive.dates(ep))
        fetched = set(self.ledger.targets(ep))
        covered_months = self.bulk_months(ep)
        days = (
            self.trading_days(start, end)
            if ep.trading_days_only
            else [d for d in _each_day(start, end) if d.weekday() < 5]
        )
        return [
            d
            for d in days
            if d not in have
            and d.isoformat() not in fetched
            and d.strftime("%Y-%m") not in covered_months
            and moment >= dt.datetime.combine(d, ep.available_at, tzinfo=JST).astimezone(dt.UTC)
        ]


def _each_day(start: dt.date, end: dt.date) -> Iterable[dt.date]:
    current = start
    while current <= end:
        yield current
        current += dt.timedelta(days=1)


def _month_in(key: str) -> str | None:
    """``…_202501.csv.gz`` から ``2025-01``。無ければ None。"""
    import re

    m = re.search(r"(20\d{2})(\d{2})", Path(key).name)
    return f"{m.group(1)}-{m.group(2)}" if m else None


def as_of(
    frame: pl.DataFrame | pl.LazyFrame,
    date: dt.date,
    *,
    date_column: str = "DiscDate",
    by: str = "Code",
) -> pl.DataFrame:
    """「その時点で見えていた最新の 1 件」を銘柄ごとに取る（ルックアヘッド防止の定型）。"""
    lazy = frame.lazy().filter(pl.col(date_column) <= date)
    order = [date_column] + (["DiscTime"] if "DiscTime" in lazy.collect_schema() else [])
    return lazy.sort(order).group_by(by, maintain_order=True).last().collect()
