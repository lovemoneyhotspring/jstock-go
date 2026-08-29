"""日次の実行サイクル。

バックテストと**同じ部品**を使い、ブローカーだけが実物に替わる。

1サイクルの流れ:
    1. 未約定注文と建玉を突き合わせて現状を把握する
    2. 足を更新する（保存済みの続きだけ取得）
    3. 戦略 → 合成 → サイジング → ストップ優先 → 差分 → リスク判定
    4. 発注（dry-run なら記録だけして発注しない）
    5. すべてを journal に残す

安全装置:
    - ``--live`` と ``WBJP_ENV=prod`` の両方が揃わない限り発注しない
    - キルスイッチが有効なら即座に何もせず終了する
    - 同じ ``client_order_id`` が journal にあれば発注しない（冪等）。
      注文IDの種は **基準日** なので、この冪等性は実行をまたいで効く。
      cron が二重に走っても、失敗後に手で再実行しても二重発注にならない。
"""

from __future__ import annotations

import datetime as dt
import uuid
from collections.abc import Iterable
from dataclasses import dataclass, field, replace
from decimal import Decimal

import polars as pl

from wbcore.broker.base import Broker, BrokerError
from wbcore.clock import today_utc
from wbcore.data.provider import MarketDataProvider
from wbcore.data.store import BarStore
from wbcore.domain.market_rules import rules_for
from wbcore.domain.models import (
    Balance,
    CombinedSignal,
    Order,
    OrderRequest,
    Position,
    Side,
    Signal,
    TargetPosition,
)
from wbcore.indicators.ohlcv import atr, sma
from wbcore.logging import get_logger
from wbjp.config import Config
from wbjp.db.repo import Journal
from wbjp.engine.reconcile import ReconcileSettings, reconcile
from wbjp.portfolio.sizer import SizingContext, build_sizer
from wbjp.risk.limits import RiskContext, RiskManager
from wbjp.risk.stops import StopBook, apply_stop_priority, sync_broker_stops
from wbjp.strategy.base import Strategy, StrategyContext
from wbjp.strategy.combiner import build_combiner

log = get_logger(__name__)

ATR_PERIOD = 14


def _pending_buy_value(
    open_orders: Iterable[Order], prices: dict[str, Decimal]
) -> dict[str, Decimal]:
    """銘柄ごとの「板に残っている買い注文」の想定約定代金。

    建玉比率の判定を「約定分＋発注中」で行うために要る。未約定を無視すると、
    上限ぎりぎりの注文を出した直後に同額をもう一度出せてしまう。

    指値があればそれを、成行なら直近終値を使う。値段が分からない注文は
    黙って0円扱いにせず、除外したことが分かるよう呼び出し側には現れない。
    """
    pending: dict[str, Decimal] = {}
    for order in open_orders:
        if order.side is not Side.BUY or not order.status.is_open:
            continue
        price = order.limit_price or prices.get(order.symbol)
        if price is None:
            continue
        pending[order.symbol] = (
            pending.get(order.symbol, Decimal(0)) + order.remaining_quantity * price
        )
    return pending


#: ウォームアップに加えて余分に読む日数。祝日と欠測を吸収する。
HISTORY_PADDING_DAYS = 200


@dataclass
class CycleResult:
    """1サイクルの結果。"""

    run_id: str
    as_of: dt.date
    live: bool
    reason: str
    signals: list[Signal] = field(default_factory=list)
    combined: dict[str, CombinedSignal] = field(default_factory=dict)
    targets: list[TargetPosition] = field(default_factory=list)
    planned: list[OrderRequest] = field(default_factory=list)
    placed: list[OrderRequest] = field(default_factory=list)
    rejected: dict[str, str] = field(default_factory=dict)

    @property
    def dry_run(self) -> bool:
        return not self.live


class LiveRunner:
    """日次サイクルの実行。"""

    def __init__(
        self,
        config: Config,
        strategies: list[Strategy],
        broker: Broker,
        store: BarStore,
        journal: Journal,
        provider: MarketDataProvider | None = None,
    ) -> None:
        self.config = config
        self.strategies = strategies
        self.broker = broker
        self.store = store
        self.journal = journal
        self.provider = provider

        file_config = config.file
        if file_config.universe.bar_interval.is_intraday:
            # 日中足のライブ運用には「新しい足が確定したときだけ判断する」
            # エポック管理と、実行の重なりを防ぐロックが要る。それが無い状態で
            # 動かすと、5分ごとに同じ足で判断し直す。黙って日足で動くよりは
            # ここで止める。
            raise NotImplementedError(
                f"ライブ運用は日足のみ対応です（設定は {file_config.universe.interval} 足）。"
                "日中足の運用には判断エポックとロックの実装が要ります。"
                "バックテストは `wbjp backtest` で日中足に対応しています"
            )
        for strategy in strategies:
            if not strategy.supports(file_config.universe.bar_interval):
                raise ValueError(
                    f"戦略 {strategy.name} は {file_config.universe.interval} 足に対応していません"
                )
            strategy.bind(file_config.universe.bar_interval)
        self.combiner = build_combiner(
            file_config.strategies.combiner,
            {s.name: s.weight for s in file_config.strategies.enabled},
        )
        self.sizer = build_sizer(file_config.sizing)
        self.rules = rules_for(
            file_config.universe.market, topix500=set(file_config.universe.topix500_symbols)
        )
        self.risk = RiskManager(file_config.risk, file_config.universe.symbols, self.rules)

    def run_once(
        self,
        *,
        live: bool = False,
        as_of: dt.date | None = None,
        run_id: str | None = None,
    ) -> CycleResult:
        """1サイクル実行する。

        Args:
            live: True かつ環境の条件を満たすときだけ実際に発注する。
            as_of: 判断の基準日。既定は保存済みの最新取引日。
        """
        allowed, reason = self.config.allows_live_orders(live)
        run_id = run_id or f"{today_utc():%Y%m%d}-{uuid.uuid4().hex[:8]}"

        symbols = self.config.file.universe.symbols
        if not symbols:
            raise ValueError("universe.symbols が空です。売買対象を設定してください")

        # キルスイッチは他の何よりも先に効かせる
        if self.config.file.risk.kill_switch:
            log.error("キルスイッチが有効なため、サイクルを中止します")
            return CycleResult(run_id, as_of or today_utc(), False, reason)

        self._refresh_bars(symbols)
        bars = self._load_bars(symbols)
        if not bars:
            raise ValueError("足データがありません。先に `wbjp data sync` を実行してください")

        if as_of is None:
            latest = max(frame["date"].max() for frame in bars.values())  # type: ignore[type-var]
            if not isinstance(latest, dt.date):
                raise TypeError(f"足の日付が date ではありません: {type(latest)}")
            as_of = latest
        balance = self.broker.get_balance()
        if balance.currency != self.rules.currency:
            raise ValueError(
                f"ブローカーの残高通貨 {balance.currency} が市場 {self.rules.market.value} "
                f"の通貨 {self.rules.currency} と一致しません。設定とブローカーの市場を揃えてください"
            )
        positions = self.broker.positions_by_symbol()
        equity = balance.cash_balance + sum(
            (p.market_value for p in positions.values()), start=Decimal(0)
        )

        self.journal.start_run(
            run_id,
            as_of,
            self.config.env.value,
            "live" if allowed else "dry_run",
            equity,
            balance.cash_balance,
        )
        log.info(
            "サイクル開始",
            run_id=run_id,
            as_of=str(as_of),
            env=self.config.env.value,
            live=allowed,
            reason=reason,
            equity=str(equity),
            positions=len(positions),
        )

        try:
            result = self._execute(run_id, as_of, bars, positions, balance, equity, allowed, reason)
        except Exception as exc:
            self.journal.finish_run(run_id, "error", str(exc))
            log.error("サイクルが失敗しました", run_id=run_id, error=str(exc))
            raise

        self.journal.finish_run(run_id, "ok")
        return result

    # -- 内部 ---------------------------------------------------------------

    def _execute(
        self,
        run_id: str,
        as_of: dt.date,
        bars: dict[str, pl.DataFrame],
        positions: dict[str, Position],
        balance: Balance,
        equity: Decimal,
        allowed: bool,
        reason: str,
    ) -> CycleResult:
        file_config = self.config.file
        closes = {s: Decimal(str(f["close"][-1])) for s, f in bars.items()}
        atr_values = {
            s: Decimal(str(v))
            for s, f in bars.items()
            if (v := f[f"atr_{ATR_PERIOD}"][-1]) is not None
        }
        trend_exit_sma = file_config.stops.trend_exit_sma
        trend_values = (
            {
                s: Decimal(str(v))
                for s, f in bars.items()
                if trend_exit_sma is not None
                and f"sma_{trend_exit_sma}" in f.columns
                and (v := f[f"sma_{trend_exit_sma}"][-1]) is not None
            }
            if trend_exit_sma is not None
            else {}
        )

        # 1) 戦略の判断
        ctx = StrategyContext(
            as_of=as_of,
            _bars=bars,
            _positions=positions,
            equity=equity,
            interval=file_config.universe.bar_interval,
            market=file_config.universe.market,
        )
        signals: list[Signal] = []
        for strategy in self.strategies:
            found = strategy.on_bars(ctx)
            log.info(
                "戦略の判断", code="wbjp.signals", strategy=strategy.describe(), signals=len(found)
            )
            signals.extend(found)

        combined = self.combiner.combine(signals)
        self.journal.record_signals(run_id, signals)
        self.journal.record_combined(run_id, combined)

        # 2) ストップを復元・更新し、抵触分を最優先で手仕舞う
        stops = StopBook(self.journal.load_stops())
        stops.ensure(
            positions,
            atr_values,
            as_of,
            atr_multiple=file_config.sizing.atr_stop_multiple,
            trailing=file_config.stops.trailing,
            initial_stop_pct=file_config.stops.initial_stop_pct,
        )
        stops.update_breakeven(closes, file_config.stops.breakeven_after_r)
        stops.update_trailing(closes, atr_values)

        # 3) サイジング
        lot_sizes = self._lot_sizes()
        targets = self.sizer.size(
            combined,
            SizingContext(
                equity=equity,
                buying_power=balance.buying_power,
                prices=closes,
                atr=atr_values,
                lot_sizes=lot_sizes,
                positions=positions,
                default_lot_size=self.rules.default_lot_size,
            ),
            entry_threshold=file_config.strategies.entry_threshold,
            exit_threshold=file_config.strategies.exit_threshold,
        )
        # ブローカーに逆指値を置いていても、日足の判定は保険として残す。
        # 逆指値が何かの理由で消えていた日にも、翌寄付で手仕舞える。
        targets = apply_stop_priority(
            targets,
            stops.take_profit_targets(
                closes,
                {s: p.quantity for s, p in positions.items()},
                lot_sizes,
                target_r=file_config.stops.take_profit_r,
                fraction=file_config.stops.take_profit_fraction,
                default_lot_size=self.rules.default_lot_size,
            )
            + stops.runner_targets(
                closes, {s: p.quantity for s, p in positions.items()}, trend_values
            )
            + stops.time_exit_targets(
                closes,
                as_of,
                stale_days=file_config.stops.stale_exit_days,
                max_days=file_config.stops.max_hold_days,
            )
            + stops.exit_targets(closes),
        )
        self.journal.record_targets(run_id, targets)

        # 4) 差分だけを注文にする
        open_orders = self.broker.get_open_orders()
        plan = reconcile(
            targets,
            positions,
            open_orders,
            closes,
            # 注文IDの種は run_id ではなく **基準日**。run_id は実行ごとに
            # 乱数を含むため、同じ日に2回走らせると別のIDが振られ、
            # journal の冪等チェックをすり抜けて二重発注になる。基準日を
            # 種にすれば「同じ取引日の同じ判断」は必ず同じIDになる。
            order_id_seed=f"{as_of:%Y%m%d}",
            settings=ReconcileSettings(
                order_type=file_config.execution.order_type,
                limit_offset=file_config.execution.limit_offset,
                tax_type=file_config.execution.tax_account_type,
            ),
            lot_sizes=lot_sizes,
            bought_today=self._bought_today(),
            rules=self.rules,
        )

        # 5) リスク判定
        risk_ctx = RiskContext(
            equity=equity,
            balance=balance,
            positions=positions,
            base_prices=closes,
            pending_value=_pending_buy_value(open_orders, closes),
            orders_today=self.journal.orders_today(),
        )
        approved, rejected = self.risk.check_all(plan.orders, risk_ctx)
        rejected.update(plan.skipped)

        # 6) 逆指値をブローカーに置く市場では、戦略の注文より先に古い逆指値を
        #    片付ける。手仕舞いの売りと逆指値が両方板に乗ると二重売却になる。
        stop_requests: list[OrderRequest] = []
        if self.config.file.uses_broker_stops and self.rules.broker_stop_order_type:
            stop_plan = sync_broker_stops(
                stops.all(),
                positions,
                open_orders,
                order_type=self.rules.broker_stop_order_type,
                order_id_seed=f"{as_of:%Y%m%d}",
                tax_type=file_config.execution.tax_account_type,
                pending_exits={o.symbol for o in approved if o.side is Side.SELL},
            )
            self._cancel_stops(stop_plan.cancel, live=allowed)
            stop_approved, stop_rejected = self.risk.check_all(stop_plan.place, risk_ctx)
            rejected.update({f"{k} (逆指値)": v for k, v in stop_rejected.items()})
            stop_requests = stop_approved

        # 7) 発注
        placed = self._place(run_id, approved, rejected, risk_ctx, live=allowed)
        placed += self._place(run_id, stop_requests, rejected, risk_ctx, live=allowed)

        self.journal.record_risk_events(run_id, rejected)
        self.journal.record_snapshot(run_id, as_of, positions.values())
        self.journal.save_stops(stops.all())

        result = CycleResult(
            run_id=run_id,
            as_of=as_of,
            live=allowed,
            reason=reason,
            signals=signals,
            combined=combined,
            targets=targets,
            planned=plan.orders + stop_requests,
            placed=placed,
            rejected=rejected,
        )
        log.info(
            "サイクル完了",
            run_id=run_id,
            planned=len(plan.orders),
            placed=len(placed),
            rejected=len(rejected),
            mode="実発注" if allowed else "dry-run",
        )
        return result

    def _place(
        self,
        run_id: str,
        requests: list[OrderRequest],
        rejected: dict[str, str],
        risk_ctx: RiskContext,
        *,
        live: bool,
    ) -> list[OrderRequest]:
        placed: list[OrderRequest] = []

        for request in requests:
            # journal に既にあるなら、同じ判断からの再実行。発注しない。
            if self.journal.was_placed(request.client_order_id):
                log.info(
                    "発注済みのためスキップ（冪等）",
                    symbol=request.symbol,
                    client_order_id=request.client_order_id,
                )
                continue

            if not live:
                log.info(
                    "[dry-run] 発注しません",
                    symbol=request.symbol,
                    side=request.side.value,
                    quantity=str(request.quantity),
                    limit_price=str(request.limit_price) if request.limit_price else None,
                    reason=request.reason,
                )
                self.journal.record_order(run_id, request, Journal.DRY_RUN_STATUS)
                continue

            try:
                # 発注前にブローカーの見積りと突き合わせる
                preview = self.broker.preview(request)
                # サイクル本体と**同じ**判断材料で見る。ここで equity=0 や
                # 空の建玉を渡すと、_position_weight が無条件に通過し、
                # 実弾の直前にある最後の関門が実質無効になる。
                # 残高だけは、直前の発注で減っている可能性があるため取り直す。
                decision = self.risk.check(
                    request,
                    replace(risk_ctx, balance=self.broker.get_balance()),
                    preview=preview,
                )
                if not decision.approved:
                    rejected[request.symbol] = decision.reason
                    continue

                ack = self.broker.place(request)
                self.journal.record_order(run_id, request, ack.status.value, ack.broker_order_id)
                placed.append(request)
            except BrokerError as exc:
                rejected[request.symbol] = str(exc)
                log.error("発注に失敗", symbol=request.symbol, error=str(exc))

        return placed

    def _cancel_stops(self, orders: list[Order], *, live: bool) -> None:
        for order in orders:
            if not live:
                log.info(
                    "[dry-run] 逆指値を取り消しません",
                    symbol=order.symbol,
                    client_order_id=order.client_order_id,
                    stop_price=str(order.stop_price),
                )
                continue
            try:
                self.broker.cancel(order.client_order_id)
                log.info(
                    "古い逆指値を取り消し",
                    symbol=order.symbol,
                    client_order_id=order.client_order_id,
                    stop_price=str(order.stop_price),
                )
            except BrokerError as exc:
                log.error("逆指値の取消に失敗", symbol=order.symbol, error=str(exc))

    def _refresh_bars(self, symbols: list[str]) -> None:
        if self.provider is None:
            return
        today = today_utc()
        warmup = max((s.warmup_bars for s in self.strategies), default=1)
        start = today - dt.timedelta(days=warmup * 2 + HISTORY_PADDING_DAYS)
        self.store.sync(self.provider, symbols, start, today)

    def _load_bars(self, symbols: list[str]) -> dict[str, pl.DataFrame]:
        loaded = self.store.read_many(symbols)
        expressions = [atr(ATR_PERIOD)]
        trend_exit_sma = self.config.file.stops.trend_exit_sma
        if trend_exit_sma is not None:
            expressions.append(sma(trend_exit_sma))
        return {
            symbol: frame.with_columns(expressions)
            for symbol, frame in loaded.items()
            if frame.height > ATR_PERIOD
        }

    def _lot_sizes(self) -> dict[str, Decimal]:
        return {s: Decimal(v) for s, v in self.config.file.universe.lot_size_overrides.items()}

    def _bought_today(self) -> set[str]:
        """当日買い付けた銘柄。差金決済の防止に使う。"""
        getter = getattr(self.broker, "bought_today", None)
        return set(getter) if getter else set()
