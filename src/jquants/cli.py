"""J-Quants（Standard）の全データを蓄積するコマンドライン（``jquants``）。

発注は一切しない。API キー（``WBJP_JQUANTS_API_KEY``）とデータ置き場
（``WBJP_DATA_DIR/jquants``）は ``wbjp`` / ``accum`` と共有する。

- ``sync``     台帳を見て必要な端点・日付だけ取る（cron 用。冪等）
- ``backfill`` 一括ダウンロードで全期間を取り込む（初回）
- ``status``   端点ごとの最古・最新・欠け・最終取得
- ``check``    その日の全端点が揃っているか（欠けがあれば非 0 で終了）
- ``query``    DuckDB で端点をビューにして SQL を実行（研究用）
"""

from __future__ import annotations

import datetime as dt
from pathlib import Path
from typing import Annotated

import typer
from rich.console import Console
from rich.table import Table

from wbcore.clock import fmt, now_utc, today_utc
from wbcore.logging import bind_run_context, configure_logging, get_logger
from wbcore.settings import AppSettings

app = typer.Typer(
    help="J-Quants データの蓄積（オフラインで戦略を検討するための保管庫）",
    no_args_is_help=True,
    add_completion=False,
)
console = Console()
log = get_logger(__name__)

_Only = Annotated[
    list[str] | None,
    typer.Option("--only", help="端点を絞る（名前かパス。複数可）"),
]


@app.callback()
def main(
    ctx: typer.Context,
    log_level: Annotated[str, typer.Option(help="ログレベル")] = "INFO",
    json_logs: Annotated[bool, typer.Option("--json-logs", help="端末にも JSON で出力")] = False,
) -> None:
    settings = AppSettings()
    configure_logging(
        log_level,
        json_output=json_logs,
        timezone=settings.timezone,
        log_file=settings.log_file("jquants"),
    )
    run_id = bind_run_context(
        app="jquants", env=settings.env.value, command=ctx.invoked_subcommand or ""
    )
    ctx.obj = {"settings": settings, "run_id": run_id}


def _root(settings: AppSettings) -> Path:
    return settings.data_dir / "jquants"


def _ingestor(ctx: typer.Context, *, need_client: bool = True):  # type: ignore[no-untyped-def]
    """保管庫・台帳・（要れば）API クライアントを組み立てる。

    ``status`` / ``check`` のように手元だけ見るコマンドは API キーが無くても動く。
    """
    from wbcore.credentials import MissingCredentialsError
    from wbcore.data.jquants_archive import Archive, Ingestor, Ledger
    from wbcore.data.jquants_client import JQuantsClient

    settings: AppSettings = ctx.obj["settings"]
    archive = Archive(_root(settings))
    ledger = Ledger(archive.ledger_path)
    client = None
    if need_client:
        try:
            client = JQuantsClient.from_env()
        except MissingCredentialsError as exc:
            console.print(f"[red]{exc}[/red]")
            raise typer.Exit(1) from None
    return Ingestor(client, archive, ledger, run_id=ctx.obj["run_id"])


def _print_results(results, *, title: str) -> None:  # type: ignore[no-untyped-def]
    if not results:
        console.print(f"{title}: やることはありません（すべて最新）")
        return
    table = Table(title=title, title_justify="left")
    for column in ("端点", "対象", "経路", "件数", "変化"):
        table.add_column(column, justify="right" if column in ("件数", "変化") else "left")
    for r in results:
        table.add_row(r.endpoint, r.target, r.source, f"{r.rows:,}", f"{r.changed:,}")
    console.print(table)


# --------------------------------------------------------------------------


@app.command()
def sync(
    ctx: typer.Context,
    days: Annotated[
        int | None,
        typer.Option(
            help="何日遡って「一度も取っていない日」を埋めるか（省略時は訂正の猶予ぶんだけ）"
        ),
    ] = None,
    only: _Only = None,
    dry_run: Annotated[bool, typer.Option("--dry-run", help="取らずに、やることだけ表示")] = False,
) -> None:
    """台帳と取引カレンダーを見て、必要な端点・日付だけ取り込む。冪等。cron はこれを固定間隔で叩く。"""
    ing = _ingestor(ctx)
    settings: AppSettings = ctx.obj["settings"]
    now = now_utc()
    if dry_run:
        from wbcore.data.jquants_archive import endpoint

        jobs = ing.plan(now, lookback_days=days)
        wanted = {endpoint(n).path for n in (only or [])}
        table = Table(title=f"やること（{fmt(now, settings.timezone)}）", title_justify="left")
        table.add_column("端点")
        table.add_column("対象")
        table.add_column("引数")
        shown = 0
        for ep, target, params in jobs:
            if wanted and ep.path not in wanted:
                continue
            table.add_row(ep.path, target, ", ".join(f"{k}={v}" for k, v in params.items()))
            shown += 1
        console.print(table if shown else "やることはありません（すべて最新）")
        return
    result = ing.sync(now, lookback_days=days, only=only or ())
    _print_results(result.ingests, title=f"取り込み（{fmt(now_utc(), settings.timezone)}）")
    if result.failures:
        for f in result.failures:
            console.print(f"[red]{f.endpoint} {f.target}: {f.error}[/red]")
        console.print(
            f"[red]{len(result.failures)} 件の取り込みに失敗しました（次回の sync で再試行されます）[/red]"
        )
        raise typer.Exit(1)


@app.command()
def backfill(
    ctx: typer.Context,
    since: Annotated[
        str | None, typer.Option(help="この年月から（YYYY-MM）。省略時は取れる全期間")
    ] = None,
    only: _Only = None,
    no_raw: Annotated[bool, typer.Option("--no-raw", help="一括 CSV を _raw/ に残さない")] = False,
) -> None:
    """一括ダウンロード（月次 csv.gz）で全期間を取り込む。初回に 1 回。再実行しても更新分だけ取る。"""
    from wbcore.data.jquants_archive import ENDPOINTS, endpoint

    ing = _ingestor(ctx)
    targets = [endpoint(n) for n in only] if only else [e for e in ENDPOINTS if e.bulk]
    all_results = []
    failures = []
    for ep in targets:
        if not ep.bulk:
            console.print(f"[yellow]{ep.path} は一括に無いので `sync --days N` で遡ります[/yellow]")
            continue
        with console.status(f"{ep.path} を一括取り込み中"):
            result = ing.backfill(ep, since=since, keep_raw=not no_raw)
        all_results.extend(result.ingests)
        failures.extend(result.failures)
        console.print(
            f"{ep.path}: {len(result.ingests)} ファイル、{sum(r.rows for r in result.ingests):,} 行"
        )
    _print_results(all_results, title="一括取り込み")
    if failures:
        for f in failures:
            console.print(f"[red]{f.endpoint} {f.target}: {f.error}[/red]")
        console.print(
            f"[red]{len(failures)} 件に失敗しました（再実行すればそこだけ取り直します）[/red]"
        )
        raise typer.Exit(1)


@app.command()
def status(ctx: typer.Context) -> None:
    """端点ごとの保存状況（月数・最古・最新・最終取得）。"""
    from wbcore.data.jquants_archive import ENDPOINTS

    ing = _ingestor(ctx, need_client=False)
    settings: AppSettings = ctx.obj["settings"]
    table = Table(title=f"蓄積の状況（{ing.archive.root}）", title_justify="left")
    for column in ("端点", "月数", "最古", "最新", "最終取得", "一括"):
        table.add_column(column, justify="right" if column == "月数" else "left")
    for ep in ENDPOINTS:
        months = ing.archive.months(ep)
        history = ing.ledger.history(ep, limit=1)
        last = fmt(history[0].fetched_utc, settings.timezone) if history else "—"
        table.add_row(
            ep.path,
            str(len(months)),
            months[0] if months else "—",
            months[-1] if months else "—",
            last,
            "○" if ep.bulk else "—",
        )
    console.print(table)


@app.command()
def check(
    ctx: typer.Context,
    date: Annotated[
        str | None, typer.Option(help="確認する日（YYYY-MM-DD）。既定は直近の営業日")
    ] = None,
    days: Annotated[int, typer.Option(help="欠けを探す範囲（日）")] = 30,
) -> None:
    """営業日ごとの欠けを探す。欠けがあれば非 0 で終了する（監視用）。"""
    from wbcore.data.jquants_archive import ENDPOINTS, Mode

    ing = _ingestor(ctx, need_client=False)
    end = dt.date.fromisoformat(date) if date else today_utc()
    start = end - dt.timedelta(days=days)
    table = Table(title=f"欠け（{start} 〜 {end}）", title_justify="left")
    table.add_column("端点")
    table.add_column("欠けている営業日")
    missing_total = 0
    for ep in ENDPOINTS:
        if ep.mode is not Mode.DATE:
            continue
        gaps = ing.gaps(ep, start, end)
        if gaps:
            missing_total += len(gaps)
            shown = ", ".join(d.isoformat() for d in gaps[:8])
            if len(gaps) > 8:
                shown += f" …（計 {len(gaps)}）"
            table.add_row(ep.path, shown)
    if missing_total:
        console.print(table)
        log.warning("欠けがあります", code="jquants.gap", missing=missing_total)
        raise typer.Exit(2)
    console.print(f"欠けはありません（{start} 〜 {end}）")


@app.command()
def query(
    ctx: typer.Context,
    sql: Annotated[
        str, typer.Argument(help="SQL。端点は名前（equities_bars_daily 等）のビューで参照できる")
    ],
    limit: Annotated[int, typer.Option(help="表示行数")] = 50,
) -> None:
    """DuckDB で横断的に調べる。例: SELECT Code, Date, AdjC FROM equities_bars_daily WHERE Code='72030' ORDER BY Date DESC LIMIT 5"""
    import duckdb

    from wbcore.data.jquants_archive import ENDPOINTS

    settings: AppSettings = ctx.obj["settings"]
    root = _root(settings)
    conn = duckdb.connect()
    for ep in ENDPOINTS:
        directory = root / ep.name
        if directory.is_dir() and any(directory.glob("*.parquet")):
            conn.execute(
                f"CREATE VIEW {ep.name} AS SELECT * FROM read_parquet('{directory}/*.parquet', union_by_name=true)"
            )
    result = conn.execute(sql).pl()
    console.print(result.head(limit))
    if result.height > limit:
        console.print(f"…（全 {result.height:,} 行のうち {limit} 行を表示）")
