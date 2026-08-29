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
from typing import TYPE_CHECKING, Annotated

import typer
from rich.console import Console
from rich.table import Table

from wbcore.broker.base import BrokerError
from wbcore.credentials import Credentials as Creds
from wbcore.credentials import (
    Environment,
    MissingCredentialsError,
    credential_source,
    load_credentials,
    store_credentials,
)
from wbcore.data.provider import MarketDataProvider
from wbcore.logging import configure_logging, get_logger
from wbjp.config import Config, load_config

if TYPE_CHECKING:
    from wbjp.engine.backtest import BacktestRunner
    from wbjp.engine.bt_engine import BacktraderRunner

app = typer.Typer(
    help="Webull証券 日本株 自動売買システム",
    no_args_is_help=True,
    add_completion=False,
)
data_app = typer.Typer(help="足データの取得と確認", no_args_is_help=True)
creds_app = typer.Typer(help="APIキーの管理", no_args_is_help=True)
app.add_typer(data_app, name="data")
app.add_typer(creds_app, name="credentials")

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
    """設定の ``execution.broker`` で選んだブローカーに接続する（積立と同じ経路）。"""
    from wbcore.broker.registry import connect

    return connect(
        config.file.execution.broker,
        config.env,
        market=config.file.universe.market,
        tax_type=config.file.execution.tax_account_type,
        extended_hours=config.file.execution.extended_hours,
        notify=lambda message: console.print(f"[yellow]{message}[/yellow]"),
    )


def _build_provider(config: Config) -> MarketDataProvider:
    """設定の ``universe.data_provider`` で選んだ取得元を組み立てる。"""
    from wbcore.data.registry import connect

    return connect(
        config.file.universe.data_provider, config.env, market=config.file.universe.market
    )


def _build_feed(config: Config):  # type: ignore[no-untyped-def]
    """設定の間隔と基準足で足を供給する窓口。"""
    from wbcore.data.feed import BarFeed

    universe = config.file.universe
    return BarFeed(config.settings.bars_dir, universe.market, base=universe.base_bar_interval)


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
    from wbcore.data.store import BarStore
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
        store=BarStore(config.settings.bars_dir, config.file.universe.bar_interval),
        journal=journal,
        provider=None if no_sync else _build_provider(config),
    )

    result = runner.run_once(live=live, as_of=dt.date.fromisoformat(as_of) if as_of else None)

    mode = "[green]実発注[/green]" if result.live else f"[yellow]dry-run[/yellow] ({result.reason})"
    console.print(
        f"\n[bold]サイクル {result.run_id}[/bold]  基準日 {result.as_of}  "
        f"市場 {config.file.universe.market.value}  {mode}"
    )

    if result.planned:
        table = Table(title="注文", title_justify="left")
        for column in ("銘柄", "売買", "種別", "数量", "価格", "状態", "理由"):
            table.add_column(column)
        placed_ids = {r.client_order_id for r in result.placed}
        for request in result.planned:
            price = request.limit_price or request.stop_price
            table.add_row(
                request.symbol,
                request.side.value,
                request.order_type.value,
                f"{request.quantity:,}",
                str(price) if price is not None else "成行",
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
    cash: Annotated[
        int | None, typer.Option(help="初期資金（口座通貨。既定は円300万/ドル3万）")
    ] = None,
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
    engine: Annotated[
        str,
        typer.Option(
            help="約定エンジン: native（自前の PaperBroker）/ backtrader（Cerebro で約定）"
        ),
    ] = "native",
) -> None:
    """保存済みの足でバックテストする。

    判断ロジックはどちらのエンジンでも同一。--engine backtrader は約定と
    口座管理を Backtrader に任せ、自前エンジンとの突き合わせに使う。
    """
    from wbjp.engine.backtest import BacktestRunner
    from wbjp.strategy.registry import build_all

    if engine not in {"native", "backtrader"}:
        console.print(f"[red]--engine は native か backtrader: {engine}[/red]")
        raise typer.Exit(2)

    config = _load(config_dir)
    feed = _build_feed(config)
    store = feed.store(config.file.universe.bar_interval)
    symbols = config.file.universe.symbols

    bars = feed.read(symbols, config.file.universe.bar_interval)
    if not bars:
        console.print("[red]足データがありません。先に `wbjp data sync` を実行してください[/red]")
        raise typer.Exit(1)

    if cash is None:
        cash = 3_000_000 if config.file.universe.market.value == "JP" else 30_000
    strategies = build_all(config.file.strategies.enabled)
    runner: BacktestRunner | BacktraderRunner
    if engine == "backtrader":
        from wbjp.engine.bt_engine import BacktraderRunner

        runner = BacktraderRunner(strategies, config.file, initial_cash=Decimal(cash))
    else:
        runner = BacktestRunner(strategies, config.file, initial_cash=Decimal(cash))
    cash_yield = None
    yield_symbol = config.file.regime.cash_yield_symbol
    if yield_symbol and engine == "native":
        cash_yield = store.read(yield_symbol)
        if cash_yield.height == 0:
            console.print(
                f"[yellow]待機資金の利回り {yield_symbol} の足がありません。無利息で続けます[/yellow]"
            )
            cash_yield = None
    result = runner.run(
        bars,
        start=dt.date.fromisoformat(from_),
        end=dt.date.fromisoformat(to) if to else None,
        **({"cash_yield": cash_yield} if engine == "native" else {}),
    )

    table = Table(title=f"バックテスト結果 ({engine})", title_justify="left")
    table.add_column("項目")
    table.add_column("値", justify="right")
    for key, value in result.summary().items():
        table.add_row(str(key), str(value))
    for key, value in result.analysis.items():
        table.add_row(str(key), str(value))
    console.print(table)

    if engine == "backtrader" and config.file.execution.order_type == "limit":
        console.print(
            "\n[yellow]※ 指値は Backtrader がバー内の高安で約定判定するため、"
            "自前エンジン（寄付だけで判定）より約定しやすく、結果は一致しません。"
            '突き合わせには execution.order_type = "market" を使ってください[/yellow]'
        )
    console.print(
        "\n[dim]※ 過去の成績は将来を保証しません。"
        "少数銘柄・短期間の結果は特に当てになりません[/dim]"
    )


@app.command()
def quality(
    candidates: Annotated[
        Path | None, typer.Option(help="候補ティッカーのファイル（1行1銘柄）。省略時はユニバース")
    ] = None,
    out: Annotated[Path | None, typer.Option(help="合格銘柄を書き出す universe ファイル")] = None,
    refresh: Annotated[bool, typer.Option("--refresh", help="財務データを取り直す")] = False,
    relaxed: Annotated[
        bool, typer.Option("--relaxed", help="緩めの閾値（30〜50 銘柄に収める）")
    ] = False,
    show_failed: Annotated[
        bool, typer.Option("--show-failed", help="不合格も理由付きで表示")
    ] = False,
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """財務の質（ROE・粗利率・負債・FCF）で母集団を絞る（バフェット流スクリーニング）。

    yfinance の年次財務（直近 4 期）で判定する。過去時点の財務は取れないため、
    書き出した母集団でバックテストすると **今日の情報で過去を選ぶ** 生存者バイアスが乗る。
    """
    from wbcore.data.fundamentals import FundamentalsStore, QualityThresholds, evaluate
    from wbjp.config import read_symbols_file

    config = load_config(config_dir)
    symbols = read_symbols_file(candidates) if candidates else list(config.file.universe.symbols)
    symbols = [
        s for s in dict.fromkeys(symbols) if not s.startswith("^") and s not in ("SPY", "QQQ")
    ]
    store = FundamentalsStore(config.settings.data_dir / "fundamentals")
    thresholds = QualityThresholds.relaxed() if relaxed else QualityThresholds()

    reports = []
    with console.status(f"財務データを取得中（{len(symbols)} 銘柄）"):
        for symbol in symbols:
            data = store.get(symbol, refresh=refresh)
            if data is None:
                continue
            reports.append(evaluate(data, thresholds))

    passed = [r for r in reports if r.passed]
    table = Table(
        title=f"質のスクリーニング: 合格 {len(passed)} / {len(reports)}", title_justify="left"
    )
    for column in (
        "銘柄",
        "ROE最小",
        "粗利最小",
        "営業利益率",
        "D/E",
        "利払余力",
        "FCF/NI",
        "FCF成長",
    ):
        table.add_column(column, justify="right" if column != "銘柄" else "left")
    if show_failed:
        table.add_column("不合格の理由")

    def fmt(m: dict[str, float], key: str, pct: bool = True) -> str:
        v = m.get(key)
        if v is None:
            return "—"
        if v == float("inf"):
            return "∞"
        return f"{v:.0%}" if pct else f"{v:.1f}"

    for r in sorted(reports, key=lambda r: (not r.passed, r.symbol)):
        if not r.passed and not show_failed:
            continue
        row = [
            r.symbol if r.passed else f"[dim]{r.symbol}[/dim]",
            fmt(r.metrics, "roe_min"),
            fmt(r.metrics, "gross_margin_min"),
            fmt(r.metrics, "operating_margin"),
            fmt(r.metrics, "debt_to_equity", pct=False),
            fmt(r.metrics, "interest_coverage", pct=False),
            fmt(r.metrics, "fcf_to_net_income", pct=False),
            fmt(r.metrics, "fcf_growth"),
        ]
        if show_failed:
            row.append("; ".join(r.failed))
        table.add_row(*row)
    console.print(table)

    if out:
        lines = [
            "# 財務の質で絞った母集団（`wbjp quality` が生成）。",
            f"# 生成日 {dt.date.today()}。閾値: {thresholds}",
            "# 注意: 今日の財務で選んでいるため、過去のバックテストには生存者バイアスが乗る。",
            "SPY",
            *[r.symbol for r in passed],
        ]
        out.write_text("\n".join(lines) + "\n", encoding="utf-8")
        console.print(f"\n{len(passed)} 銘柄 ＋ SPY を {out} に書き出しました")


@app.command()
def screen(
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
    as_of: Annotated[str | None, typer.Option(help="判断の基準日 YYYY-MM-DD")] = None,
    top: Annotated[int, typer.Option(help="表示する件数")] = 20,
    show_failed: Annotated[
        bool, typer.Option("--show-failed", help="条件を満たさなかった銘柄も理由付きで表示")
    ] = False,
) -> None:
    """保存済みの足で銘柄をスクリーニングし、エントリー条件の合致度順に並べる。

    上位 sizing.max_positions 件が、次のサイクルで新規に建てる候補。
    ブローカーには接続しない（建玉は空として評価する）。
    """
    from wbcore.data.store import BarStore
    from wbjp.strategy.base import StrategyContext
    from wbjp.strategy.combiner import build_combiner
    from wbjp.strategy.registry import build_all
    from wbjp.strategy.samples.trend_pullback import TrendPullbackStrategy

    config = _load(config_dir)
    store = BarStore(config.settings.bars_dir)
    bars = store.read_many(config.file.universe.symbols)
    if not bars:
        console.print("[red]足データがありません。先に `wbjp data sync` を実行してください[/red]")
        raise typer.Exit(1)

    cutoff = dt.date.fromisoformat(as_of) if as_of else None
    if cutoff:
        import polars as pl

        bars = {s: f.filter(pl.col("date") <= cutoff) for s, f in bars.items()}
        bars = {s: f for s, f in bars.items() if f.height > 0}
    latest = max(f["date"].max() for f in bars.values())  # type: ignore[type-var]
    if not isinstance(latest, dt.date):
        raise TypeError(f"足の日付が date ではありません: {type(latest)}")

    strategies = build_all(config.file.strategies.enabled)
    ctx = StrategyContext(as_of=latest, _bars=bars)
    signals = [sig for strategy in strategies for sig in strategy.on_bars(ctx)]
    combiner = build_combiner(
        config.file.strategies.combiner,
        {s.name: s.weight for s in config.file.strategies.enabled},
    )
    combined = combiner.combine(signals)
    threshold = config.file.strategies.entry_threshold
    ranked = sorted(
        (c for c in combined.values() if c.direction >= threshold),
        key=lambda c: -c.direction,
    )
    meta = {sig.symbol: sig.meta for sig in signals if sig.meta}
    reasons = {sig.symbol: sig.reason for sig in signals}

    console.print(
        f"\n[bold]スクリーニング[/bold]  基準日 {latest}  市場 {config.file.universe.market.value}"
        f"  対象 {len(bars)} 銘柄  合致 {len(ranked)} 銘柄"
    )
    if ranked:
        limit = config.file.sizing.max_positions
        table = Table(title=f"順位（上位 {limit} 件が採用候補）", title_justify="left")
        for column in (
            "順位",
            "銘柄",
            "スコア",
            "終値",
            "出来高比",
            "高値比",
            "ATR%",
            "売買代金",
            "内訳",
        ):
            table.add_column(
                column,
                justify="right" if column not in ("銘柄", "内訳") else "left",
                no_wrap=column != "内訳",
            )
        for rank, item in enumerate(ranked[:top], start=1):
            m = meta.get(item.symbol, {})
            style = "green" if rank <= limit else ""
            table.add_row(
                f"[{style}]{rank}[/{style}]" if style else str(rank),
                item.symbol,
                f"{item.direction:.3f}",
                f"{m.get('close', 0) or ctx.close(item.symbol):,.2f}",
                f"{m['dryup_ratio']:.0%}" if "dryup_ratio" in m else "-",
                f"{-m['drawdown']:.1%}" if "drawdown" in m else "-",
                f"{m['atr_ratio']:.1%}" if "atr_ratio" in m else "-",
                f"{m['dollar_volume'] / 1e6:,.0f}M" if "dollar_volume" in m else "-",
                _score_breakdown(m) or reasons.get(item.symbol, item.reason)[:50],
            )
        console.print(table)
    else:
        console.print("  条件を満たす銘柄はありません")

    if show_failed:
        pullbacks = [s for s in strategies if isinstance(s, TrendPullbackStrategy)]
        if not pullbacks:
            console.print("[yellow]--show-failed は trend_pullback 戦略でのみ使えます[/yellow]")
            return
        strategy = pullbacks[0]
        console.print("\n[bold]条件を満たさなかった銘柄[/bold]")
        for symbol in sorted(bars):
            if symbol in combined or symbol == strategy.benchmark:
                continue
            if not ctx.has_bars(symbol, strategy.warmup_bars):
                console.print(f"  {symbol}: 足が {strategy.warmup_bars} 本に満たない")
                continue
            frame = ctx.bars(symbol).with_columns(strategy.indicators())
            result = strategy.screen(frame)
            console.print(f"  {symbol}: " + " / ".join(result.failed))


def _score_breakdown(meta: dict[str, object]) -> str:
    """スコアの内訳（押し目/乖離/趨勢/流動性）を1行にする。"""
    labels = (("dryup", "出来高枯れ"), ("rs", "RS"), ("trend", "趨勢"), ("liquid", "流動性"))
    parts = [f"{label} {float(meta[key]):.2f}" for key, label in labels if key in meta]  # type: ignore[arg-type]
    return " / ".join(parts)


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


@app.command("strategies")
def strategies_list(
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """使える戦略を一覧する。「この設定での使用」は ``strategies.toml`` での重み。

    積立の戦略は別プロジェクト（`uv run accum strategies`）。
    """
    from wbcore.registry import summary_of
    from wbjp.strategy.registry import STRATEGIES

    # 設定は読めなくてもよい。登録済みの戦略一覧は設定と独立に見せたい。
    usage: dict[str, str] = {}
    directory = config_dir or load_config(config_dir).settings.config_dir
    try:
        file_config = load_config(directory).file
    except (FileNotFoundError, ValueError) as exc:
        console.print(f"[yellow]設定を読めませんでした（登録済みのみ表示）: {exc}[/yellow]\n")
    else:
        for entry in file_config.strategies.strategies:
            usage[entry.name] = (
                f"重み{entry.weight:g}"
                if entry.enabled
                else f"[dim]重み{entry.weight:g}（停止）[/dim]"
            )

    table = Table(title=f"戦略 ({directory})", title_justify="left")
    table.add_column("戦略")
    table.add_column("説明")
    table.add_column("この設定での使用")
    for cls in STRATEGIES.classes():
        table.add_row(cls.name, summary_of(cls), usage.get(cls.name, "[dim]—[/dim]"))
    console.print(table)


# --------------------------------------------------------------------------
# データ
# --------------------------------------------------------------------------


@data_app.command("sync")
def data_sync(
    days: Annotated[int, typer.Option(help="何日ぶん遡って取得するか")] = 900,
    force: Annotated[bool, typer.Option("--force", help="保存済みを無視して取り直す")] = False,
    interval: Annotated[
        str | None,
        typer.Option(
            help="足の間隔: 1d / 1h / 30m / 15m / 5m / 1m。省略時は設定の universe.interval"
        ),
    ] = None,
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
) -> None:
    """足データを更新する（保存済みの続きだけ取得）。"""
    from wbcore.data.provider import Interval
    from wbcore.data.store import BarStore

    config = _load(config_dir)
    try:
        chosen = Interval.parse(interval) if interval else config.file.universe.bar_interval
    except ValueError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(2) from None
    # --interval を明示したときはその足だけ、省略時は設定の基準足も一緒に揃える
    feed = _build_feed(config) if interval is None else None
    store = BarStore(config.settings.bars_dir, chosen)
    end = dt.date.today()

    symbols = list(config.file.universe.symbols)
    # レジーム判定の指数と待機資金の利回り系列も一緒に取る
    if config.file.regime.enabled:
        for extra in (config.file.regime.benchmark, config.file.regime.cash_yield_symbol):
            if extra and extra not in symbols:
                symbols.append(extra)
    provider = _build_provider(config)
    start = end - dt.timedelta(days=days)
    try:
        if feed is not None:
            counts = feed.sync(provider, symbols, start, end, chosen, force=force)
        else:
            counts = store.sync(provider, symbols, start, end, force=force)
    except Exception as exc:
        # cron の中で黙って落ちると、分足の穴に気づくのが遅れる
        from wbcore.notify import alert

        alert("足の取り込みに失敗", f"{directory_label(config)} / {chosen.value}: {exc}")
        console.print(f"[red]足の取り込みに失敗しました: {exc}[/red]")
        raise typer.Exit(1) from None
    for symbol, count in sorted(counts.items()):
        console.print(f"  {symbol}: {count} 本")


def directory_label(config: Config) -> str:
    return str(config.settings.config_dir)


@data_app.command("check")
def data_check(
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
    days: Annotated[int, typer.Option(help="穴を探す範囲（暦日）")] = 30,
    notify: Annotated[
        bool, typer.Option("--notify", help="問題があれば WBJP_ALERT_WEBHOOK_URL に通知する")
    ] = False,
) -> None:
    """足の蓄積が止まっていないか、穴が無いかを調べる。問題があれば exit 1。

    cron から `data sync` の直後に回す。取引日は「日足があった日」で決める
    ので祝日の一覧は要らない。分足は 7 日で取れなくなるので、止まっている
    ことに早く気づくのが目的。
    """
    from wbcore.data.health import check
    from wbcore.data.provider import Interval

    config = load_config(config_dir)
    universe = config.file.universe
    intervals: list[Interval] = []
    for candidate in (universe.base_bar_interval, universe.bar_interval, Interval.D1):
        if candidate is not None and candidate not in intervals:
            intervals.append(candidate)
    symbols = list(universe.symbols)
    if not symbols:
        console.print("[red]universe.symbols が空です[/red]")
        raise typer.Exit(2)

    results = check(config.settings.bars_dir, symbols, intervals, lookback_days=days)
    problems = [c for c in results if not c.healthy]

    table = Table(title=f"足の蓄積状況 ({directory_label(config)})", title_justify="left")
    for column in ("銘柄", "間隔", "本数", "最初", "最終", "状態"):
        table.add_column(column, justify="right" if column == "本数" else "left")
    for c in results:
        state = c.describe()
        table.add_row(
            c.symbol,
            c.interval.value,
            f"{c.bars:,}",
            str(c.first or "—"),
            str(c.last or "—"),
            state if c.healthy else f"[red]{state}[/red]",
        )
    console.print(table)

    if not problems:
        console.print("[green]すべて正常[/green]")
        return
    lines = [f"{c.symbol} {c.interval.value}: {c.describe()}" for c in problems]
    console.print(f"\n[red]{len(problems)} 件の問題[/red]")
    if notify:
        from wbcore.notify import alert

        alert(f"足の蓄積に {len(problems)} 件の問題（{directory_label(config)}）", "\n".join(lines))
    raise typer.Exit(1)


@data_app.command("status")
def data_status(
    config_dir: Annotated[Path | None, typer.Option(help="設定ディレクトリ")] = None,
    interval: Annotated[
        str | None, typer.Option(help="見る足の間隔。省略時は設定の universe.interval")
    ] = None,
) -> None:
    """保存済みの足データを一覧する（見立ては含まない。保存されている本物の足だけ）。"""
    from wbcore.data.provider import Interval
    from wbcore.data.store import BarStore

    config = load_config(config_dir)
    try:
        chosen = Interval.parse(interval) if interval else config.file.universe.bar_interval
    except ValueError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(2) from None
    summary = BarStore(config.settings.bars_dir, chosen).summary()

    if summary.height == 0:
        console.print(f"保存済みの {chosen.value} 足はありません")
        return

    table = Table(title=f"保存済みの足（{chosen.value}）", title_justify="left")
    for column in summary.columns:
        table.add_column(column)
    for row in summary.iter_rows():
        table.add_row(*[str(v) for v in row])
    console.print(table)


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
