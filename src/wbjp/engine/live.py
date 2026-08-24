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
    - 同じ ``client_order_id`` が journal にあれば発注しない（冪等）
"""

from __future__ import annotations

import datetime as dt
import uuid
from dataclasses import dataclass, field
from decimal import Decimal

import polars as pl

from wbjp.broker.base import Broker, BrokerError
from wbjp.config import Config
from wbjp.data.provider import MarketDataProvider
from wbjp.data.store import BarStore
from wbjp.db.repo import Journal
from wbjp.domain.models import (
    Balance,
    CombinedSignal,
    OrderRequest,
    Position,
    Signal,
    TargetPosition,
)
from wbjp.engine.reconcile import ReconcileSettings, reconcile
from wbjp.indicators.ohlcv import atr
from wbjp.logging import get_logger
from wbjp.portfolio.sizer import SizingContext, build_sizer
from wbjp.risk.limits import RiskContext, RiskManager
from wbjp.risk.stops import StopBook, apply_stop_priority
from wbjp.strategy.base import Strategy, StrategyContext
from wbjp.strategy.combiner import build_combiner

log = get_logger(__name__)

ATR_PERIOD = 14

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
        self.combiner = build_combiner(
            file_config.strategies.combiner,
            {s.name: s.weight for s in file_config.strategies.enabled},
        )
        self.sizer = build_sizer(file_config.sizing)
        self.risk = RiskManager(file_config.risk, file_config.universe.symbols)

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
        run_id = run_id or f"{dt.date.today():%Y%m%d}-{uuid.uuid4().hex[:8]}"

        symbols = self.config.file.universe.symbols
        if not symbols:
            raise ValueError("universe.symbols が空です。売買対象を設定してください")

        # キルスイッチは他の何よりも先に効かせる
        if self.config.file.risk.kill_switch:
            log.error("キルスイッチが有効なため、サイクルを中止します")
            return CycleResult(run_id, as_of or dt.date.today(), False, reason)

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

        # 1) 戦略の判断
        ctx = StrategyContext(
            as_of=as_of,
            _bars=bars,
            _positions=positions,
            equity=equity,
        )
        signals: list[Signal] = []
        for strategy in self.strategies:
            found = strategy.on_bars(ctx)
            log.info("戦略の判断", strategy=strategy.describe(), signals=len(found))
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
        )
        stops.update_trailing(closes, atr_values)

        # 3) サイジング
        targets = self.sizer.size(
            combined,
            SizingContext(
                equity=equity,
                buying_power=balance.buying_power,
                prices=closes,
                atr=atr_values,
                lot_sizes=self._lot_sizes(),
                positions=positions,
            ),
            entry_threshold=file_config.strategies.entry_threshold,
            exit_threshold=file_config.strategies.exit_threshold,
        )
        targets = apply_stop_priority(targets, stops.exit_targets(closes))
        self.journal.record_targets(run_id, targets)

        # 4) 差分だけを注文にする
        plan = reconcile(
            targets,
            positions,
            self.broker.get_open_orders(),
            closes,
            run_id=run_id,
            settings=ReconcileSettings(
                order_type=file_config.execution.order_type,
                limit_offset=file_config.execution.limit_offset,
                tax_type=file_config.execution.tax_account_type,
            ),
            lot_sizes=self._lot_sizes(),
            topix500=set(file_config.universe.topix500_symbols),
            bought_today=self._bought_today(),
        )

        # 5) リスク判定
        approved, rejected = self.risk.check_all(
            plan.orders,
            RiskContext(
                equity=equity,
                balance=balance,
                positions=positions,
                base_prices=closes,
                orders_today=self.journal.orders_today(dt.date.today()),
            ),
        )
        rejected.update(plan.skipped)

        # 6) 発注
        placed = self._place(run_id, approved, rejected, live=allowed)

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
            planned=plan.orders,
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
                self.journal.record_order(run_id, request, "DRY_RUN")
                continue

            try:
                # 発注前にブローカーの見積りと突き合わせる
                preview = self.broker.preview(request)
                decision = self.risk.check(
                    request,
                    RiskContext(
                        equity=Decimal(0),
                        balance=self.broker.get_balance(),
                        base_prices={},
                    ),
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

    def _refresh_bars(self, symbols: list[str]) -> None:
        if self.provider is None:
            return
        today = dt.date.today()
        warmup = max((s.warmup_bars for s in self.strategies), default=1)
        start = today - dt.timedelta(days=warmup * 2 + HISTORY_PADDING_DAYS)
        self.store.sync(self.provider, symbols, start, today)

    def _load_bars(self, symbols: list[str]) -> dict[str, pl.DataFrame]:
        loaded = self.store.read_many(symbols)
        return {
            symbol: frame.with_columns(atr(ATR_PERIOD))
            for symbol, frame in loaded.items()
            if frame.height > ATR_PERIOD
        }

    def _lot_sizes(self) -> dict[str, Decimal]:
        return {s: Decimal(v) for s, v in self.config.file.universe.lot_size_overrides.items()}

    def _bought_today(self) -> set[str]:
        """当日買い付けた銘柄。差金決済の防止に使う。"""
        getter = getattr(self.broker, "bought_today", None)
        return set(getter) if getter else set()
