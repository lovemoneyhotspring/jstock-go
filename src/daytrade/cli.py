"""デイトレのコマンドライン（``daytrade``）。

安全の原則は ``wbjp`` / ``accum`` と同じ: ``open`` / ``close`` は既定で **dry-run**。
実際に発注するには ``--live`` が要り、本番口座ではさらに ``WBJP_ENV=prod`` が要る。

1 日の流れ（すべて JST）:
    20:30  ``daytrade plan``          前夜。アーカイブから翌営業日の母集団を作る
    09:00  ``daytrade open --live``   気配でギャップ下位 N 銘柄を選び、成行で買う
    15:20  ``daytrade close --live``  当日買った分を成行で売る（15:25 以降ならクロージング・オークションで引け値）
"""

from __future__ import annotations

import datetime as dt
import functools
import re
import sys
from collections.abc import Callable
from decimal import Decimal
from pathlib import Path
from typing import TYPE_CHECKING, Annotated, Any, ParamSpec, TypeVar

import typer
from rich.console import Console
from rich.table import Table

from daytrade.config import DEFAULT_CONFIG_DIR, DaytradeConfig, load
from wbcore.clock import fmt, now_utc, to_zone
from wbcore.domain.models import Market
from wbcore.logging import bind_run_context, configure_logging, get_logger
from wbcore.settings import AppSettings, allows_live_orders, describe_mode

if TYPE_CHECKING:
    from daytrade.ledger import Ledger
    from daytrade.plan import Plan
    from daytrade.select import Pick, Quote
    from wbcore.broker.base import Broker
    from wbcore.data.jquants_archive import Archive
    from wbcore.domain.models import OrderRequest

app = typer.Typer(
    help="デイトレ（寄付で買い、大引で売る）", no_args_is_help=True, add_completion=False
)
console = Console()
log = get_logger(__name__)

_ConfigDir = Annotated[
    Path | None, typer.Option(help=f"設定ディレクトリ（既定 {DEFAULT_CONFIG_DIR}）")
]
_Date = Annotated[
    str | None, typer.Option("--date", help="判定日（YYYY-MM-DD、既定は今日／次の営業日）")
]

JST = Market.JP.timezone
#: ログに残す銘柄名の見本の上限（全部は長い。件数は別に残す）。
SAMPLE = 20
P = ParamSpec("P")
R = TypeVar("R")


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
        log_file=settings.log_file("daytrade"),
    )
    run_id = bind_run_context(
        app="daytrade", env=settings.env.value, command=ctx.invoked_subcommand or ""
    )
    ctx.obj = {"settings": settings, "run_id": run_id}


# --------------------------------------------------------------------------
# 補助
# --------------------------------------------------------------------------


def _plain(markup: str) -> str:
    return re.sub(r"\[/?[a-z ]+\]", "", markup)


def _yen(value: Decimal | float | int) -> str:
    return f"{float(value):,.0f}"


def _settings(ctx: typer.Context) -> AppSettings:
    settings: AppSettings = ctx.obj["settings"]
    return settings


def _log_config(config: DaytradeConfig, settings: AppSettings, **extra: Any) -> None:
    """実行時の設定を 1 レコードに残す。不具合の再現には「そのとき何が有効だったか」が要る。"""
    r = config.regime
    log.info(
        "実行時の設定",
        code="daytrade.config",
        strategy="jp_gap_fade",
        env=settings.env.value,
        enabled=config.capital.enabled,
        max_capital=str(config.capital.max_capital),
        order_budget=str(config.capital.order_budget),
        positions=config.capital.positions,
        budget_per_order=str(config.capital.budget_per_order),
        segments=config.universe.segments,
        min_turnover=str(config.universe.min_turnover),
        exclude_cap_terciles=config.universe.exclude_cap_terciles,
        max_gap=str(config.signal.max_gap),
        min_gap=str(config.signal.min_gap),
        skip_months=r.skip_months,
        iv_gate=str(r.iv_gate),
        drift_gate=str(r.drift_gate) if r.drift_gate is not None else None,
        equity_curve_days=r.equity_curve_days,
        equity_curve_scale=str(r.equity_curve_scale),
        weighting=config.capital.weighting,
        us_skip=[str(r.us_skip_low), str(r.us_skip_high)] if r.us_skip_high is not None else None,
        us_vix_override=str(r.us_vix_override),
        quote_source=config.execution.quote_source,
        entry_window=list(config.execution.entry_window),
        exit_window=list(config.execution.exit_window),
        max_quote_age=config.execution.max_quote_age,
        kill_switch=config.execution.kill_switch,
        state_dir=str(settings.state_dir),
        data_dir=str(settings.data_dir),
        **extra,
    )


def _load(config_dir: Path | None) -> DaytradeConfig:
    try:
        return load(config_dir or DEFAULT_CONFIG_DIR)
    except FileNotFoundError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(1) from None
    except ValueError as exc:
        console.print(f"[red]設定が不正です: {exc}[/red]")
        raise typer.Exit(1) from None


def _archive(settings: AppSettings) -> Archive:
    from wbcore.data.jquants_archive import Archive

    return Archive(settings.data_dir / "jquants")


def _parse_date(text: str | None) -> dt.date | None:
    if text is None:
        return None
    try:
        return dt.date.fromisoformat(text)
    except ValueError:
        console.print(f"[red]日付の形式が不正です: {text}（YYYY-MM-DD）[/red]")
        raise typer.Exit(1) from None


def _today_jst(now: dt.datetime | None = None) -> dt.date:
    return to_zone(now or now_utc(), JST).date()


def _in_window(config: DaytradeConfig, name: str, now: dt.datetime) -> bool:
    start, end = config.execution.window(name)
    local = to_zone(now, JST).time()
    return start <= local <= end


def _describe_window(config: DaytradeConfig, name: str) -> str:
    start, end = config.execution.window(name)
    return f"{start:%H:%M}〜{end:%H:%M} JST"


def _is_trading_day(settings: AppSettings, day: dt.date) -> bool:
    """東証の営業日か。カレンダーが無ければ平日扱い。"""
    from daytrade.calendar import TradingCalendar

    return TradingCalendar.from_archive(_archive(settings)).is_trading_day(day)


def _skip_holiday(settings: AppSettings, day: dt.date, phase: str) -> bool:
    """休場日なら何もしない（気配が取れずに毎回アラートを飛ばさないため）。"""
    if _is_trading_day(settings, day):
        return False
    console.print(f"[dim]{day} は休場日。何もしません[/dim]")
    log.info("休場日", code="daytrade.skip", reason="holiday", phase=phase, day=day.isoformat())
    return True


def _connect(settings: AppSettings, config: DaytradeConfig) -> Broker:
    from wbcore.broker.registry import connect

    return connect(
        config.execution.broker,
        settings.env,
        market=Market.JP,
        tax_type=config.execution.tax_account_type,
        notify=lambda message: console.print(f"[yellow]{message}[/yellow]"),
    )


def _confirm_live(settings: AppSettings, allowed: bool, yes: bool) -> None:
    if allowed and settings.env.is_production and not yes:
        console.print("[bold red]本番環境で実際に発注します[/bold red]")
        if not sys.stdin.isatty():
            console.print(
                "[red]非対話環境では確認を取れません。cron から回すなら --yes を付けてください[/red]"
            )
            raise typer.Exit(1)
        if not typer.confirm("続行しますか?"):
            raise typer.Abort()


def _crash(title: str, code: str) -> Callable[[Callable[P, R]], Callable[P, R]]:
    """cron では誰も端末を見ていない。理由を通知してから落とす。"""

    def wrap(func: Callable[P, R]) -> Callable[P, R]:
        @functools.wraps(func)
        def inner(*args: P.args, **kwargs: P.kwargs) -> R:
            from wbcore.notify import alert

            try:
                return func(*args, **kwargs)
            except typer.Exit, typer.Abort:
                raise
            except Exception as exc:
                log.exception(f"{title}が異常終了", code=code, error=str(exc))
                alert(f"デイトレ: {title}が異常終了", f"{type(exc).__name__}: {exc}")
                raise typer.Exit(1) from exc

        return inner

    return wrap


# --------------------------------------------------------------------------
# plan
# --------------------------------------------------------------------------


def _plan_day(settings: AppSettings, date: dt.date | None, now: dt.datetime) -> dt.date:
    """判定日。指定が無ければ「引け前なら今日、引け後なら次の営業日」。"""
    from daytrade.calendar import TradingCalendar

    if date is not None:
        return date
    cal = TradingCalendar.from_archive(_archive(settings))
    local = to_zone(now, JST)
    if local.time() < dt.time(15, 30):
        return cal.next_trading_day(local.date(), inclusive=True)
    return cal.next_trading_day(local.date())


def _build_plan(settings: AppSettings, config: DaytradeConfig, day: dt.date) -> Plan:
    from daytrade import plan as planning

    plan = planning.build(_archive(settings), config, day)
    parquet, _ = planning.save(plan, settings.daytrade_dir)
    log.info(
        "候補を作成",
        code="daytrade.plan",
        day=plan.meta.day,
        prev_day=plan.meta.prev_day,
        candidates=plan.meta.candidates,
        eligible=plan.meta.eligible,
        positions=plan.meta.positions,
        budget=plan.meta.budget_per_order,
        iv_prev=plan.meta.iv_prev,
        path=str(parquet),
    )
    return plan


def _refresh_iv(settings: AppSettings, config: DaytradeConfig, plan: Plan) -> Plan:
    """前夜の plan に IV が無ければ取り直す。

    オプションの足は 27:00 頃の更新なので、20:30 の plan には前日の IV がまだ無い。
    朝の sync で入っていればここで拾う。それでも無ければゲートは効かせず取引する
    （低 IV の日は期待値がほぼ 0 で、負ではない）。
    """
    from dataclasses import replace

    from daytrade.plan import iv_on

    if config.regime.iv_gate <= 0 or plan.meta.iv_prev is not None:
        return plan
    value = iv_on(_archive(settings), dt.date.fromisoformat(plan.meta.prev_day))
    if value is None:
        log.warning(
            "前日の IV がアーカイブに無いためゲート無しで進行",
            code="daytrade.iv_missing",
            prev_day=plan.meta.prev_day,
        )
        return plan
    return replace(plan, meta=replace(plan.meta, iv_prev=value))


def _verdict(
    settings: AppSettings,
    config: DaytradeConfig,
    plan: Plan,
    day: dt.date,
    market_gap: float | None,
) -> Any:
    """危険信号を評価し、ログに残す。"""
    from daytrade.ledger import Ledger, realized_pnl
    from daytrade.regime import Signals, evaluate
    from wbcore.domain.models import Side

    recent: float | None = None
    if config.regime.equity_curve_days > 0:
        from daytrade.calendar import TradingCalendar

        cal = TradingCalendar.from_archive(_archive(settings))
        days: list[dt.date] = []
        cursor = day
        for _ in range(config.regime.equity_curve_days):
            cursor = cal.previous_trading_day(cursor)
            days.append(cursor)
        with Ledger(settings.daytrade_db_path) as ledger:
            history = realized_pnl(ledger, days)
            # 本発注の履歴が 1 日も無ければ信号なし（始めたばかりで縮めない）
            traded = any(not o.is_dry_run for d_ in days for o in ledger.orders_on(d_, Side.BUY))
        incomplete = [d_ for d_, v in history.items() if v is None]
        if incomplete:
            log.warning(
                "実現損益が確定していない日があり、その日を除いて資産曲線を評価",
                code="daytrade.pnl_incomplete",
                days=[d_.isoformat() for d_ in incomplete],
            )
        known = [v for v in history.values() if v is not None]
        recent = sum(known) if traded and known else None
    us_ret: float | None = None
    vix: float | None = None
    if config.regime.us_skip_high is not None:
        from daytrade.usmarket import latest_before

        session = latest_before(day)
        if session is not None:
            us_ret, vix = session.spx_ret, session.vix
    verdict = evaluate(
        config.regime,
        Signals(
            day=day,
            iv_prev=plan.meta.iv_prev,
            drift=plan.meta.drift,
            market_gap=market_gap,
            recent_pnl=recent,
            us_ret=us_ret,
            vix=vix,
        ),
    )
    log.info(
        "危険信号",
        code="daytrade.regime",
        day=day.isoformat(),
        trade=verdict.trade,
        reasons=verdict.reasons,
        **verdict.notes,
    )
    return verdict


def _print_plan(plan: Plan, config: DaytradeConfig, settings: AppSettings) -> None:
    """候補の概要。N と予算は plan 時点ではなく**今の設定**（資金を変えたら即反映）。"""
    meta = plan.meta
    n = config.capital.positions
    budget = "—（資金 0）" if n == 0 else f"{_yen(config.capital.budget_per_order)} 円"
    console.print(
        f"判定日 {meta.day}（前営業日 {meta.prev_day}）  候補 {meta.eligible} / {meta.candidates} 銘柄  "
        f"N={n}  1 注文 {budget}"
    )
    signals = []
    if meta.iv_prev is not None:
        signals.append(f"IV {meta.iv_prev:.1f}")
    if meta.drift is not None:
        signals.append(f"市場の日中ドリフト {meta.drift * 1e4:+.1f} bp/日")
    if signals:
        console.print("[dim]信号: " + "  ".join(signals) + "[/dim]")
    reasons = (
        plan.frame.filter(~pl_col("eligible")).group_by("segment").agg(pl_len()).sort("segment")
    )
    console.print(
        "[dim]除外: "
        + "、".join(f"{s} {n}" for s, n in reasons.iter_rows())
        + f"  決算(前日引け後) {int(plan.frame['earn_prev'].sum())}"
        + f"  決算(当日予定) {int(plan.frame['disc_today'].sum())}"
        + f"  日々公表 {int(plan.frame['alert'].sum())}[/dim]"
    )
    console.print(f"[dim]保存先 {settings.daytrade_dir}[/dim]")


def pl_col(name: str) -> Any:
    import polars as pl

    return pl.col(name)


def pl_len() -> Any:
    import polars as pl

    return pl.len()


@app.command("plan")
@_crash("候補の作成", "daytrade.crash")
def plan_command(ctx: typer.Context, date: _Date = None, config_dir: _ConfigDir = None) -> None:
    """翌営業日の母集団を作る（前夜に cron で回す）。9:00 の open はこれを読む。"""
    settings = _settings(ctx)
    config = _load(config_dir)
    if not config.capital.enabled:
        console.print("[dim]jp_gap_fade は無効（capital.enabled = false）。何もしません[/dim]")
        log.info("戦略が無効", code="daytrade.skip", reason="disabled")
        return
    day = _plan_day(settings, _parse_date(date), now_utc())
    _log_config(config, settings, phase="plan", day=day.isoformat())
    plan = _build_plan(settings, config, day)
    _print_plan(plan, config, settings)


# --------------------------------------------------------------------------
# open
# --------------------------------------------------------------------------


def _quotes_for(
    settings: AppSettings,
    config: DaytradeConfig,
    symbols: list[str],
    *,
    source: str | None,
    quote_file: Path | None,
) -> dict[str, Quote]:
    from daytrade.quotes import quote_source

    name = source or config.execution.quote_source
    provider = quote_source(
        name, settings.env, quote_file=quote_file or config.execution.quote_file
    )
    quotes = provider.fetch(symbols)
    missing = [s for s in symbols if s not in quotes]
    log.info(
        "気配を取得",
        code="daytrade.quotes",
        source=name,
        requested=len(symbols),
        received=len(quotes),
        missing=len(missing),
        missing_sample=missing[:SAMPLE],
    )
    return quotes


def _fresh(
    quotes: dict[str, Quote], config: DaytradeConfig, now: dt.datetime, *, allow_delayed: bool
) -> dict[str, Quote]:
    """古い気配・遅延の気配を落とす。"""
    limit = dt.timedelta(seconds=config.execution.max_quote_age)
    kept: dict[str, Quote] = {}
    stale: list[str] = []
    delayed: list[str] = []
    oldest: dt.datetime | None = None
    for symbol, quote in quotes.items():
        oldest = quote.at if oldest is None or quote.at < oldest else oldest
        if quote.delayed and not allow_delayed:
            delayed.append(symbol)
            continue
        if now - quote.at > limit and not allow_delayed:
            stale.append(symbol)
            continue
        kept[symbol] = quote
    if stale or delayed:
        log.warning(
            "使えない気配を除外",
            code="daytrade.quotes",
            stale=len(stale),
            stale_sample=stale[:SAMPLE],
            delayed=len(delayed),
            delayed_sample=delayed[:SAMPLE],
            max_age_sec=config.execution.max_quote_age,
            oldest=oldest.isoformat() if oldest else None,
        )
    return kept


def _buy_request(
    pick: Pick, day: dt.date, config: DaytradeConfig, *, attempt: int = 0
) -> OrderRequest:
    from wbcore.domain.models import OrderRequest, OrderType, Side, make_client_order_id

    # 前回が拒否されていたら種を変える（同じ ID はブローカーが弾く）。attempt 0 は従来と同じ ID
    seed = f"daytrade|{day}" if attempt == 0 else f"daytrade|{day}|{attempt}"
    return OrderRequest(
        client_order_id=make_client_order_id(seed, pick.symbol, Side.BUY, pick.quantity),
        symbol=pick.symbol,
        side=Side.BUY,
        order_type=OrderType.MARKET,
        quantity=pick.quantity,
        tax_type=config.execution.tax_account_type,
        reason=f"jp_gap_fade {day} gap {pick.gap:+.2%} #{pick.rank}",
    )


def _place_recorded(
    broker: Broker, ledger: Ledger, request: OrderRequest, day: dt.date, price: Decimal
) -> None:
    """送る前に台帳へ PENDING を書き、送ったら結果で更新する。

    送信後に落ちても台帳には残るので、次の実行で同じ注文を送り直さない
    （二重買付より買い漏れの方がまし）。
    """
    from wbcore.broker.base import OrderRejectedError
    from wbcore.domain.models import OrderStatus

    ledger.record(request, day, OrderStatus.PENDING.value, price=price)
    try:
        ack = broker.place(request)
    except OrderRejectedError:
        # 明確な拒否は台帳にも拒否と書く。PENDING のままだと「送信結果不明」と区別できず、
        # 次の実行が再送しない
        ledger.update_status(request.client_order_id, OrderStatus.REJECTED)
        raise
    ledger.update_status(request.client_order_id, ack.status, broker_order_id=ack.broker_order_id)


@app.command("open")
@_crash("寄付の買い", "daytrade.crash")
def open_command(
    ctx: typer.Context,
    live: Annotated[
        bool, typer.Option("--live", help="注文を出す。無ければ判断だけ行い、注文は出さない")
    ] = False,
    yes: Annotated[bool, typer.Option("--yes", "-y", help="本番の確認を省く（cron 用）")] = False,
    ignore_window: Annotated[
        bool, typer.Option("--ignore-window", help="時間帯の外でも判断する")
    ] = False,
    allow_delayed: Annotated[
        bool, typer.Option("--allow-delayed", help="遅延した気配でも使う（検証用）")
    ] = False,
    quote_source: Annotated[
        str | None, typer.Option("--quote-source", help="気配の取得元を上書き")
    ] = None,
    quote_file: Annotated[
        Path | None, typer.Option("--quote-file", help="csv のときのファイル")
    ] = None,
    date: _Date = None,
    config_dir: _ConfigDir = None,
) -> None:
    """9:00: 気配でギャップ下位 N 銘柄を選び、成行で買う。既定は dry-run。"""
    from daytrade import plan as planning
    from daytrade.ledger import DRY_RUN_STATUS, Ledger
    from daytrade.select import pick as pick_symbols
    from daytrade.select import rank as rank_symbols
    from wbcore.broker.base import BrokerError
    from wbcore.domain.models import Side
    from wbcore.notify import alert

    settings = _settings(ctx)
    config = _load(config_dir)
    if not config.capital.enabled:
        console.print("[dim]jp_gap_fade は無効（capital.enabled = false）。何もしません[/dim]")
        log.info("戦略が無効", code="daytrade.skip", reason="disabled")
        return
    console.print(describe_mode(settings.env, live, kill_switch=config.execution.kill_switch))
    now = now_utc()
    day = _parse_date(date) or _today_jst(now)
    watch_only = config.capital.positions == 0
    _log_config(
        config,
        settings,
        phase="open",
        day=day.isoformat(),
        live=live,
        allow_delayed=allow_delayed,
        quote_override=quote_source,
        watch_only=watch_only,
    )
    if watch_only:
        console.print(
            "[yellow]資金 0（max_capital = 0）: スクリーニングと候補の表示だけ行い、買いません[/yellow]"
        )
    if _skip_holiday(settings, day, "open"):
        return
    if live and not ignore_window and not _in_window(config, "entry", now):
        console.print(
            f"[dim]発注時間帯の外（{_describe_window(config, 'entry')}）。何もしません[/dim]"
        )
        log.info(
            "発注時間帯の外",
            code="daytrade.skip",
            reason="window",
            window=_describe_window(config, "entry"),
        )
        return
    plan = planning.load(settings.daytrade_dir, day)
    if plan is None:
        console.print(
            f"[yellow]{day} の候補が無いので今作ります（前夜の plan が走っていません）[/yellow]"
        )
        plan = _build_plan(settings, config, day)
    plan = _refresh_iv(settings, config, plan)
    _print_plan(plan, config, settings)

    allowed, reason = allows_live_orders(
        settings.env, live, kill_switch=config.execution.kill_switch
    )
    ledger = Ledger(settings.daytrade_db_path)
    try:
        if not allowed:
            # dry-run は確認のたびに増える。その日の古い dry-run は消して最新だけ残す
            ledger.clear_dry_run(day)
        # 生きている／約定した買いがあれば今日は終わり。拒否・失効だけなら送り直す
        already = [o for o in ledger.orders_on(day, Side.BUY) if not o.is_dry_run and not o.is_dead]
        if already:
            console.print(
                f"[dim]今日の買いは発注済み（{len(already)} 件、冪等）。何もしません[/dim]"
            )
            log.info("発注済み", code="daytrade.skip", reason="already", orders=len(already))
            return
        symbols = plan.eligible["symbol"].to_list()
        quotes = _fresh(
            _quotes_for(settings, config, symbols, source=quote_source, quote_file=quote_file),
            config,
            now,
            allow_delayed=allow_delayed,
        )
        if not quotes:
            console.print("[red]使える気配がありません。発注しません[/red]")
            log.error("気配が無いため見送り", code="daytrade.skip", reason="no_quotes")
            alert("デイトレ: 気配が取れず寄付の買いを見送り", f"{day} 候補 {len(symbols)} 銘柄")
            return
        from daytrade.regime import market_gap_of

        prev = dict(plan.eligible.select("symbol", "prev_close").iter_rows())
        gaps = [float(q.price) / prev[sym] - 1 for sym, q in quotes.items() if prev.get(sym)]
        verdict = _verdict(settings, config, plan, day, market_gap_of(gaps))
        if not verdict.trade:
            console.print(
                "[yellow]危険信号により今日は取引しません: "
                + "、".join(verdict.reasons)
                + "[/yellow]"
            )
            log.info(
                "危険信号で見送り", code="daytrade.skip", reason="regime", reasons=verdict.reasons
            )
            return
        # 様子見モードでは「買うとしたら」の上位を目安の予算で見せる。
        # 次点（N の先 RANKING_EXTRA 件）も順位表として残す——「なぜ X が選ばれなかったか」を後から追うため
        n = WATCH_ROWS if watch_only else config.capital.positions
        budget = config.capital.order_budget if watch_only else config.capital.budget_per_order
        if verdict.scale < 1:
            budget = (budget * Decimal(str(verdict.scale))).quantize(Decimal(1))
            console.print(f"[yellow]{verdict.scale_reason}（1 注文 {_yen(budget)} 円）[/yellow]")
        ranking = rank_symbols(plan.eligible, quotes, config.signal)
        picks = pick_symbols(
            plan.eligible,
            quotes,
            n=n,
            budget=budget,
            config=config.signal,
            weighting="equal" if watch_only else config.capital.weighting,
            ranked=ranking,
        )
        picked = {p.symbol: p for p in picks}
        _print_picks(picks, quotes, plan, watch_only=watch_only)
        log.info(
            "順位表（N と次点）",
            code="daytrade.ranking",
            day=day.isoformat(),
            n=n,
            budget=str(budget),
            scale=verdict.scale,
            weighting=config.capital.weighting,
            quotes=len(quotes),
            rows=[
                {
                    "rank": r.rank,
                    "symbol": r.symbol,
                    "gap": str(r.gap),
                    "price": str(r.price),
                    "vol": round(r.vol, 4) if r.vol is not None else None,
                    "quantity": str(picked[r.symbol].quantity) if r.symbol in picked else None,
                    "picked": r.symbol in picked,
                }
                for r in ranking[: n + RANKING_EXTRA]
            ],
        )
        for p in picks:
            log.info(
                "銘柄を選定",
                code="daytrade.pick",
                day=day.isoformat(),
                symbol=p.symbol,
                code_=p.code,
                rank=p.rank,
                gap=str(p.gap),
                prev_close=str(p.prev_close),
                price=str(p.price),
                quantity=str(p.quantity),
                amount=str(p.amount),
            )
        if not picks:
            log.info(
                "条件に合う銘柄なし", code="daytrade.skip", reason="no_picks", quotes=len(quotes)
            )
            return
        if watch_only:
            log.info(
                "資金 0 のため買わない", code="daytrade.skip", reason="no_capital", picks=len(picks)
            )
            return
        _confirm_live(settings, allowed, yes)
        failures: list[str] = []
        broker = _connect(settings, config) if allowed else None
        remaining: Decimal | None = None
        for p in picks:
            request = _buy_request(
                p, day, config, attempt=ledger.dead_count(day, p.symbol, Side.BUY)
            )
            if ledger.was_placed(request.client_order_id):
                console.print(f"  {p.symbol}: [dim]発注済み（冪等）[/dim]")
                continue
            if broker is None:
                ledger.record(request, day, DRY_RUN_STATUS, price=p.price)
                outcome = "dry-run"
            else:
                try:
                    if remaining is None:
                        remaining = broker.get_balance().buying_power
                    need = p.amount + p.fee
                    if need > remaining:
                        outcome = (
                            f"見送り 買付余力不足（必要 {_yen(need)} / 余力 {_yen(remaining)}）"
                        )
                        failures.append(f"{p.symbol}: {outcome}")
                        console.print(f"  {p.symbol}: [red]{outcome}[/red]")
                        continue
                    remaining -= need
                    _place_recorded(broker, ledger, request, day, p.price)
                    outcome = "発注"
                except BrokerError as exc:
                    outcome = f"失敗 {exc}"
                    failures.append(f"{p.symbol}: {exc}")
                    console.print(f"  {p.symbol}: [red]{outcome}[/red]")
            log.info(
                "寄付の買い注文",
                code="daytrade.order",
                day=day.isoformat(),
                symbol=p.symbol,
                side="BUY",
                client_order_id=request.client_order_id,
                quantity=str(p.quantity),
                price=str(p.price),
                amount=str(p.amount),
                live=broker is not None,
                outcome=_plain(outcome),
            )
        if failures:
            alert(f"デイトレ: {len(failures)} 件の買いが通らず", "\n".join(failures))
        log.info(
            "寄付の買いを終了",
            code="daytrade.run",
            phase="open",
            live=allowed,
            reason=reason,
            n=n,
            budget=str(budget),
            scale=verdict.scale,
            picks=len(picks),
            failures=len(failures),
        )
    finally:
        ledger.close()


#: 様子見モード（資金 0）で見せる候補の数。
WATCH_ROWS = 5
#: 順位表に残す次点の数（N の先）。
RANKING_EXTRA = 5


def _print_picks(
    picks: list[Pick], quotes: dict[str, Quote], plan: Plan, *, watch_only: bool = False
) -> None:
    label = "候補（買わない: 資金 0）" if watch_only else "寄付の買い"
    table = Table(title=f"{plan.meta.day} {label}（気配 {len(quotes)} 銘柄から）")
    for column in ("#", "銘柄", "名称", "前日終値", "気配", "ギャップ", "株数", "金額", "手数料"):
        table.add_column(column, justify="right" if column not in ("銘柄", "名称") else "left")
    for p in picks:
        table.add_row(
            str(p.rank),
            p.symbol,
            p.name[:12],
            _yen(p.prev_close),
            _yen(p.price),
            f"{p.gap:+.2%}",
            f"{p.quantity:,.0f}",
            _yen(p.amount),
            _yen(p.fee),
        )
    console.print(table)
    if not picks:
        console.print(
            "[dim]条件に合う銘柄がありません（ギャップダウンが無いか、1 単元が予算に届かない）[/dim]"
        )


# --------------------------------------------------------------------------
# close
# --------------------------------------------------------------------------


@app.command("close")
@_crash("引けの売り", "daytrade.crash")
def close_command(
    ctx: typer.Context,
    live: Annotated[
        bool, typer.Option("--live", help="注文を出す。無ければ対象を示すだけ")
    ] = False,
    yes: Annotated[bool, typer.Option("--yes", "-y", help="本番の確認を省く（cron 用）")] = False,
    ignore_window: Annotated[
        bool, typer.Option("--ignore-window", help="時間帯の外でも売る")
    ] = False,
    date: _Date = None,
    config_dir: _ConfigDir = None,
) -> None:
    """15:20〜: 今日買った分を成行で売る（15:25 以降ならクロージング・オークションで引け値）。既定は dry-run。"""
    from daytrade.ledger import DRY_RUN_STATUS, Ledger
    from wbcore.broker.base import BrokerError
    from wbcore.domain.models import (
        OrderRequest,
        OrderStatus,
        OrderType,
        Side,
        make_client_order_id,
    )
    from wbcore.notify import alert

    settings = _settings(ctx)
    config = _load(config_dir)
    console.print(describe_mode(settings.env, live, kill_switch=config.execution.kill_switch))
    now = now_utc()
    day = _parse_date(date) or _today_jst(now)
    if _skip_holiday(settings, day, "close"):
        return
    if live and not ignore_window and not _in_window(config, "exit", now):
        console.print(
            f"[dim]手仕舞いの時間帯の外（{_describe_window(config, 'exit')}）。何もしません[/dim]"
        )
        log.info(
            "手仕舞いの時間帯の外",
            code="daytrade.skip",
            reason="window",
            window=_describe_window(config, "exit"),
        )
        return
    allowed, reason = allows_live_orders(
        settings.env, live, kill_switch=config.execution.kill_switch
    )
    _log_config(config, settings, phase="close", day=day.isoformat(), live=live)
    ledger = Ledger(settings.daytrade_db_path)
    try:
        buys = [o for o in ledger.orders_on(day, Side.BUY) if not o.is_dry_run]
        # 生きている／約定した売りだけが「発注済み」。拒否・失効した売りは数えない（再送する）
        sells = {
            o.symbol: o
            for o in ledger.orders_on(day, Side.SELL)
            if not o.is_dry_run and not o.is_dead
        }
        if not buys:
            dry = [o for o in ledger.orders_on(day, Side.BUY) if o.is_dry_run]
            console.print(
                f"[dim]今日の買いが台帳にありません（dry-run {len(dry)} 件）。何もしません[/dim]"
            )
            log.info("売る対象なし", code="daytrade.skip", reason="no_buys", dry_run=len(dry))
            if allowed:
                _warn_unrecorded_positions(settings, config, day)
            return
        broker = _connect(settings, config) if allowed else None
        # 約定数量はブローカーに聞く（部分約定・拒否をそのまま売り数量に反映する）
        targets: list[tuple[str, Decimal, Decimal | None]] = []
        for order in buys:
            filled = order.filled_quantity
            fill_price = order.avg_fill_price
            if broker is not None:
                try:
                    current = broker.get_order(order.client_order_id)
                except BrokerError as exc:
                    console.print(
                        f"  {order.symbol}: [yellow]照会に失敗、台帳の値で続行: {exc}[/yellow]"
                    )
                    current = None
                if current is not None:
                    filled, fill_price = current.filled_quantity, current.avg_fill_price
                    ledger.update_status(
                        order.client_order_id,
                        current.status,
                        filled_quantity=filled,
                        avg_fill_price=fill_price,
                        broker_order_id=current.broker_order_id,
                    )
                    log.info(
                        "買い注文の約定状況",
                        code="daytrade.fill",
                        day=day.isoformat(),
                        symbol=order.symbol,
                        client_order_id=order.client_order_id,
                        broker_order_id=current.broker_order_id,
                        before=order.status,
                        after=current.status.value,
                        quantity=str(order.quantity),
                        filled=str(filled),
                        avg_fill_price=str(fill_price) if fill_price is not None else None,
                    )
                else:
                    log.warning(
                        "買い注文を照会できず台帳の値で続行",
                        code="daytrade.fill",
                        day=day.isoformat(),
                        symbol=order.symbol,
                        client_order_id=order.client_order_id,
                        before=order.status,
                        after=None,
                        filled=str(filled),
                    )
            elif (
                order.status in {OrderStatus.SUBMITTED.value, OrderStatus.PENDING.value}
                and filled == 0
            ):
                filled = order.quantity  # dry-run では全約定とみなして対象を示す
            if filled <= 0:
                console.print(
                    f"  {order.symbol}: [dim]約定なし（{order.status}）。売る数量がありません[/dim]"
                )
                continue
            if order.symbol in sells:
                console.print(f"  {order.symbol}: [dim]売り発注済み（冪等）[/dim]")
                continue
            targets.append((order.symbol, filled, fill_price))
        if not targets:
            log.info("売る対象なし", code="daytrade.skip", reason="nothing_to_sell")
            return
        _confirm_live(settings, allowed, yes)
        failures: list[str] = []
        for symbol, quantity, fill_price in targets:
            # 前回の売りが拒否されていたら種を変えて送り直す（同じ ID はブローカーが弾く）
            attempt = ledger.dead_count(day, symbol, Side.SELL)
            request = OrderRequest(
                client_order_id=make_client_order_id(
                    f"daytrade-close|{day}|{attempt}", symbol, Side.SELL, quantity
                ),
                symbol=symbol,
                side=Side.SELL,
                order_type=OrderType.MARKET,
                quantity=quantity,
                tax_type=config.execution.tax_account_type,
                reason=f"jp_gap_fade {day} 引けで手仕舞い",
            )
            if ledger.was_placed(request.client_order_id):
                console.print(f"  {symbol}: [dim]売り発注済み（冪等）[/dim]")
                continue
            if broker is None:
                ledger.record(request, day, DRY_RUN_STATUS, price=fill_price)
                outcome = "dry-run"
                console.print(f"  {symbol}: {quantity:,.0f} 株 [yellow]dry-run[/yellow]")
            else:
                try:
                    _place_recorded(broker, ledger, request, day, fill_price or Decimal(0))
                    outcome = "発注"
                    console.print(f"  {symbol}: {quantity:,.0f} 株 [green]発注[/green]")
                except BrokerError as exc:
                    outcome = f"失敗 {exc}"
                    failures.append(f"{symbol}: {exc}")
                    console.print(f"  {symbol}: [red]{outcome}[/red]")
            log.info(
                "引けの売り注文",
                code="daytrade.order",
                day=day.isoformat(),
                symbol=symbol,
                side="SELL",
                client_order_id=request.client_order_id,
                quantity=str(quantity),
                price=str(fill_price) if fill_price is not None else None,
                live=broker is not None,
                outcome=_plain(outcome),
            )
        if failures:
            # 売れ残りは持ち越しになる。人が手で売る必要があるので必ず知らせる
            alert(
                f"デイトレ: {len(failures)} 件の手仕舞いが通らず（持ち越しの恐れ）",
                "\n".join(failures),
            )
        log.info(
            "引けの売りを終了",
            code="daytrade.run",
            phase="close",
            live=allowed,
            reason=reason,
            sells=len(targets),
            failures=len(failures),
        )
    finally:
        ledger.close()


def _warn_unrecorded_positions(settings: AppSettings, config: DaytradeConfig, day: dt.date) -> None:
    """台帳に今日の買いが無いのに、今日の候補だった銘柄をブローカーが保有していれば知らせる。

    台帳を失う・open が送信後に落ちて記録できない、といったときの保険。自動では売らない
    （他の戦略の保有かもしれない）。人が確かめて手で売る。
    """
    from daytrade import plan as planning
    from wbcore.broker.base import BrokerError
    from wbcore.notify import alert

    plan = planning.load(settings.daytrade_dir, day)
    if plan is None:
        return
    symbols = set(plan.eligible["symbol"].to_list())
    try:
        positions = _connect(settings, config).positions_by_symbol()
    except BrokerError as exc:
        log.warning("建玉を照会できず保険の確認を省略", code="daytrade.reconcile", error=str(exc))
        return
    held = {s: p.quantity for s, p in positions.items() if s in symbols and p.quantity > 0}
    if not held:
        return
    detail = "、".join(f"{s} {q:,.0f} 株" for s, q in sorted(held.items()))
    console.print(
        f"[red]台帳に無い建玉があります（今日の候補の銘柄）: {detail}。手で確かめてください[/red]"
    )
    log.error(
        "台帳に無い建玉",
        code="daytrade.reconcile",
        day=day.isoformat(),
        held={s: str(q) for s, q in held.items()},
    )
    alert("デイトレ: 台帳に無い建玉（持ち越しの恐れ）", detail)


@app.command("verify")
@_crash("手仕舞いの検証", "daytrade.crash")
def verify_command(ctx: typer.Context, date: _Date = None, config_dir: _ConfigDir = None) -> None:
    """引け後: 今日の売りが全部約定したかをブローカーに照会し、未約定（持ち越し）なら通知する。"""
    from daytrade.ledger import Ledger
    from wbcore.broker.base import BrokerError
    from wbcore.domain.models import Side
    from wbcore.notify import alert

    settings = _settings(ctx)
    config = _load(config_dir)
    day = _parse_date(date) or _today_jst()
    _log_config(config, settings, phase="verify", day=day.isoformat())
    ledger = Ledger(settings.daytrade_db_path)
    try:
        buys = [o for o in ledger.orders_on(day, Side.BUY) if not o.is_dry_run]
        sells = [o for o in ledger.orders_on(day, Side.SELL) if not o.is_dry_run]
        if not buys:
            console.print("[dim]今日の本発注はありません[/dim]")
            log.info("検証の対象なし", code="daytrade.skip", reason="no_buys", phase="verify")
            return
        broker = _connect(settings, config)
        carried: list[str] = []
        bought: dict[str, Decimal] = {}
        for order in buys:
            current = broker.get_order(order.client_order_id)
            if current is not None:
                ledger.update_status(
                    order.client_order_id,
                    current.status,
                    filled_quantity=current.filled_quantity,
                    avg_fill_price=current.avg_fill_price,
                    broker_order_id=current.broker_order_id,
                )
                bought[order.symbol] = (
                    bought.get(order.symbol, Decimal(0)) + current.filled_quantity
                )
            else:
                bought[order.symbol] = bought.get(order.symbol, Decimal(0)) + order.filled_quantity
        sold: dict[str, Decimal] = {}
        for order in sells:
            current = broker.get_order(order.client_order_id)
            filled = order.filled_quantity
            if current is not None:
                filled = current.filled_quantity
                ledger.update_status(
                    order.client_order_id,
                    current.status,
                    filled_quantity=filled,
                    avg_fill_price=current.avg_fill_price,
                    broker_order_id=current.broker_order_id,
                )
                log.info(
                    "売り注文の約定状況",
                    code="daytrade.fill",
                    day=day.isoformat(),
                    symbol=order.symbol,
                    client_order_id=order.client_order_id,
                    broker_order_id=current.broker_order_id,
                    before=order.status,
                    after=current.status.value,
                    quantity=str(order.quantity),
                    filled=str(filled),
                    avg_fill_price=str(current.avg_fill_price)
                    if current.avg_fill_price is not None
                    else None,
                )
            sold[order.symbol] = sold.get(order.symbol, Decimal(0)) + filled
        for symbol, quantity in sorted(bought.items()):
            remaining = quantity - sold.get(symbol, Decimal(0))
            if remaining > 0:
                carried.append(f"{symbol} {remaining:,.0f} 株")
                console.print(
                    f"  {symbol}: [red]{remaining:,.0f} 株が売れていません（持ち越し）[/red]"
                )
            elif quantity > 0:
                console.print(f"  {symbol}: {quantity:,.0f} 株 手仕舞い済み")
        # 台帳と食い違う建玉（送信結果不明の買いが実は通っていた等）はブローカー側で確かめる
        try:
            positions = broker.positions_by_symbol()
        except BrokerError as exc:
            log.warning("建玉を照会できず突合を省略", code="daytrade.reconcile", error=str(exc))
            positions = {}
        flagged = {c.split(" ")[0] for c in carried}
        for symbol in sorted(bought):
            held = positions.get(symbol)
            if held is not None and held.quantity > 0 and symbol not in flagged:
                carried.append(f"{symbol} {held.quantity:,.0f} 株（台帳と不一致）")
                console.print(
                    f"  {symbol}: [red]ブローカーに {held.quantity:,.0f} 株の建玉"
                    "（台帳では手仕舞い済み）[/red]"
                )
                log.error(
                    "台帳と建玉が不一致",
                    code="daytrade.reconcile",
                    day=day.isoformat(),
                    symbol=symbol,
                    held=str(held.quantity),
                )
        if carried:
            log.error("持ち越し", code="daytrade.carry", day=day.isoformat(), positions=carried)
            alert(
                "デイトレ: 売れ残りがあります（持ち越し）。翌朝に手で売ってください",
                "\n".join(carried),
            )
        else:
            log.info("手仕舞いを確認", code="daytrade.run", phase="verify", live=True, carried=0)
    except BrokerError as exc:
        console.print(f"[red]照会に失敗: {exc}[/red]")
        raise typer.Exit(1) from None
    finally:
        ledger.close()


# --------------------------------------------------------------------------
# status / quotes / backtest
# --------------------------------------------------------------------------


@app.command("status")
def status_command(ctx: typer.Context, date: _Date = None, config_dir: _ConfigDir = None) -> None:
    """今日の候補と台帳の注文を表示する。"""
    from daytrade import plan as planning
    from daytrade.ledger import Ledger

    settings = _settings(ctx)
    config = _load(config_dir)
    day = _parse_date(date) or _today_jst()
    state = "有効" if config.capital.enabled else "[red]無効（capital.enabled = false）[/red]"
    console.print(
        f"jp_gap_fade: {state}  資金 {_yen(config.capital.max_capital)} 円 → N={config.capital.positions}、"
        f"1 注文 {_yen(config.capital.budget_per_order)} 円"
        + ("（資金 0: 買わない）" if config.capital.positions == 0 else "")
    )
    plan = planning.load(settings.daytrade_dir, day)
    if plan is None:
        console.print(f"[yellow]{day} の候補がありません（daytrade plan を実行）[/yellow]")
    else:
        _print_plan(plan, config, settings)
    with Ledger(settings.daytrade_db_path) as ledger:
        orders = ledger.orders_on(day)
    if not orders:
        console.print("[dim]今日の注文はありません[/dim]")
        return
    table = Table(title=f"{day} の注文")
    for column in ("時刻", "銘柄", "売買", "株数", "約定", "価格", "約定単価", "状態"):
        table.add_column(column)
    for o in orders:
        table.add_row(
            fmt(dt.datetime.fromisoformat(o.placed_at), settings.timezone),
            o.symbol,
            o.side.value,
            f"{o.quantity:,.0f}",
            f"{o.filled_quantity:,.0f}",
            _yen(o.price) if o.price is not None else "",
            _yen(o.avg_fill_price) if o.avg_fill_price is not None else "",
            o.status,
        )
    console.print(table)


@app.command("quotes")
def quotes_command(
    ctx: typer.Context,
    symbols: Annotated[list[str], typer.Argument(help="銘柄（7203 9984 …）")],
    source: Annotated[str | None, typer.Option("--source", help="webull / yfinance / csv")] = None,
    quote_file: Annotated[Path | None, typer.Option("--quote-file")] = None,
    config_dir: _ConfigDir = None,
) -> None:
    """気配の取得元の疎通を確かめる。Webull が日本株を返すかはこれで見る。"""
    from daytrade.quotes import QuoteError

    settings = _settings(ctx)
    config = _load(config_dir)
    try:
        quotes = _quotes_for(settings, config, symbols, source=source, quote_file=quote_file)
    except QuoteError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(1) from None
    now = now_utc()
    for symbol in symbols:
        q = quotes.get(symbol)
        if q is None:
            console.print(f"  {symbol}: [red]取れませんでした[/red]")
            continue
        age = int((now - q.at).total_seconds())
        flag = "（遅延）" if q.delayed else ""
        console.print(
            f"  {symbol}: {_yen(q.price)} 円  {fmt(q.at, settings.timezone, seconds=True)}  {age} 秒前 {q.source}{flag}"
        )


@app.command("backtest")
def backtest_command(
    ctx: typer.Context,
    since: Annotated[str, typer.Option("--since", help="開始日")] = "2017-01-01",
    until: Annotated[str | None, typer.Option("--until", help="終了日（既定は最新）")] = None,
    trades: Annotated[bool, typer.Option("--trades", help="個別の取引も出す")] = False,
    config_dir: _ConfigDir = None,
) -> None:
    """アーカイブで同じ規則を検証する（資金固定・100 株単位・段階手数料）。"""
    from daytrade import backtest as bt

    settings = _settings(ctx)
    config = _load(config_dir)
    start = _parse_date(since) or dt.date(2017, 1, 1)
    end = _parse_date(until) or _today_jst()
    if config.margin.enabled:
        _backtest_margin(settings, config, start, end, trades=trades)
        return
    try:
        result = bt.run(_archive(settings), config, start, end)
    except ValueError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(1) from None
    s = result.summary
    console.print(
        f"{start}〜{end}  資金 {_yen(s.capital)} 円  N={config.capital.positions}  "
        f"営業日 {s.days}（取引 {s.traded_days}）  往復手数料 {s.round_trip_bp:.1f} bp"
    )
    console.print(
        f"損益合計 {_yen(s.total_pnl)} 円  日平均 {_yen(s.mean_daily)} 円  年率 {s.annual_return:.1%}  "
        f"Sharpe {s.sharpe:.2f}  最大 DD {_yen(s.max_drawdown)} 円  勝率(日) {s.win_rate:.1%}"
    )
    console.print(
        f"月次: 平均 {_yen(s.monthly_mean)} 円  中央値 {_yen(s.monthly_median)} 円  "
        f"10% 点 {_yen(s.monthly_p10)} 円  勝ち月 {s.monthly_win:.0%}  平均銘柄数 {s.avg_positions:.1f}"
    )
    table = Table(title="年別")
    for column in ("年", "営業日", "取引日", "損益", "日平均", "勝率(取引日)"):
        table.add_column(column, justify="right")
    for year, days, traded, pnl, mean, win in result.yearly().iter_rows():
        table.add_row(str(year), str(days), str(traded), _yen(pnl), _yen(mean), f"{(win or 0):.1%}")
    console.print(table)
    if trades:
        console.print(
            result.trades.select("Date", "Code", "gap", "shares", "O", "C", "pnl").tail(30)
        )


def _backtest_margin(
    settings: AppSettings, config: DaytradeConfig, start: dt.date, end: dt.date, *, trades: bool
) -> None:
    """ロング + 信用売り（``[margin]``）の検証。ロング・ショートの内訳も出す。"""
    from daytrade import backtest as bt

    try:
        result = bt.run_margin(_archive(settings), config, start, end)
    except ValueError as exc:
        console.print(f"[red]{exc}[/red]")
        raise typer.Exit(1) from None
    cash = config.capital.max_capital
    console.print(
        f"{start}〜{end}  現金 {_yen(cash)} 円（保証金）  ロング N={config.capital.positions} "
        f"{_yen(config.capital.max_capital)} 円  ショート N={config.margin.positions} "
        f"{_yen(config.margin.max_capital)} 円 × 通常 {config.margin.multiplier_normal:g} / "
        f"弱 {config.margin.multiplier_long_weak:g}  営業日 {result.summary.days}"
    )
    for label, s in (
        ("合算", result.summary),
        ("ロング", result.long_summary),
        ("ショート", result.short_summary),
    ):
        console.print(
            f"{label:<5} 損益 {_yen(s.total_pnl):>12} 円  年率(対現金) "
            f"{float(s.total_pnl) / (s.days / bt.TRADING_DAYS) / float(cash):6.1%}  "
            f"Sharpe {s.sharpe:5.2f}  最大 DD {_yen(s.max_drawdown):>10} 円  "
            f"取引日 {s.traded_days}  往復コスト {s.round_trip_bp:.1f} bp"
        )
    table = Table(title="年別（ロング / ショート）")
    for column in ("年", "営業日", "取引日", "合算", "ロング", "ショート", "勝率(取引日)"):
        table.add_column(column, justify="right")
    for row in result.yearly().iter_rows(named=True):
        table.add_row(
            str(row["year"]),
            str(row["days"]),
            str(row["traded"]),
            _yen(row["pnl"]),
            _yen(row["long_pnl"]),
            _yen(row["short_pnl"]),
            f"{(row['win'] or 0):.1%}",
        )
    console.print(table)
    if trades:
        console.print(
            result.short_trades.select("Date", "Code", "gap", "shares", "O", "C", "pnl").tail(30)
        )
