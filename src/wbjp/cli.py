"""コマンドライン。

安全の原則:
    ``run`` は既定で **dry-run**。実際に発注するには ``--live`` が要り、
    本番環境ではさらに ``WBJP_ENV=prod`` が要る。片方だけでは発注しない。
"""

from __future__ import annotations

import datetime as dt
import sys
from decimal import Decimal
from pathlib import Path
from typing import Annotated

import typer
from rich.console import Console
from rich.table import Table

from wbjp.broker.base import BrokerError
from wbjp.config import Credentials as Creds
from wbjp.config import (
    Environment,
    MissingCredentialsError,
    credential_source,
    load_config,
    load_credentials,
    store_credentials,
)
from wbjp.logging import configure_logging, get_logger, register_secret

app = typer.Typer(
    help="Webull証券 日本株 自動売買システム",
    no_args_is_help=True,
    add_completion=False,
)
data_app = typer.Typer(help="足データの取得と確認", no_args_is_help=True)
creds_app = typer.Typer(help="APIキーの管理", no_args_is_help=True)
accumulate_app = typer.Typer(help="指数の積立（戦術と銘柄の対応）", no_args_is_help=True)
app.add_typer(data_app, name="data")
app.add_typer(creds_app, name="credentials")
app.add_typer(accumulate_app, name="accumulate")

console = Console()
log = get_logger(__name__)


@app.callback()
def main(
    log_level: Annotated[str, typer.Option(help="ログレベル")] = "INFO",
    json_logs: Annotated[bool, typer.Option("--json-logs", help="JSON形式で出力")] = False,
) -> None:
    configure_logging(log_level, json_output=json_logs)


# --------------------------------------------------------------------------
# 補助
# --------------------------------------------------------------------------


def _build_broker(config, live: bool):  # type: ignore[no-untyped-def]
    """環境に応じたブローカーを作る。

    dry-run でも本物のブローカーを使う。残高・建玉・未約定の実データが
    無ければ、差分計算が意味を成さないため。発注だけを止める。
    """
    from wbjp.broker.webull import WebullBroker

    credentials = load_credentials(config.env)
    register_secret(credentials.app_key, credentials.app_secret, credentials.account_id)

    if credentials.is_public_test_account:
        console.print(
            "[yellow]公開テスト口座を使用中です。残高・建玉は他の利用者により変動します[/yellow]"
        )

    broker = WebullBroker(
        credentials,
        config.env,
        config.settings.endpoints.trade,
        tax_type=config.file.execution.tax_account_type,
    )
    broker.check_key_expiry()
    return broker


def _load(config_dir: Path | None):  # type: ignore[no-untyped-def]
    config = load_config(config_dir)
    if not config.file.universe.symbols:
        console.print(
            "[red]universe.symbols が空です。config/settings.toml を設定してください[/red]"
        )
        raise typer.Exit(1)
    return config


# --------------------------------------------------------------------------
# 口座
# --------------------------------------------------------------------------


@app.command()
def account(
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """残高・建玉・未約定注文を表示する。"""
    config = load_config(config_dir)
    broker = _build_broker(config, live=False)

    balance = broker.get_balance()
    console.print(f"\n[bold]口座[/bold] ({config.env.value})")
    console.print(f"  現金        {balance.cash_balance:>15,} {balance.currency}")
    console.print(f"  買付余力    {balance.buying_power:>15,} {balance.currency}")
    console.print(f"  評価額      {balance.market_value:>15,} {balance.currency}")

    positions = broker.get_positions()
    if positions:
        table = Table(title="建玉", title_justify="left")
        for column in ("銘柄", "数量", "取得単価", "現在値", "評価損益"):
            table.add_column(column, justify="right")
        for position in positions:
            pnl = position.unrealized_pnl
            colour = "green" if pnl >= 0 else "red"
            table.add_row(
                position.symbol,
                f"{position.quantity:,}",
                f"{position.cost_price:,}",
                f"{position.last_price:,}",
                f"[{colour}]{pnl:,.0f}[/{colour}]",
            )
        console.print(table)
    else:
        console.print("  建玉なし")

    open_orders = broker.get_open_orders()
    if open_orders:
        table = Table(title="未約定注文", title_justify="left")
        for column in ("銘柄", "売買", "種別", "数量", "指値", "状態"):
            table.add_column(column)
        for order in open_orders:
            table.add_row(
                order.symbol,
                order.side.value,
                order.order_type.value,
                f"{order.quantity:,}",
                str(order.limit_price or "-"),
                order.status.value,
            )
        console.print(table)


@app.command()
def orders(
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """未約定注文を client_order_id 付きで一覧する。

    ``account`` の表には ID が出ないため、取り消したい注文を指定できない。
    """
    config = load_config(config_dir)
    broker = _build_broker(config, live=False)

    open_orders = broker.get_open_orders()
    if not open_orders:
        console.print(f"[dim]未約定注文はありません ({config.env.value})[/dim]")
        return

    table = Table(title=f"未約定注文 ({config.env.value})", title_justify="left")
    for column in ("client_order_id", "銘柄", "売買", "種別", "数量", "未約定", "指値", "状態"):
        table.add_column(column)
    for order in open_orders:
        table.add_row(
            order.client_order_id or "[dim]-[/dim]",
            order.symbol,
            order.side.value,
            order.order_type.value,
            f"{order.quantity:,}",
            f"{order.remaining_quantity:,}",
            str(order.limit_price or "-"),
            order.status.value,
        )
    console.print(table)


@app.command("order")
def order_show(
    client_order_id: Annotated[str, typer.Argument(help="調べたい注文の client_order_id")],
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """1件の注文の現在の状態を照会する。

    取り消し後の確認にも使う。取り消された注文は未約定一覧から消えるが、
    ここでは CANCELLED として残る。
    """
    config = load_config(config_dir)
    broker = _build_broker(config, live=False)

    found = broker.get_order(client_order_id)
    if found is None:
        console.print(f"[yellow]注文が見つかりません: {client_order_id}[/yellow]")
        raise typer.Exit(1)

    console.print(f"\n[bold]注文[/bold] {found.client_order_id} ({config.env.value})")
    console.print(f"  銘柄      {found.symbol}")
    console.print(f"  売買/種別 {found.side.value} / {found.order_type.value}")
    console.print(f"  数量      {found.quantity:,}（約定 {found.filled_quantity:,}）")
    console.print(f"  指値      {found.limit_price or '成行'}")
    console.print(f"  状態      {found.status.value}")
    if found.avg_fill_price:
        console.print(f"  約定単価  {found.avg_fill_price:,}")
    if found.broker_order_id:
        console.print(f"  broker_order_id {found.broker_order_id}")


@app.command()
def cancel(
    client_order_id: Annotated[str, typer.Argument(help="取り消す注文の client_order_id")],
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """未約定の注文を取り消す。

    取消は注文を減らす方向の操作なので、``run --live`` のような
    二重ロックは課さない。
    """
    config = load_config(config_dir)
    broker = _build_broker(config, live=True)

    try:
        broker.cancel(client_order_id)
    except BrokerError as exc:
        console.print(f"[red]取消に失敗しました: {exc}[/red]")
        raise typer.Exit(1) from exc

    console.print(f"[green]取消を送信しました: {client_order_id}[/green]")

    # 取消は非同期のことがある。実際にどうなったかを見て返す。
    found = broker.get_order(client_order_id)
    if found is not None:
        console.print(f"  現在の状態: {found.status.value}")


# --------------------------------------------------------------------------
# 実行
# --------------------------------------------------------------------------


@app.command()
def run(
    live: Annotated[bool, typer.Option("--live", help="実際に発注する（既定は dry-run）")] = False,
    as_of: Annotated[str | None, typer.Option(help="判断の基準日 YYYY-MM-DD")] = None,
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
    no_sync: Annotated[bool, typer.Option("--no-sync", help="足の更新をしない")] = False,
    yes: Annotated[
        bool, typer.Option("--yes", "-y", help="本番の確認を省く（cron など非対話実行用）")
    ] = False,
) -> None:
    """日次サイクルを実行する。

    既定は dry-run。実発注には --live が必要で、本番では
    WBJP_ENV=prod も同時に必要。cron から回すときは --yes も付ける。
    """
    from wbjp.data.store import BarStore
    from wbjp.data.yfinance_provider import YFinanceProvider
    from wbjp.db.repo import Journal
    from wbjp.engine.live import LiveRunner
    from wbjp.strategy.registry import build_all

    config = _load(config_dir)
    allowed, _ = config.allows_live_orders(live)

    if allowed and config.env.is_production and not yes:
        console.print("[bold red]本番環境で実際に発注します[/bold red]")
        # cron には stdin が無い。ここで確認を求めると Abort になるだけで、
        # ログには理由が残らない。何が足りないかを明示して落とす。
        if not sys.stdin.isatty():
            console.print(
                "[red]非対話環境では確認を取れません。cron から回すなら --yes を付けてください[/red]"
            )
            raise typer.Exit(1)
        if not typer.confirm("続行しますか?"):
            raise typer.Abort()

    broker = _build_broker(config, live=allowed)
    journal = Journal(config.settings.db_path)

    runner = LiveRunner(
        config=config,
        strategies=build_all(config.file.strategies.enabled),
        broker=broker,
        store=BarStore(config.settings.bars_dir),
        journal=journal,
        provider=None if no_sync else YFinanceProvider(),
    )

    result = runner.run_once(live=live, as_of=dt.date.fromisoformat(as_of) if as_of else None)

    mode = "[green]実発注[/green]" if result.live else f"[yellow]dry-run[/yellow] ({result.reason})"
    console.print(f"\n[bold]サイクル {result.run_id}[/bold]  基準日 {result.as_of}  {mode}")

    if result.planned:
        table = Table(title="注文", title_justify="left")
        for column in ("銘柄", "売買", "数量", "指値", "状態", "理由"):
            table.add_column(column)
        placed_ids = {r.client_order_id for r in result.placed}
        for request in result.planned:
            table.add_row(
                request.symbol,
                request.side.value,
                f"{request.quantity:,}",
                str(request.limit_price or "成行"),
                "発注" if request.client_order_id in placed_ids else "見送り",
                request.reason[:40],
            )
        console.print(table)
    else:
        console.print("  発注する注文はありません")

    if result.rejected:
        console.print("\n[yellow]見送った銘柄:[/yellow]")
        for symbol, why in sorted(result.rejected.items()):
            console.print(f"  {symbol}: {why}")

    journal.close()


@app.command()
def backtest(
    from_: Annotated[str, typer.Option("--from", help="開始日 YYYY-MM-DD")] = "2024-01-01",
    to: Annotated[str | None, typer.Option("--to", help="終了日 YYYY-MM-DD")] = None,
    cash: Annotated[int, typer.Option(help="初期資金（円）")] = 3_000_000,
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """保存済みの足でバックテストする。"""
    from wbjp.data.store import BarStore
    from wbjp.engine.backtest import BacktestRunner
    from wbjp.strategy.registry import build_all

    config = _load(config_dir)
    store = BarStore(config.settings.bars_dir)
    symbols = config.file.universe.symbols

    bars = store.read_many(symbols)
    if not bars:
        console.print("[red]足データがありません。先に `wbjp data sync` を実行してください[/red]")
        raise typer.Exit(1)

    runner = BacktestRunner(
        build_all(config.file.strategies.enabled), config.file, initial_cash=Decimal(cash)
    )
    result = runner.run(
        bars,
        start=dt.date.fromisoformat(from_),
        end=dt.date.fromisoformat(to) if to else None,
    )

    table = Table(title="バックテスト結果", title_justify="left")
    table.add_column("項目")
    table.add_column("値", justify="right")
    for key, value in result.summary().items():
        table.add_row(str(key), str(value))
    console.print(table)

    console.print(
        "\n[dim]※ 過去の成績は将来を保証しません。"
        "少数銘柄・短期間の結果は特に当てになりません[/dim]"
    )


@app.command()
def explain(
    run_id: Annotated[str, typer.Argument(help="調べたい実行ID")],
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """ある実行の判断過程を丸ごと表示する。

    シグナル → 合成 → 目標 → 注文 → 拒否理由 の順に、
    「なぜそうなったか」を追える。
    """
    from wbjp.db.repo import Journal

    config = load_config(config_dir)
    journal = Journal(config.settings.db_path)

    if journal.get_run(run_id) is None:
        console.print(f"[red]実行 {run_id} が見つかりません[/red]")
        raise typer.Exit(1)

    for section, rows in journal.explain(run_id).items():
        if not rows:
            continue
        table = Table(title=section, title_justify="left")
        for column in rows[0]:
            table.add_column(column)
        for row in rows:
            table.add_row(*[str(v)[:40] for v in row.values()])
        console.print(table)

    journal.close()


@app.command()
def runs(
    limit: Annotated[int, typer.Option(help="表示件数")] = 20,
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """過去の実行を一覧する。"""
    from wbjp.db.repo import Journal

    config = load_config(config_dir)
    journal = Journal(config.settings.db_path)

    table = Table(title="実行履歴", title_justify="left")
    for column in ("run_id", "基準日", "環境", "モード", "状態", "資産"):
        table.add_column(column)
    for row in journal.recent_runs(limit):
        table.add_row(
            row["run_id"],
            row["as_of"],
            row["env"],
            row["mode"],
            row["status"],
            str(row["equity"] or "-"),
        )
    console.print(table)
    journal.close()


# --------------------------------------------------------------------------
# データ
# --------------------------------------------------------------------------


@data_app.command("sync")
def data_sync(
    days: Annotated[int, typer.Option(help="何日ぶん遡って取得するか")] = 900,
    force: Annotated[bool, typer.Option("--force", help="保存済みを無視して取り直す")] = False,
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """足データを更新する（保存済みの続きだけ取得）。"""
    from wbjp.data.store import BarStore
    from wbjp.data.yfinance_provider import YFinanceProvider

    config = _load(config_dir)
    store = BarStore(config.settings.bars_dir)
    end = dt.date.today()

    counts = store.sync(
        YFinanceProvider(),
        config.file.universe.symbols,
        end - dt.timedelta(days=days),
        end,
        force=force,
    )
    for symbol, count in sorted(counts.items()):
        console.print(f"  {symbol}: {count} 本")


@data_app.command("status")
def data_status(
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """保存済みの足データを一覧する。"""
    from wbjp.data.store import BarStore

    config = load_config(config_dir)
    summary = BarStore(config.settings.bars_dir).summary()

    if summary.height == 0:
        console.print("保存済みの足データはありません")
        return

    table = Table(title="保存済みの足", title_justify="left")
    for column in summary.columns:
        table.add_column(column)
    for row in summary.iter_rows():
        table.add_row(*[str(v) for v in row])
    console.print(table)


# --------------------------------------------------------------------------
# 積立
# --------------------------------------------------------------------------

#: 積立関連で共通のオプション。
_ConfigDir = Annotated[Path | None, typer.Option(help="設定ディレクトリ")]


def _yen(value: float) -> str:
    """桁が大きい金額は万円に丸める。80桁の端末で表が潰れるのを避ける。"""
    if abs(value) >= 1_000_000:
        return f"{value / 10_000:,.0f}万"
    return f"{value:,.0f}"


def _load_accumulate(config_dir: Path | None, *, allow_overlap: bool = False):  # type: ignore[no-untyped-def]
    """config/accumulate.toml を読む。失敗したら理由を出して終了する。"""
    from wbjp.accumulate import load

    # 積立は universe.symbols を使わないので、_load ではなく load_config を呼ぶ。
    # _load は売買用の検証（ユニバースが空なら終了）を含んでいる。
    directory = config_dir or load_config(config_dir).settings.config_dir
    try:
        return load(directory, allow_overlap=allow_overlap)
    except FileNotFoundError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(1) from None
    except ValueError as exc:
        console.print(f"[red]設定が不正です: {exc}[/red]")
        raise typer.Exit(1) from None


def _accumulate_bars(config_dir: Path | None, symbols: list[str]):  # type: ignore[no-untyped-def]
    """保存済みの足を読む。無い銘柄は警告して除く。"""
    from wbjp.data.store import BarStore

    store = BarStore(load_config(config_dir).settings.bars_dir)
    bars = store.read_many(symbols)
    missing = sorted(set(symbols) - set(bars))
    if missing:
        console.print(
            f"[yellow]足データがありません: {', '.join(missing)}"
            f"（`wbjp accumulate sync` を実行してください）[/yellow]"
        )
    return bars


@accumulate_app.command("list")
def accumulate_list(config_dir: _ConfigDir = None) -> None:
    """戦術と銘柄の対応を一覧する。"""
    config = _load_accumulate(config_dir, allow_overlap=True)

    table = Table(title="積立戦術", title_justify="left")
    table.add_column("id")
    table.add_column("戦術")
    table.add_column("銘柄")
    table.add_column("有効", justify="center")
    for entry in config.tactics:
        try:
            described = entry.build().describe()
        except ValueError as exc:
            described = f"[red]{exc}[/red]"
        table.add_row(entry.id, described, ", ".join(entry.symbols), "○" if entry.enabled else "—")
    console.print(table)
    console.print(f"\n毎月の基本予算: {config.monthly_budget:,.0f}円 / 銘柄")

    # 一覧は重複を許して読む（止めてある戦術も見せたいため）。そのぶん
    # 衝突を検出できるのはここだけなので、必ず知らせる。
    try:
        config.validate_assignment()
    except ValueError as exc:
        console.print(f"\n[red]{exc}[/red]")
        raise typer.Exit(1) from None


@accumulate_app.command("sync")
def accumulate_sync(
    days: Annotated[int, typer.Option(help="何日ぶん遡って取得するか")] = 10_950,
    force: Annotated[bool, typer.Option("--force", help="保存済みを無視して取り直す")] = False,
    config_dir: _ConfigDir = None,
) -> None:
    """積立対象の銘柄の足を更新する。

    200日移動平均を使ううえ、増額が効くのは暴落局面なので、売買用の銘柄より
    ずっと長い履歴が要る。既定の 10,950 日は約30年ぶん。上昇局面しか含まない
    短い期間で検証すると、増額の効果が実際より小さく出る。

    既に保存済みの銘柄は「最終日より後」しか取りに行かないので、あとから
    期間を伸ばしたいときは ``--force`` を付けて取り直す。
    """
    from wbjp.data.store import BarStore
    from wbjp.data.yfinance_provider import YFinanceProvider

    config = _load_accumulate(config_dir)
    store = BarStore(load_config(config_dir).settings.bars_dir)
    end = dt.date.today()

    counts = store.sync(
        YFinanceProvider(), config.symbols, end - dt.timedelta(days=days), end, force=force
    )
    for symbol, count in sorted(counts.items()):
        console.print(f"  {symbol}: {count} 本")


@accumulate_app.command("plan")
def accumulate_plan(
    days: Annotated[int, typer.Option(help="直近何営業日ぶんを表示するか")] = 10,
    config_dir: _ConfigDir = None,
) -> None:
    """直近の投下額を銘柄ごとに表示する（今いくら買うか）。"""
    import polars as pl

    from wbjp.accumulate import AccumulationSettings, build_plan

    config = _load_accumulate(config_dir)
    bars = _accumulate_bars(config_dir, config.symbols)
    if not bars:
        raise typer.Exit(1)

    total = 0
    for symbol, tactic in config.build().items():
        if symbol not in bars:
            continue
        settings = AccumulationSettings(config.monthly_budget, tactic)
        plan = build_plan(bars[symbol], settings).tail(days)

        table = Table(title=f"{symbol} — {tactic.describe()}", title_justify="left")
        for column in ("日付", "終値", "倍率", "投下額", "理由"):
            table.add_column(column, justify="right" if column != "理由" else "left")
        for row in plan.iter_rows(named=True):
            amount = row["amount"]
            style = "bold" if amount > 0 else "dim"
            table.add_row(
                str(row["date"]),
                f"{row['close']:,.2f}",
                f"{row['multiplier']:.2g}",
                f"{amount:,}円" if amount else "—",
                row["reason"],
                style=style,
            )
        console.print(table)
        total += int(plan.filter(pl.col("date") == plan["date"].max())["amount"][0])

    console.print(f"\n[bold]最終日の投下額 合計: {total:,}円[/bold]")


@accumulate_app.command("backtest")
def accumulate_backtest(
    from_: Annotated[str | None, typer.Option("--from", help="開始日 YYYY-MM-DD")] = None,
    to: Annotated[str | None, typer.Option("--to", help="終了日 YYYY-MM-DD")] = None,
    config_dir: _ConfigDir = None,
) -> None:
    """設定どおりに積み立てた場合の結果を銘柄ごとに出す。"""
    from wbjp.accumulate import AccumulationSettings, build_plan, simulate

    config = _load_accumulate(config_dir)
    start = dt.date.fromisoformat(from_) if from_ else None
    end = dt.date.fromisoformat(to) if to else None
    bars = _accumulate_bars(config_dir, config.symbols)
    if not bars:
        raise typer.Exit(1)

    table = Table(title="積立バックテスト", title_justify="left")
    table.add_column("銘柄")
    table.add_column("戦術")  # 設定の id。describe() は長すぎて表が潰れる
    for column in ("投入", "倍率", "単価", "対照比", "期末", "ﾘﾀｰﾝ"):
        table.add_column(column, justify="right")

    for entry in config.active:
        for symbol in entry.symbols:
            if symbol not in bars:
                continue
            tactic = entry.build()
            frame = bars[symbol]
            if start or end:
                import polars as pl

                if start:
                    frame = frame.filter(pl.col("date") >= start)
                if end:
                    frame = frame.filter(pl.col("date") <= end)
            if frame.height < tactic.warmup_bars:
                console.print(
                    f"[yellow]{symbol}: 足が {frame.height} 本しかなく、"
                    f"{tactic.warmup_bars} 本必要です。飛ばします[/yellow]"
                )
                continue

            settings = AccumulationSettings(config.monthly_budget, tactic)
            result = simulate(
                frame, build_plan(frame, settings), monthly_budget=config.monthly_budget
            )
            edge = result.cost_edge
            table.add_row(
                symbol,
                entry.id,
                _yen(float(result.contributed)),
                f"{result.capital_multiple:.2f}",
                _yen(result.average_cost),
                f"[green]{edge:+.2%}[/green]" if edge < 0 else f"[red]{edge:+.2%}[/red]",
                _yen(result.terminal_value),
                f"{result.total_return:+.0%}",
            )

    console.print(table)
    console.print(
        "\n[dim]※ 倍率＝基本予算だけの場合に対する総投入額の倍率。単価/投入/期末は万円。\n"
        "※ 対照群＝同じ総投入額を毎月均等に投じた場合。マイナスなら安く買えた。\n"
        "　 増額分の原資は新規資金（賞与など）を前提としている。積立予算を取り置いて\n"
        "　 作ると待機が生じ、増額の利益を打ち消す。[/dim]"
    )


@accumulate_app.command("compare")
def accumulate_compare(
    symbol: Annotated[str, typer.Argument(help="比較したい銘柄")],
    config_dir: _ConfigDir = None,
) -> None:
    """1銘柄に対して、登録済みの戦術を既定パラメータで並べて比較する。"""
    from wbjp.accumulate import (
        AccumulationSettings,
        available,
        build_plan,
        create,
        simulate,
    )

    config = _load_accumulate(config_dir, allow_overlap=True)
    bars = _accumulate_bars(config_dir, [symbol])
    if symbol not in bars:
        raise typer.Exit(1)
    frame = bars[symbol]

    table = Table(title=f"{symbol} — 戦術の比較", title_justify="left")
    table.add_column("戦術")
    for column in ("倍率", "単価", "対照比", "1倍あたり", "期末"):
        table.add_column(column, justify="right")

    for name in available():
        tactic = create(name)
        if frame.height < tactic.warmup_bars:
            continue
        settings = AccumulationSettings(config.monthly_budget, tactic)
        result = simulate(frame, build_plan(frame, settings), monthly_budget=config.monthly_budget)
        # 追加資金1倍あたりの単価改善。効果を資金量で割った効率。
        extra = result.capital_multiple - 1.0
        efficiency = f"{result.cost_edge / extra:+.1%}" if extra > 0.001 else "—"
        table.add_row(
            tactic.describe(),
            f"{result.capital_multiple:.2f}",
            _yen(result.average_cost),
            f"{result.cost_edge:+.2%}",
            efficiency,
            _yen(result.terminal_value),
        )

    console.print(table)
    console.print(
        "\n[dim]※ 既定パラメータでの比較。倍率などは config/accumulate.toml で調整する。\n"
        "　 「1倍あたり」は追加資金1倍あたりの単価改善で、資金効率の指標。[/dim]"
    )


# --------------------------------------------------------------------------
# 認証情報
# --------------------------------------------------------------------------


@creds_app.command("set")
def credentials_set(
    env: Annotated[Environment, typer.Option(help="対象環境")] = Environment.UAT,
) -> None:
    """APIキーを OS のキーチェーンに保存する。

    リポジトリには秘密を書かない。キーチェーンの無いサーバーでは
    環境変数か、0600 にした .env を使う（README 参照）。
    """
    app_key = typer.prompt("App Key")
    app_secret = typer.prompt("App Secret", hide_input=True)
    account_id = typer.prompt("Account ID")

    try:
        store_credentials(env, Creds(app_key, app_secret, account_id))
    except Exception as exc:
        # ヘッドレスな Linux には使えるバックエンドが無いことが多い。
        console.print(f"[red]キーチェーンに保存できませんでした: {exc}[/red]")
        console.print(
            "このホストにはキーチェーンが無いようです。代わりに環境変数か、"
            f"0600 にした .env に [bold]WBJP_{env.value.upper()}_APP_KEY[/bold] 等を設定してください。"
        )
        raise typer.Exit(1) from exc
    console.print(f"[green]{env.value} 環境の認証情報をキーチェーンに保存しました[/green]")


@creds_app.command("check")
def credentials_check(
    env: Annotated[Environment, typer.Option(help="対象環境")] = Environment.UAT,
) -> None:
    """認証情報が解決できるか確認する（秘密は表示しない）。"""
    try:
        credentials = load_credentials(env)
    except MissingCredentialsError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(1) from exc

    console.print(f"[green]{env.value}: 認証情報を解決できました[/green]")
    console.print(f"  {credentials!r}")
    console.print(f"  取得元: {credential_source(env)}")
    if credentials.is_public_test_account:
        console.print("[yellow]  公開されている共有テスト口座です[/yellow]")

    days = credentials.days_until_expiry()
    if days is not None:
        colour = "red" if days < 7 else "green"
        console.print(f"  [{colour}]キーの残り有効日数: {days}日[/{colour}]")


if __name__ == "__main__":
    app()
