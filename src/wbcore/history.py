"""追記専用の履歴（Parquet）。選定の候補と結果を、実行のたびに 1 ファイルずつ積む。

台帳（SQLite）は「今日もう買ったか」に答えるための**現在の状態**で、dry-run の記録は
確認のたびに消す。ログ（JSONL）は順位表の上位数件しか持たず、90 日で消える。どちらも
「先週の 9:00 に何が候補で、何を選び、何を選ばなかったか」を後から並べて分析する
用途には向かない。ここはその用途だけのための置き場:

- 1 回の実行 = 1 ファイル。``<root>/<kind>/<day>T<HHMMSS>Z-<run_id>.parquet``。
  同じ名前があれば枝番を付け、**決して上書きしない**
- 全ファイルに ``day``（判定日）・``run_id``・``recorded_at``（UTC）の列が先頭に付く。
  同じ日の複数回の実行（cron の再試行、dry-run の確認）はすべて残り、``run_id`` で
  区別できる。「その日の最終判断」は ``recorded_at`` が最大の行
- 読むときは期間で絞って縦に結合する。列が増えても古いファイルはそのまま読める
  （``diagonal_relaxed``）。polars でも DuckDB でも
  ``read_parquet('<root>/<kind>/*.parquet')`` で直接読める

ファイル名の先頭が判定日なので、期間の絞り込みはファイルを開かずに済む。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING

import polars as pl

from wbcore.clock import ensure_utc, now_utc

if TYPE_CHECKING:
    from rich.console import Console

#: 全ファイルに付く鍵の列（この順で先頭に置く）
KEY_COLUMNS = ("day", "run_id", "recorded_at")


@dataclass(frozen=True, slots=True)
class KindSummary:
    kind: str
    files: int
    first_day: dt.date | None
    last_day: dt.date | None


def _day_of(path: Path) -> dt.date | None:
    """ファイル名の先頭 10 文字（判定日）。形式が違えば None。"""
    try:
        return dt.date.fromisoformat(path.name[:10])
    except ValueError:
        return None


class HistoryStore:
    """1 つの置き場（``root``）の下に種類（``kind``）ごとのディレクトリを持つ。"""

    def __init__(self, root: Path) -> None:
        self.root = root

    def directory(self, kind: str) -> Path:
        return self.root / kind

    # ---- 書く -------------------------------------------------------------

    def append(
        self,
        kind: str,
        frame: pl.DataFrame,
        *,
        day: dt.date,
        run_id: str | None = None,
        at: dt.datetime | None = None,
    ) -> Path:
        """1 回の実行の結果を 1 ファイルとして足す。既存のファイルには触れない。

        ``run_id`` を省くと、いま束ねている CLI 実行の ID（ログと同じ）を使う。
        0 行でも書く——「その日は条件に合う銘柄が無かった」も記録のうち。
        """
        from wbcore.logging import current_run_id

        moment = ensure_utc(at or now_utc())
        rid = run_id or current_run_id() or "manual"
        body = frame.drop([c for c in KEY_COLUMNS if c in frame.columns])
        keyed = body.with_columns(
            pl.lit(day).alias("day"),
            pl.lit(rid).alias("run_id"),
            pl.lit(moment).cast(pl.Datetime("us", "UTC")).alias("recorded_at"),
        ).select([*KEY_COLUMNS, *body.columns])
        directory = self.directory(kind)
        directory.mkdir(parents=True, exist_ok=True)
        path = self._fresh_path(directory, day, moment, rid)
        tmp = path.with_name(path.name + ".tmp")
        keyed.write_parquet(tmp)
        tmp.replace(path)
        return path

    @staticmethod
    def _fresh_path(directory: Path, day: dt.date, moment: dt.datetime, run_id: str) -> Path:
        stem = f"{day.isoformat()}T{moment:%H%M%S}Z-{run_id}"
        path = directory / f"{stem}.parquet"
        n = 1
        while path.exists():
            # 枝番は "_" で繋ぐ。"-" だと名前順で本体（".parquet"）より前に並び、読む順が狂う
            n += 1
            path = directory / f"{stem}_{n}.parquet"
        return path

    # ---- 読む -------------------------------------------------------------

    def kinds(self) -> list[str]:
        if not self.root.is_dir():
            return []
        return sorted(p.name for p in self.root.iterdir() if p.is_dir())

    def files(
        self, kind: str, *, start: dt.date | None = None, end: dt.date | None = None
    ) -> list[Path]:
        """期間（判定日、両端を含む）に入るファイルを古い順に。"""
        directory = self.directory(kind)
        if not directory.is_dir():
            return []
        found: list[Path] = []
        for path in sorted(directory.glob("*.parquet")):
            day = _day_of(path)
            if day is None:
                continue
            if start is not None and day < start:
                continue
            if end is not None and day > end:
                continue
            found.append(path)
        return found

    def days(self, kind: str) -> list[dt.date]:
        return sorted({d for p in self.files(kind) if (d := _day_of(p)) is not None})

    def read(
        self, kind: str, *, start: dt.date | None = None, end: dt.date | None = None
    ) -> pl.DataFrame:
        """期間のファイルを縦に結合する。無ければ列も行も無い空のフレーム。"""
        paths = self.files(kind, start=start, end=end)
        if not paths:
            return pl.DataFrame()
        return pl.concat([pl.read_parquet(p) for p in paths], how="diagonal_relaxed")

    def latest(self, kind: str, day: dt.date) -> pl.DataFrame:
        """その日の最後の実行ぶんだけ（``recorded_at`` が最大の ``run_id``）。"""
        frame = self.read(kind, start=day, end=day)
        if frame.height == 0:
            return frame
        last = frame.select(pl.col("recorded_at").max()).item()
        return frame.filter(pl.col("recorded_at") == last)

    def summary(self) -> list[KindSummary]:
        result = []
        for kind in self.kinds():
            days = self.days(kind)
            result.append(
                KindSummary(
                    kind=kind,
                    files=len(self.files(kind)),
                    first_day=days[0] if days else None,
                    last_day=days[-1] if days else None,
                )
            )
        return result


# --------------------------------------------------------------------------
# CLI の共通部分（daytrade history / wbjp history）
# --------------------------------------------------------------------------


def _cell(value: object) -> str:
    if value is None:
        return ""
    if isinstance(value, float):
        return f"{value:,.4f}".rstrip("0").rstrip(".") if value == value else ""
    if isinstance(value, dt.datetime):
        return value.isoformat(timespec="seconds")
    return str(value)


def show_history(
    console: Console,
    store: HistoryStore,
    kind: str | None,
    *,
    start: dt.date | None = None,
    end: dt.date | None = None,
    latest_only: bool = False,
    limit: int = 50,
    csv_path: Path | None = None,
    as_json: bool = False,
) -> None:
    """``kind`` を省けば種類の一覧、指定すれば期間の行を表として見せる（CSV 書き出し可）。

    ``as_json`` のときは表を出さず、JSON を 1 個だけ標準出力に書く。読み手が AI の
    ときは罫線と桁区切りが邪魔になるだけなので、``limit`` の間引きもしない
    （全行を返す。件数を絞るのは呼ぶ側の ``--from`` / ``--to`` の仕事）。
    """
    from rich.table import Table

    from wbcore.output import emit_json

    if kind is None:
        rows = store.summary()
        if as_json:
            emit_json(
                {
                    "ok": True,
                    "root": str(store.root),
                    "kinds": [
                        {
                            "kind": s.kind,
                            "files": s.files,
                            "first_day": s.first_day,
                            "last_day": s.last_day,
                        }
                        for s in rows
                    ],
                }
            )
            return
        table = Table(title=f"履歴 {store.root}")
        for column in ("種類", "ファイル数", "最初の日", "最後の日"):
            table.add_column(column, justify="right" if column == "ファイル数" else "left")
        for s in rows:
            table.add_row(s.kind, str(s.files), _cell(s.first_day), _cell(s.last_day))
        console.print(table)
        if not rows:
            console.print("[dim]履歴はまだありません[/dim]")
        return
    if latest_only:
        if end is None:
            days = store.days(kind)
            if not days:
                if as_json:
                    emit_json({"ok": True, "kind": kind, "rows": []})
                else:
                    console.print(f"[dim]{kind}: 履歴はまだありません[/dim]")
                return
            end = days[-1]
        frame = store.latest(kind, end)
    else:
        frame = store.read(kind, start=start, end=end)
    if as_json:
        emit_json({"ok": True, "kind": kind, "count": frame.height, "rows": frame.to_dicts()})
        return
    if frame.height == 0:
        console.print(f"[dim]{kind}: 該当する行はありません[/dim]")
        return
    if csv_path is not None:
        # 分析用なので値はそのまま（桁区切りや丸めは表示だけ）
        csv_path.parent.mkdir(parents=True, exist_ok=True)
        frame.write_csv(csv_path)
        console.print(f"[green]{frame.height} 行を {csv_path} に書き出しました[/green]")
    shown = frame.sort(["day", "recorded_at"], descending=[True, True]).head(limit)
    table = Table(title=f"{kind}  {frame.height} 行（表示 {shown.height}）")
    for column in shown.columns:
        # 列が多いので端末幅に合わせて折り返す。分析用の全列は --csv か Parquet を直接読む
        table.add_column(column, overflow="fold")
    for row in shown.iter_rows():
        table.add_row(*[_cell(v) for v in row])
    console.print(table)


__all__ = ["KEY_COLUMNS", "HistoryStore", "KindSummary", "show_history"]
