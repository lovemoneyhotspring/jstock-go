"""積立のコマンドライン（``accum``）。

安全の原則はスイング売買（``wbjp``）と同じ:
    ``run`` は既定で **dry-run**。実際に発注するには ``--live`` が要り、
    本番環境ではさらに ``WBJP_ENV=prod`` が要る。片方だけでは発注しない。

APIキーは ``wbjp`` と共有する（``uv run wbjp credentials set``）。
口座もデータ置き場も同じなので、環境変数の接頭辞も ``WBJP_`` のまま。
"""

from __future__ import annotations

import datetime as dt
import re
import sys
from decimal import Decimal
from pathlib import Path
from typing import TYPE_CHECKING, Annotated

import typer
from rich.console import Console
from rich.markup import escape
from rich.table import Table

from accum.config import DEFAULT_CONFIG_DIR, FILENAME, AccumConfig, load
from wbcore.clock import fmt, now_utc, today_utc
from wbcore.logging import bind_run_context, configure_logging, get_logger
from wbcore.settings import AppSettings, allows_live_orders, describe_mode

if TYPE_CHECKING:
    from accum.execute import Contribution
    from accum.ledger import Ledger
    from wbcore.broker.base import Broker
    from wbcore.domain.models import OrderAck, OrderRequest

app = typer.Typer(
    help="積立（ドル平均法＋下落局面での増額）",
    no_args_is_help=True,
    add_completion=False,
)
console = Console()
log = get_logger(__name__)

#: 全コマンド共通のオプション。
_ConfigDir = Annotated[
    Path | None, typer.Option(help=f"設定ディレクトリ（既定 {DEFAULT_CONFIG_DIR}）")
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
        log_file=settings.log_file("accum"),
    )
    bind_run_context(app="accum", env=settings.env.value, command=ctx.invoked_subcommand or "")


# --------------------------------------------------------------------------
# 補助
# --------------------------------------------------------------------------


def _plain(markup: str) -> str:
    """Rich のマークアップを外す（ログに色指定を残さない）。"""
    return re.sub(r"\[/?[a-z ]+\]", "", markup)


def _yen(value: float) -> str:
    """桁が大きい金額は万に丸める。80桁の端末で表が潰れるのを避ける。"""
    if abs(value) >= 100_000:
        return f"{value / 10_000:,.0f}万"
    return f"{value:,.0f}"


def _dir(config_dir: Path | None) -> Path:
    return config_dir or DEFAULT_CONFIG_DIR


def _load(config_dir: Path | None, *, allow_overlap: bool = False) -> AccumConfig:
    """設定を読む。失敗したら理由を出して終了する。"""
    try:
        return load(_dir(config_dir), allow_overlap=allow_overlap)
    except FileNotFoundError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(1) from None
    except ValueError as exc:
        console.print(f"[red]設定が不正です: {exc}[/red]")
        raise typer.Exit(1) from None


def _bars(settings: AppSettings, symbols: list[str]):  # type: ignore[no-untyped-def]
    """保存済みの足を読む。無い銘柄は警告して除く。"""
    from wbcore.data.store import BarStore

    store = BarStore(settings.bars_dir)
    bars = store.read_many(symbols)
    missing = sorted(set(symbols) - set(bars))
    if missing:
        console.print(
            f"[yellow]足データがありません: {', '.join(missing)}"
            f"（`accum sync` を実行してください）[/yellow]"
        )
    return bars


def _edgar_user_agent() -> str:
    """SEC が要求する連絡先入りの User-Agent。環境変数で上書きできる。"""
    import os

    return os.environ.get("WBJP_EDGAR_USER_AGENT", "wbjp research m.feat.ta@gmail.com")


def _basket_schedule(entry, settings: AppSettings, config_dir: Path | None):  # type: ignore[no-untyped-def]
    """バスケットの配分表を組み立てる。13F は保存済みの保有一覧から作る。"""
    from wbcore.data.edgar_13f import Edgar13F, load_cusip_map, weight_schedule

    if entry.source != "13f":
        return entry.build_schedule()
    holdings = Edgar13F(entry.cik, settings.data_dir / "13f", _edgar_user_agent()).load()
    if holdings.height == 0:
        return entry.build_schedule(None)
    pairs = weight_schedule(holdings, load_cusip_map(_dir(config_dir)), top=entry.top)
    return entry.build_schedule(pairs)


# --------------------------------------------------------------------------
# 設定の確認
# --------------------------------------------------------------------------


@app.command("strategies")
def strategies_list(config_dir: _ConfigDir = None) -> None:
    """使える戦略を一覧する。「この設定での使用」は ``accum.toml`` での登場箇所。"""
    from accum.registry import TACTICS
    from wbcore.registry import summary_of

    usage: dict[str, list[str]] = {}
    try:
        config = load(_dir(config_dir), allow_overlap=True)
    except (FileNotFoundError, ValueError) as exc:
        console.print(f"[yellow]設定を読めませんでした（登録済みのみ表示）: {exc}[/yellow]\n")
    else:
        # 節の名前は角括弧で始まる。Rich のマークアップと衝突するので逃がす。
        for entry in config.tactics:
            line = escape(f"[[tactics]] {entry.id}")
            usage.setdefault(entry.tactic, []).append(
                line if entry.enabled else f"[dim]{line}（停止）[/dim]"
            )
        for basket in config.baskets:
            line = escape(f"[[baskets]] {basket.id}")
            usage.setdefault(basket.tactic, []).append(
                line if basket.enabled else f"[dim]{line}（停止）[/dim]"
            )

    table = Table(title=f"積立戦略 ({_dir(config_dir) / FILENAME})", title_justify="left")
    table.add_column("戦略")
    table.add_column("説明")
    table.add_column("この設定での使用")
    for cls in TACTICS.classes():
        uses = usage.get(cls.name, [])
        table.add_row(cls.name, summary_of(cls), "\n".join(uses) if uses else "[dim]—[/dim]")
    console.print(table)
    console.print("\n[dim]銘柄への割り当ては `accum list`、今日の投下額は `accum plan`。[/dim]")


@app.command("list")
def list_assignments(config_dir: _ConfigDir = None) -> None:
    """戦略と銘柄の対応を一覧する。"""
    config = _load(config_dir, allow_overlap=True)

    table = Table(title="積立戦略の割り当て", title_justify="left")
    table.add_column("id")
    table.add_column("戦略")
    table.add_column("銘柄")
    table.add_column("市場")
    table.add_column("判定")
    table.add_column("発注時間帯")
    table.add_column("有効", justify="center")
    budgets: list[str] = []
    for entry in config.tactics:
        try:
            tactic = entry.build()
            described, window = tactic.describe(), tactic.window.describe()
        except ValueError as exc:
            described, window = f"[red]{exc}[/red]", "—"
        table.add_row(
            entry.id,
            described,
            ", ".join(entry.symbols),
            entry.market.value,
            entry.signal_symbol or "[dim]自身[/dim]",
            window,
            "○" if entry.enabled else "—",
        )
        budgets.append(f"{entry.id}: {entry.monthly_budget:,.0f}")
    console.print(table)
    if budgets:
        console.print(f"\n[dim]毎月の基本予算: {'、'.join(budgets)}[/dim]")

    # 一覧は重複を許して読む（止めてある戦略も見せたいため）。そのぶん
    # 衝突を検出できるのはここだけなので、必ず知らせる。
    try:
        config.validate_assignment()
    except ValueError as exc:
        console.print(f"\n[red]{exc}[/red]")
        raise typer.Exit(1) from None


# --------------------------------------------------------------------------
# データ
# --------------------------------------------------------------------------


def _sync(
    settings: AppSettings, config: AccumConfig, config_dir: Path | None, *, days: int, force: bool
) -> dict[str, int]:
    """積立対象（バスケットの構成銘柄と基準銘柄を含む）の足を更新する。"""
    from wbcore.data.registry import connect as connect_provider
    from wbcore.data.store import BarStore

    store = BarStore(settings.bars_dir)
    end = today_utc()
    start = end - dt.timedelta(days=days)

    # ティッカー変換が市場ごとに違う（1305→1305.T / VOO→VOO）ので分けて取る。
    # 積立は市場をまたぐのが普通なので、市場は銘柄ごとの設定から決める。
    grouped = config.symbols_by_market()
    for entry in config.active_baskets:
        try:
            symbols = _basket_schedule(entry, settings, config_dir).symbols
        except (ValueError, FileNotFoundError) as exc:
            console.print(f"[yellow]{entry.id}: {exc}[/yellow]")
            symbols = list(entry.weights)
        if entry.benchmark:
            symbols.append(entry.benchmark)
        grouped[entry.market] = sorted(set(grouped.get(entry.market, [])) | set(symbols))

    counts: dict[str, int] = {}
    for market, symbols in grouped.items():
        provider = connect_provider(config.data_provider, settings.env, market=market)
        counts.update(store.sync(provider, symbols, start, end, force=force))
    return counts


@app.command("sync")
def sync(
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
    settings = AppSettings()
    config = _load(config_dir)
    counts = _sync(settings, config, config_dir, days=days, force=force)
    for symbol, count in sorted(counts.items()):
        console.print(f"  {symbol}: {count} 本")


@app.command("sync-13f")
def sync_13f(config_dir: _ConfigDir = None) -> None:
    """バスケットが参照する 13F（機関投資家の保有報告）を EDGAR から取る。"""
    from wbcore.data.edgar_13f import Edgar13F

    settings = AppSettings()
    config = _load(config_dir)
    ciks = sorted({b.cik for b in config.active_baskets if b.source == "13f"})
    if not ciks:
        console.print("[yellow]13F を使うバスケットがありません[/yellow]")
        return
    for cik in ciks:
        client = Edgar13F(cik, settings.data_dir / "13f", _edgar_user_agent())
        frame = client.sync()
        periods = frame["period"].n_unique()
        console.print(
            f"  CIK {cik}: {periods} 四半期、{frame.height} 行 "
            f"({frame['period'].min()} 〜 {frame['period'].max()}) → {client.holdings_path}"
        )


# --------------------------------------------------------------------------
# 計画と発注
# --------------------------------------------------------------------------


@app.command("plan")
def plan(
    days: Annotated[int, typer.Option(help="直近何営業日ぶんを表示するか")] = 10,
    config_dir: _ConfigDir = None,
) -> None:
    """直近の投下額を銘柄ごとに表示する（今いくら買うか）。"""
    import polars as pl

    from accum.plan import AccumulationSettings, build_plan

    settings = AppSettings()
    config = _load(config_dir)
    bars = _bars(settings, config.all_symbols)
    if not bars:
        raise typer.Exit(1)

    total = 0
    blocked: list[str] = []
    for entry in config.active:
        tactic = entry.build()
        signal = bars.get(entry.signal_symbol) if entry.signal_symbol else None
        for symbol in entry.symbols:
            if symbol not in bars:
                continue
            frame = build_plan(
                bars[symbol],
                AccumulationSettings(entry.monthly_budget, tactic),
                signal_bars=signal,
                signal_strict=entry.signal_lags,
            )
            frame = frame.tail(days)

            window = tactic.window
            if window.enabled and not window.allows():
                blocked.append(f"{symbol}（{window.describe()}）")
            judged = f"  判定 {entry.signal_symbol}" if entry.signal_symbol else ""
            table = Table(
                title=f"{symbol} — {tactic.describe()}{judged}  発注 {window.describe()}",
                title_justify="left",
            )
            for column in ("日付", "終値", "倍率", "投下額", "理由"):
                table.add_column(column, justify="right" if column != "理由" else "left")
            for row in frame.iter_rows(named=True):
                amount = row["amount"]
                style = "bold" if amount > 0 else "dim"
                table.add_row(
                    str(row["date"]),
                    f"{row['close']:,.2f}",
                    f"{row['multiplier']:.2g}",
                    f"{amount:,}" if amount else "—",
                    row["reason"],
                    style=style,
                )
            console.print(table)
            total += int(frame.filter(pl.col("date") == frame["date"].max())["amount"][0])

    console.print(f"\n[bold]最終日の投下額 合計: {total:,}[/bold]")
    if blocked:
        now = now_utc()
        console.print(
            f"[yellow]いまは発注時間帯の外です（現在 {fmt(now, settings.timezone)}）: "
            f"{'、'.join(blocked)}[/yellow]\n"
            "[dim]※ 時間帯は投下額を変えない。日足で決まる金額を、"
            "いつ発注してよいかだけを制御する。[/dim]"
        )


class UnconfirmedOrderError(Exception):
    """発注を送ったが、届いたかどうか分からない（応答が返らなかった）。

    台帳には送信中のまま残っている。次の ``run`` は冒頭の照会で結果を取りに行き、
    ブローカーに無ければ送信中のまま「発注済み」に数え続ける——二重買付より
    買い漏れの方がましなため。``accum orders --check`` で確かめて、無ければ
    台帳の状態を手で REJECTED にする。
    """

    def __init__(self, client_order_id: str, cause: Exception) -> None:
        super().__init__(f"{cause}（client_order_id={client_order_id}）")
        self.client_order_id = client_order_id


def _place_recorded(
    broker: Broker, ledger: Ledger, request: OrderRequest, contribution: Contribution
) -> OrderAck:
    """台帳に**先に**記録してから発注する。

    送信後・記録前にプロセスが落ちる、あるいは注文は届いたのに応答がタイムアウトで
    例外になると、次の ``run`` が同じ注文を送り直す。先に「送信中」で記録して
    おけば :meth:`~accum.ledger.Ledger.was_placed` が真になり再送されない。

    - 受理された → その状態で上書き
    - 明確に拒否された（``OrderRejectedError``）→ REJECTED。次回の差額で埋め直す
    - それ以外（通信断・タイムアウト）→ 送信中のまま残し :class:`UnconfirmedOrderError`
    """
    from wbcore.broker.base import BrokerError, OrderRejectedError
    from wbcore.domain.models import OrderStatus

    c = contribution
    amount = _notional(request, c)
    ledger.record(
        request,
        OrderStatus.PENDING.value,
        plan_month=c.month,
        amount=amount,
        market=c.market,
    )
    try:
        ack = broker.place(request)
    except OrderRejectedError:
        ledger.update_status(request.client_order_id, OrderStatus.REJECTED)
        raise
    except BrokerError as exc:
        raise UnconfirmedOrderError(request.client_order_id, exc) from exc
    ledger.record(
        request,
        ack.status.value,
        ack.broker_order_id,
        plan_month=c.month,
        amount=amount,
        market=c.market,
    )
    return ack


def _notional(request: OrderRequest, contribution: Contribution) -> Decimal:
    """台帳に「発注済み」として残す額＝**株数 × 判断に使った価格**。

    差額（``contribution.amount``）をそのまま書くと、単元に丸めて買えなかった
    端数まで発注済みに数えられ、二度と買われない。実際に買う額を書けば
    端数は次回の差額に残り、次の月曜や翌月の入金日に繰り越される。
    """
    return request.quantity * contribution.close


@app.command("run")
def run(
    live: Annotated[
        bool,
        typer.Option("--live", help="注文を出す。無ければデータ取得と判断だけ行い、注文は出さない"),
    ] = False,
    no_sync: Annotated[bool, typer.Option("--no-sync", help="足の更新をしない")] = False,
    ignore_window: Annotated[
        bool, typer.Option("--ignore-window", help="発注時間帯の外でも注文を作る")
    ] = False,
    yes: Annotated[
        bool, typer.Option("--yes", "-y", help="本番の確認を省く（cron など非対話実行用）")
    ] = False,
    config_dir: _ConfigDir = None,
) -> None:
    """今日出すべき投下を注文にする。

    「今月の目標（今日まで）− 今月の発注済み」の差額を出す。判断は前日までの
    確定足、買うのは当日の価格（バックテストと同じ規則）。月の途中から始めても、
    予算を増やしても、cron が止まっても、差額が残っていれば次の実行で埋まる。
    既定は dry-run。実発注には --live が必要で、本番では WBJP_ENV=prod も
    同時に必要。cron から回すときは --yes も付ける。
    """
    from wbcore.notify import alert

    try:
        _run(
            live=live,
            no_sync=no_sync,
            ignore_window=ignore_window,
            yes=yes,
            config_dir=config_dir,
        )
    except typer.Exit, typer.Abort:
        raise
    except Exception as exc:
        # cron / systemd では誰も端末を見ていない。理由を通知してから落とす
        log.exception("積立の実行が異常終了", code="accum.crash", error=str(exc))
        alert("積立: 実行が異常終了", f"{type(exc).__name__}: {exc}")
        raise typer.Exit(1) from exc


def _run(
    *,
    live: bool,
    no_sync: bool,
    ignore_window: bool,
    yes: bool,
    config_dir: Path | None,
) -> None:
    from accum.execute import (
        build_orders,
        ledger_symbol,
        pending_contributions,
        resolve_lot_sizes,
        should_fallback_to_limit,
        sync_order_status,
        to_order,
        unrecorded_fills,
    )
    from accum.ledger import DRY_RUN_STATUS, Ledger
    from wbcore.broker.base import BrokerError
    from wbcore.broker.registry import connect
    from wbcore.clock import to_zone
    from wbcore.domain.models import Market, OrderType
    from wbcore.notify import alert

    settings = AppSettings()
    try:
        config = load(_dir(config_dir))
    except (FileNotFoundError, ValueError) as exc:
        # cron では誰も端末を見ていない。設定の壊れで積立が止まっていることを通知する
        console.print(f"[red]設定を読めません: {exc}[/red]")
        log.error("積立の設定を読めず停止", code="accum.config", error=str(exc))
        alert("積立: 設定を読めず停止", str(exc))
        raise typer.Exit(1) from None
    console.print(describe_mode(settings.env, live, kill_switch=config.kill_switch))
    now = now_utc()
    # cron は 20 分おきに一日中叩く。発注できない時間帯に足の同期やブローカー接続まで
    # 行うと、取得元が落ちている間は 20 分ごとに通知が飛ぶ。--live のときは窓の外なら
    # ここで終える（dry-run は確認用なので、いつ実行しても判断まで見せる）
    if live and not ignore_window and not any(e.build().allows_order(now) for e in config.active):
        windows = "、".join(sorted({e.build().window.describe() for e in config.active}))
        console.print(f"[dim]発注時間帯の外（{windows}）。何もしません[/dim]")
        log.info("発注時間帯の外", code="accum.skip", windows=windows)
        return
    if not no_sync:
        _sync(settings, config, config_dir, days=30, force=False)
    bars = _bars(settings, config.all_symbols)
    ledger = Ledger(settings.data_dir / f"accum-{settings.env.value}.db")

    # 市場ごとにブローカーを1つ。積立は市場をまたぐのが普通。
    brokers: dict[Market, Broker] = {}

    def broker_for(market: Market) -> Broker:
        if market not in brokers:
            brokers[market] = connect(
                config.execution.broker,
                settings.env,
                market=market,
                tax_type=config.execution.tax_account_type,
                extended_hours=config.execution.extended_hours,
                notify=lambda message: console.print(f"[yellow]{message}[/yellow]"),
            )
        return brokers[market]

    # 前回までに送った注文がどうなったかを先に確かめる。失効・拒否なら
    # 「発注済み」から外れ、この後の差額の計算で自動的に埋め直される
    if ledger.open_orders():
        try:
            changes = sync_order_status(ledger, broker_for, now=now)
        except BrokerError as exc:
            console.print(f"[yellow]注文の照会に失敗（前回の状態のまま続けます）: {exc}[/yellow]")
            changes = []
        lost = [c for c in changes if c.lost_amount_ratio > 0]
        for change in changes:
            style = "red" if change.lost_amount_ratio > 0 else "dim"
            console.print(f"[{style}]前回の注文: {change.describe()}[/{style}]")
            log.info(
                "前回の注文の約定状況",
                code="accum.fill",
                symbol=change.symbol,
                client_order_id=change.client_order_id,
                before=change.before,
                after=change.after.value,
                filled=str(change.filled_quantity),
                quantity=str(change.quantity),
                lost_ratio=str(change.lost_amount_ratio),
            )
        if lost:
            alert(
                f"積立: {len(lost)} 件の注文が未約定のまま終了",
                "\n".join(c.describe() for c in lost),
            )

    allowed, reason = allows_live_orders(settings.env, live, kill_switch=config.kill_switch)

    def started(symbol: str) -> dt.date:
        """積立の開始日。初めて本発注に来た日を台帳に残す（dry-run は確定させない）。"""
        key = ledger_symbol(config, symbol)
        day = ledger.started_on(key)
        if day is None:
            market = config.market_of(symbol)
            day = to_zone(now, market.timezone if market else None).date()
            if allowed:
                ledger.mark_started(key, day)
        return day

    pending = pending_contributions(
        config,
        bars,
        now=now,
        started=started,
        # 台帳はブローカーの表記（452A）、設定は足の表記（452A.T）。揃えないと
        # 発注済みが常に 0 に見え、同じ月の予算を毎回買い直す
        placed=lambda symbol, month: ledger.placed_amount(ledger_symbol(config, symbol), month),
        active=lambda symbol, month: ledger.has_orders(ledger_symbol(config, symbol), month),
        max_stale_days=config.execution.max_stale_days,
    )
    if pending.stale:
        detail = "、".join(f"{s}（最終 {d}）" for s, d in sorted(pending.stale.items()))
        console.print(f"[red]足が古いため判定しませんでした: {detail}[/red]")
        log.warning(
            "足が古いため判定を見送り",
            code="accum.stale",
            symbols={s: d.isoformat() for s, d in pending.stale.items()},
        )
        alert("積立: 足が古いため判定を見送り", detail)
    if pending.stale_signals:
        detail = "、".join(f"{s}（最終 {d}）" for s, d in sorted(pending.stale_signals.items()))
        console.print(f"[yellow]判定用の足が古いため倍率が最新ではありません: {detail}[/yellow]")
        log.warning(
            "判定用の足が古い（投下は続けるが倍率は古い足のまま）",
            code="accum.stale_signal",
            symbols={s: d.isoformat() for s, d in pending.stale_signals.items()},
        )
        alert("積立: 判定用の足が古い（投下は続行）", detail)
    for symbol, due in sorted(pending.deferred.items()):
        console.print(
            f"[dim]{symbol}: 差額 {due:,.0f} はリリース日（入金日・増額の日）まで持ち越し[/dim]"
        )
        log.info("差額をリリース日まで持ち越し", code="accum.deferred", symbol=symbol, due=str(due))
    contributions = pending.contributions
    for c in contributions:
        log.info(
            "積立の判断",
            code="accum.decision",
            symbol=c.symbol,
            market=c.market.value,
            month=c.month.isoformat() if c.month else None,
            judged_on=c.date.isoformat(),
            target=str(c.target),
            placed=str(c.placed),
            due=str(c.amount),
            multiplier=c.multiplier,
            price=str(c.close),
            tactic=c.tactic.describe(),
            signal=next((e.signal_symbol for e in config.active if c.symbol in e.symbols), None),
        )
    if not contributions:
        ledger.close()
        console.print("[dim]出すべき投下はありません（今月の目標に達しています）[/dim]")
        return

    # 台帳に無い当月の約定がブローカーにあれば、台帳を失っている。買い直す前に止める
    if allowed:
        try:
            unknown = unrecorded_fills(
                ledger, broker_for, contributions, today=to_zone(now, settings.timezone).date()
            )
        except NotImplementedError:
            unknown = {}
        except BrokerError as exc:
            console.print(f"[yellow]注文履歴を照会できず、台帳との突合を省略: {exc}[/yellow]")
            log.warning("注文履歴を照会できず突合を省略", code="accum.reconcile", error=str(exc))
            unknown = {}
        if unknown:
            detail = "、".join(f"{s}（約定 {a:,.0f}）" for s, a in sorted(unknown.items()))
            console.print(
                f"[red]台帳に無い当月の約定がブローカーにあります。台帳を失っていませんか: "
                f"{detail}。発注を止めます[/red]"
            )
            log.error("台帳に無い当月の約定", code="accum.reconcile", fills=str(unknown))
            alert("積立: 台帳に無い当月の約定があるため発注を停止", detail)
            ledger.close()
            raise typer.Exit(1)

    # 売買単位は設定より銘柄マスタを信じる（設定の書き間違いで拒否されるより良い）
    lots = resolve_lot_sizes(contributions, config.execution.lot_size_overrides, broker_for)
    for symbol, (configured, api) in sorted(lots.mismatches.items()):
        console.print(
            f"[yellow]{symbol}: 売買単位が設定（{configured}）と銘柄情報（{api}）で異なるため "
            f"{api} を使います。lot_size_overrides を直してください[/yellow]"
        )
        log.warning(
            "売買単位が設定と銘柄情報で食い違い",
            code="accum.lot_size",
            symbol=symbol,
            configured=configured,
            api=api,
        )
    for market, why in lots.failures.items():
        log.warning(
            "銘柄情報を照会できず設定の売買単位で進行",
            code="accum.lot_size",
            market=market.value,
            error=why,
        )
    planned = build_orders(
        contributions,
        tax_type=config.execution.tax_account_type,
        lot_sizes=lots.sizes,
        moment=now,
        ignore_window=ignore_window,
        order_type=OrderType(config.execution.order_type.upper()),
        limit_offset=config.execution.limit_offset,
    )
    if allowed and settings.env.is_production and not yes:
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

    status: dict[str, str] = {}
    failures: list[str] = []
    # 市場ごとの買付余力。同じ実行で複数銘柄を出すとき、先に出した分を引いて
    # 次を判断する（毎回同じ余力と比べると 2 銘柄目以降が余力を超えて通る）
    remaining: dict[Market, Decimal] = {}
    try:
        for item in planned:
            c = item.contribution
            key = f"{c.symbol} {c.date}"
            if item.request is None:
                status[key] = f"[yellow]見送り[/yellow] {item.note}"
                continue
            # 台帳にあれば同じ判断からの再実行（祝日で足が増えない日など）。送らない。
            if ledger.was_placed(item.request.client_order_id):
                status[key] = "[dim]発注済み（冪等）[/dim]"
                continue
            if not allowed:
                ledger.record(
                    item.request,
                    DRY_RUN_STATUS,
                    plan_month=c.month,
                    amount=_notional(item.request, c),
                    market=c.market,
                )
                status[key] = "[yellow]dry-run[/yellow]"
                continue
            try:
                broker = broker_for(c.market)
                # 発注前にブローカーの見積りと買付余力を突き合わせる。
                # 余力不足で拒否されるより、こちらで止めて理由を残す方が追える。
                preview = broker.preview(item.request)
                cost = preview.estimated_cost + preview.estimated_fee
                if c.market not in remaining:
                    remaining[c.market] = broker.get_balance().buying_power
                buying_power = remaining[c.market]
                if cost > buying_power:
                    note = f"買付余力不足（必要 {cost:,.0f} / 余力 {buying_power:,.0f}）"
                    status[key] = f"[red]見送り[/red] {note}"
                    failures.append(f"{key}: {note}")
                    continue
                remaining[c.market] = buying_power - cost
                request = item.request
                try:
                    _place_recorded(broker, ledger, request, c)
                except BrokerError as exc:
                    if not (
                        config.execution.fallback_to_limit
                        and should_fallback_to_limit(request, exc)
                    ):
                        raise
                    # 気配値が無いと成行は通らない（UAT の新規上場 ETF で実測）。
                    # 同じ内容を指値で出し直す。上限価格が付くぶん不利にはならない
                    request = to_order(
                        c,
                        tax_type=config.execution.tax_account_type,
                        lot_size=lots.sizes.get(c.symbol),
                        order_type=OrderType.LIMIT,
                        limit_offset=config.execution.limit_offset,
                        seed="accum-limit",
                    )
                    log.warning(
                        "成行が気配値無しで拒否されたため指値で出し直し",
                        code="accum.fallback_limit",
                        symbol=c.symbol,
                        limit_price=str(request.limit_price),
                    )
                    _place_recorded(broker, ledger, request, c)
                    status[key] = f"[green]発注[/green]（指値 {request.limit_price:,} に切替）"
            except UnconfirmedOrderError as exc:
                # 送ったかどうか分からない。台帳には送信中として残してあるので
                # 次回は再送されない（二重買付より買い漏れの方がまし）。人が確かめる
                status[key] = f"[red]要確認[/red] {exc}"
                failures.append(f"{key}: {exc}")
                log.error(
                    "発注の結果を確認できず",
                    code="accum.unconfirmed",
                    symbol=c.symbol,
                    client_order_id=exc.client_order_id,
                    error=str(exc),
                )
            except BrokerError as exc:
                status[key] = f"[red]失敗[/red] {exc}"
                failures.append(f"{key}: {exc}")
                log.error("積立の発注に失敗", symbol=c.symbol, error=str(exc))
            else:
                status.setdefault(key, "[green]発注[/green]")
    finally:
        ledger.close()
    for item in planned:
        c = item.contribution
        log.info(
            "積立の注文",
            code="accum.order",
            symbol=c.symbol,
            market=c.market.value,
            month=c.month.isoformat() if c.month else None,
            client_order_id=item.request.client_order_id if item.request else None,
            order_type=item.request.order_type.value if item.request else None,
            quantity=str(item.request.quantity) if item.request else None,
            price=str(c.close),
            amount=str(c.amount),
            live=allowed,
            outcome=_plain(status[f"{c.symbol} {c.date}"]),
            note=item.note or None,
        )
    log.info(
        "積立の実行を終了",
        code="accum.run",
        live=allowed,
        reason=reason,
        orders=len(planned),
        failures=len(failures),
    )
    if failures:
        alert(f"積立: {len(failures)} 件の発注に失敗", "\n".join(failures))

    mode = "[green]発注あり[/green]" if allowed else f"[yellow]発注なし[/yellow]（{reason}）"
    console.print(
        f"\n[bold]積立[/bold]  基準日 {planned[0].contribution.date}  "
        f"{fmt(now, settings.timezone)}  {mode}"
    )
    # 内訳は「目標・繰り越し・発注済み」の式で長い。短い列は折り返さず、残りの幅を
    # 内訳 : 状態 = 3 : 1 で分ける（expand が無いと ratio は効かない）
    table = Table(title="注文", title_justify="left", expand=True)
    for column in ("銘柄", "判断日", "市場", "価格", "投下額", "倍率", "株数"):
        table.add_column(
            column,
            justify="right" if column in ("価格", "投下額", "株数") else "left",
            no_wrap=True,
        )
    table.add_column("状態", ratio=1)
    table.add_column("内訳", ratio=3)
    for item in planned:
        c = item.contribution
        table.add_row(
            c.symbol,
            str(c.date),
            c.market.value,
            f"{c.close:,.2f}",
            f"{c.amount:,.0f}",
            f"×{c.multiplier:g}",
            f"{item.request.quantity:,}" if item.request else "—",
            status[f"{c.symbol} {c.date}"],
            c.reason,
        )
    console.print(table)
    if failures:
        # 通知は出しているが、systemd / cron にも失敗として見せる
        raise typer.Exit(1)


@app.command("backup")
def backup(
    dest: Annotated[
        Path | None, typer.Option(help="保存先ディレクトリ（既定 data/backup）")
    ] = None,
    keep: Annotated[int, typer.Option(help="残す世代数。古いものから消す")] = 30,
) -> None:
    """台帳（data/accum-<env>.db）を日付付きで複製する。cron で毎日回す。

    台帳は「今月いくら発注済みか」の唯一の記録で、ブローカーから再構築できない。
    失うと次の実行で当月の予算を買い直す。
    """
    from accum.ledger import Ledger

    settings = AppSettings()
    directory = dest or settings.data_dir / "backup"
    stem = f"accum-{settings.env.value}"
    target = directory / f"{stem}-{today_utc():%Y%m%d}.db"
    with Ledger(settings.data_dir / f"{stem}.db") as ledger:
        ledger.backup(target)
    console.print(f"複製しました: {target}")
    old = sorted(directory.glob(f"{stem}-*.db"))[:-keep] if keep > 0 else []
    for path in old:
        path.unlink()
    if old:
        console.print(f"[dim]古い複製を {len(old)} 件削除[/dim]")
    log.info("台帳を複製", code="accum.backup", path=str(target), removed=len(old))


@app.command("orders")
def orders(
    limit: Annotated[int, typer.Option(help="表示件数")] = 20,
    check: Annotated[
        bool, typer.Option("--check", help="結果が確定していない注文をブローカーに照会して更新する")
    ] = False,
    config_dir: _ConfigDir = None,
) -> None:
    """送った注文とその約定状況（台帳）を表示する。

    「発注済み」に数える額は、生きている注文と約定した分だけ。失効・拒否の
    未約定分は数えず、次の `run` で差額として埋め直される。
    """
    from accum.execute import sync_order_status
    from accum.ledger import Ledger
    from wbcore.broker.base import BrokerError
    from wbcore.broker.registry import connect
    from wbcore.clock import fmt_iso
    from wbcore.domain.models import Market

    settings = AppSettings()
    config = _load(config_dir, allow_overlap=True)
    ledger = Ledger(settings.data_dir / f"accum-{settings.env.value}.db")
    try:
        if check:
            brokers: dict[Market, Broker] = {}

            def broker_for(market: Market) -> Broker:
                if market not in brokers:
                    brokers[market] = connect(
                        config.execution.broker,
                        settings.env,
                        market=market,
                        tax_type=config.execution.tax_account_type,
                        notify=lambda message: console.print(f"[yellow]{message}[/yellow]"),
                    )
                return brokers[market]

            try:
                for change in sync_order_status(ledger, broker_for):
                    console.print(f"更新: {change.describe()}")
            except BrokerError as exc:
                console.print(f"[red]照会に失敗: {exc}[/red]")
                raise typer.Exit(1) from None

        rows = ledger.recent(limit)
        if not rows:
            console.print("[dim]台帳に注文はありません[/dim]")
            return
        table = Table(title=f"積立の注文（{settings.env.value}）", title_justify="left")
        for column in (
            "送信",
            "銘柄",
            "月",
            "投下額",
            "株数",
            "約定",
            "平均価格",
            "状態",
            "有効額",
        ):
            table.add_column(
                column,
                justify="right"
                if column in ("投下額", "株数", "約定", "平均価格", "有効額")
                else "left",
            )
        for o in rows:
            dead = o.status in {"CANCELLED", "REJECTED", "EXPIRED"}
            table.add_row(
                fmt_iso(o.placed_at, settings.timezone),
                o.symbol,
                f"{o.plan_month:%Y-%m}" if o.plan_month else "—",
                f"{o.amount:,.0f}" if o.amount is not None else "—",
                f"{o.quantity.normalize():,f}",
                f"{o.filled_quantity.normalize():,f}",
                f"{o.avg_fill_price:,.2f}" if o.avg_fill_price is not None else "—",
                f"[red]{o.status}[/red]" if dead else o.status,
                f"{o.effective_amount:,.0f}",
            )
        console.print(table)
        console.print(
            "[dim]有効額＝「発注済み」に数える額。失効・拒否は約定ぶんだけ。dry-run は 0[/dim]"
        )
    finally:
        ledger.close()


# --------------------------------------------------------------------------
# 検証
# --------------------------------------------------------------------------


@app.command("backtest")
def backtest(
    from_: Annotated[str | None, typer.Option("--from", help="開始日 YYYY-MM-DD")] = None,
    to: Annotated[str | None, typer.Option("--to", help="終了日 YYYY-MM-DD")] = None,
    config_dir: _ConfigDir = None,
) -> None:
    """設定どおりに積み立てた場合の結果を銘柄ごとに出す。"""
    import polars as pl

    from accum.plan import AccumulationSettings, build_plan
    from accum.simulate import simulate

    settings = AppSettings()
    config = _load(config_dir)
    start = dt.date.fromisoformat(from_) if from_ else None
    end = dt.date.fromisoformat(to) if to else None
    bars = _bars(settings, config.all_symbols)
    if not bars:
        raise typer.Exit(1)

    table = Table(title="積立バックテスト", title_justify="left")
    table.add_column("銘柄")
    table.add_column("戦略")  # 設定の id。describe() は長すぎて表が潰れる
    for column in ("投入", "倍率", "単価", "対照比", "期末", "ﾘﾀｰﾝ"):
        table.add_column(column, justify="right")

    for entry in config.active:
        for symbol in entry.symbols:
            if symbol not in bars:
                continue
            tactic = entry.build()
            frame = bars[symbol]
            if start or end:
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

            signal = bars.get(entry.signal_symbol) if entry.signal_symbol else None
            if start or end:
                signal = (
                    None
                    if signal is None
                    else signal.filter(pl.col("date") <= (end or dt.date.max))
                )
            budget = entry.monthly_budget
            plan_frame = build_plan(
                frame,
                AccumulationSettings(budget, tactic),
                signal_bars=signal,
                signal_strict=entry.signal_lags,
            )
            result = simulate(frame, plan_frame, monthly_budget=budget)
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
        "\n[dim]※ 倍率＝基本予算だけの場合に対する総投入額の倍率。単価/投入/期末は万。\n"
        "※ 対照群＝同じ総投入額を毎月均等に投じた場合。マイナスなら安く買えた。\n"
        "　 増額分の原資は新規資金（賞与など）を前提としている。積立予算を取り置いて\n"
        "　 作ると待機が生じ、増額の利益を打ち消す。[/dim]"
    )


@app.command("basket")
def basket(
    from_: Annotated[str | None, typer.Option("--from", help="開始日 YYYY-MM-DD")] = None,
    to: Annotated[str | None, typer.Option("--to", help="終了日 YYYY-MM-DD")] = None,
    show_weights: Annotated[
        bool, typer.Option("--weights", help="いま有効な配分を表示する")
    ] = False,
    config_dir: _ConfigDir = None,
) -> None:
    """バスケット（複数銘柄への配分）で積み立てた結果を基準銘柄と比べる。

    足は ``accum sync``、13F は ``accum sync-13f`` で先に取る。
    """
    import polars as pl

    from accum.basket import BasketSettings, build_basket_plan, simulate_basket

    settings = AppSettings()
    config = _load(config_dir, allow_overlap=True)
    if not config.active_baskets:
        console.print("[yellow]有効なバスケットがありません[/yellow]")
        raise typer.Exit(1)
    start = dt.date.fromisoformat(from_) if from_ else None
    end = dt.date.fromisoformat(to) if to else None

    table = Table(title="バスケット積立（万）", title_justify="left")
    table.add_column("id")
    table.add_column("開始")
    for column in ("投入", "期末", "XIRR", "DD", "基準期末", "基準XIRR", "基準DD"):
        table.add_column(column, justify="right")

    result = None
    for entry in config.active_baskets:
        try:
            schedule = _basket_schedule(entry, settings, config_dir)
        except (ValueError, FileNotFoundError) as exc:
            console.print(f"[red]{exc}[/red]")
            continue
        symbols = schedule.symbols
        needed = symbols + ([entry.benchmark] if entry.benchmark else [])
        bars = _bars(settings, needed)
        if start or end:
            bars = {
                s: f.filter(
                    (pl.col("date") >= (start or dt.date.min))
                    & (pl.col("date") <= (end or dt.date.max))
                )
                for s, f in bars.items()
            }
        benchmark = bars.pop(entry.benchmark, None) if entry.benchmark else None
        if entry.benchmark and benchmark is not None and entry.benchmark in symbols:
            bars[entry.benchmark] = benchmark  # 基準が構成銘柄でもある場合（VOO単独など）
        bars = {s: f for s, f in bars.items() if s in symbols and f.height > 0}
        if not bars:
            continue

        if show_weights:
            latest = schedule.at(today_utc())
            console.print(
                f"[bold]{entry.id}[/bold] いま有効な配分: "
                + ", ".join(f"{s} {w:.1%}" for s, w in sorted(latest.items(), key=lambda x: -x[1]))
            )

        basket_settings = BasketSettings(
            entry.monthly_budget or config.monthly_budget,
            schedule,
            entry.build_tactic(),
            entry.build_tilt(),
        )
        try:
            result = simulate_basket(
                bars, build_basket_plan(bars, basket_settings), benchmark=benchmark
            )
        except ValueError as exc:
            console.print(f"[yellow]{entry.id}: {exc}[/yellow]")
            continue
        b, bm = result.basket, result.benchmark
        better = bm is None or b.xirr >= bm.xirr
        table.add_row(
            entry.id,
            f"{result.start:%Y-%m}",
            _yen(float(b.contributed)),
            _yen(b.terminal_value),
            f"[green]{b.xirr:+.1%}[/green]" if better else f"[red]{b.xirr:+.1%}[/red]",
            f"{b.max_drawdown:.0%}",
            _yen(bm.terminal_value) if bm else "—",
            f"{bm.xirr:+.1%}" if bm else "—",
            f"{bm.max_drawdown:.0%}" if bm else "—",
        )

    console.print(table)
    if result is None:
        return
    console.print(
        f"\n[dim]※ 終了 {result.end}。基準＝同じ日に同じ額を benchmark に投じた場合。"
        "XIRR は年率の内部収益率。\n"
        "※ 最大DD は時間加重の評価額指数から。投下額の増減による見かけの変動は含まない。\n"
        "※ 13F の配分は提出日の翌営業日から反映。買収・上場廃止で足の無い銘柄は除いて\n"
        "　 正規化しており、成績はやや控えめに出る（買収は通常プレミアム付き）。[/dim]"
    )


@app.command("compare")
def compare(
    symbol: Annotated[str, typer.Argument(help="比較したい銘柄")],
    config_dir: _ConfigDir = None,
) -> None:
    """1銘柄に対して、登録済みの戦略を既定パラメータで並べて比較する。"""
    from accum.plan import AccumulationSettings, build_plan
    from accum.registry import TACTICS
    from accum.simulate import simulate

    settings = AppSettings()
    config = _load(config_dir, allow_overlap=True)
    bars = _bars(settings, [symbol])
    if symbol not in bars:
        raise typer.Exit(1)
    frame = bars[symbol]

    table = Table(title=f"{symbol} — 戦略の比較", title_justify="left")
    table.add_column("戦略")
    for column in ("倍率", "単価", "対照比", "1倍あたり", "期末"):
        table.add_column(column, justify="right")

    for name in TACTICS.available():
        tactic = TACTICS.create(name)
        if frame.height < tactic.warmup_bars:
            continue
        plan_frame = build_plan(frame, AccumulationSettings(config.monthly_budget, tactic))
        result = simulate(frame, plan_frame, monthly_budget=config.monthly_budget)
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
        f"\n[dim]※ 既定パラメータでの比較。倍率などは {_dir(config_dir) / FILENAME} で調整する。\n"
        "　 「1倍あたり」は追加資金1倍あたりの単価改善で、資金効率の指標。[/dim]"
    )
