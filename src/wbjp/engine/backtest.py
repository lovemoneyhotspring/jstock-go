"""バックテスト。

**本番と同じ部品を使う。** 戦略・合成・サイジング・リコンサイル・リスクは
すべてライブと共通で、差し替わるのは :class:`~wbjp.broker.paper.PaperBroker`
だけ。バックテスト専用の売買ロジックは1行も書かない。

これを守らないと「バックテストでは動いたのに本番で挙動が違う」が起きる。

1日の流れ:
    1. 前日の注文を、当日の**寄付**で約定させる
    2. 当日の終値までの足で判断する（未来は見えない）
    3. 合成 → サイジング → 差分 → リスク判定 → 発注
    4. 注文は翌営業日に持ち越す

判断は終値、約定は翌日の寄付。この1日のずれが実運用の姿であり、
同日終値で約定させると取れない価格で売買できることになる。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field
from decimal import Decimal

import polars as pl

from wbjp.broker.paper import PaperBroker
from wbjp.config import FileConfig
from wbjp.domain.models import CombinedSignal, Fill, Signal
from wbjp.engine.reconcile import ReconcileSettings, reconcile
from wbjp.indicators.ohlcv import atr
from wbjp.logging import get_logger
from wbjp.portfolio.sizer import SizingContext, build_sizer
from wbjp.risk.limits import RiskContext, RiskManager
from wbjp.risk.stops import StopBook, apply_stop_priority
from wbjp.strategy.base import Strategy, StrategyContext
from wbjp.strategy.combiner import build_combiner

log = get_logger(__name__)

#: サイジングとストップに使う ATR の期間。
ATR_PERIOD = 14


@dataclass(frozen=True, slots=True)
class DayRecord:
    """1日ぶんの記録。デバッグと検証のすべてがここに残る。"""

    date: dt.date
    equity: Decimal
    cash: Decimal
    signals: list[Signal]
    combined: dict[str, CombinedSignal]
    targets: dict[str, Decimal]
    orders: list[str]
    fills: list[Fill]
    rejected: dict[str, str]


@dataclass
class BacktestResult:
    """バックテストの結果。"""

    records: list[DayRecord] = field(default_factory=list)
    initial_equity: Decimal = Decimal(0)
    final_equity: Decimal = Decimal(0)
    fills: list[Fill] = field(default_factory=list)

    @property
    def total_return(self) -> Decimal:
        if self.initial_equity == 0:
            return Decimal(0)
        return (self.final_equity - self.initial_equity) / self.initial_equity

    @property
    def equity_curve(self) -> pl.DataFrame:
        return pl.DataFrame(
            {
                "date": [r.date for r in self.records],
                "equity": [float(r.equity) for r in self.records],
            }
        )

    @property
    def max_drawdown(self) -> Decimal:
        """最大ドローダウン（正の値で返す）。

        資産曲線の山からの下落率の最大値。総リターンより実感に近い
        指標で、運用を続けられるかどうかを左右する。
        """
        peak = Decimal("-Infinity")
        worst = Decimal(0)
        for record in self.records:
            peak = max(peak, record.equity)
            if peak > 0:
                worst = max(worst, (peak - record.equity) / peak)
        return worst

    def summary(self) -> dict[str, object]:
        wins = [f for f in self.fills if f.side.value == "SELL"]
        return {
            "日数": len(self.records),
            "初期資産": self.initial_equity,
            "最終資産": self.final_equity,
            "総リターン": f"{self.total_return:.2%}",
            "最大ドローダウン": f"{self.max_drawdown:.2%}",
            "約定回数": len(self.fills),
            "決済回数": len(wins),
        }


class BacktestRunner:
    """日足のバックテスト。"""

    def __init__(
        self,
        strategies: list[Strategy],
        config: FileConfig,
        *,
        initial_cash: Decimal = Decimal(1_000_000),
        commission_rate: Decimal | None = None,
    ) -> None:
        self.strategies = strategies
        self.config = config
        self.combiner = build_combiner(
            config.strategies.combiner,
            {s.name: s.weight for s in config.strategies.enabled},
        )
        self.sizer = build_sizer(config.sizing)
        self.risk = RiskManager(config.risk, config.universe.symbols)

        broker_kwargs: dict[str, object] = {"initial_cash": initial_cash}
        if commission_rate is not None:
            broker_kwargs["commission_rate"] = commission_rate
        self.broker = PaperBroker(**broker_kwargs)  # type: ignore[arg-type]

        self.stops = StopBook()

    def run(
        self,
        bars: dict[str, pl.DataFrame],
        start: dt.date | None = None,
        end: dt.date | None = None,
    ) -> BacktestResult:
        """バックテストを実行する。

        Args:
            bars: 銘柄 → 日足（``date`` 昇順）。
            start: この日以降を売買対象にする。前の足はウォームアップに使う。
        """
        if not bars:
            raise ValueError("足データが空です")

        enriched = self._precompute_indicators(bars)
        trading_days = self._trading_days(enriched, start, end)
        if not trading_days:
            raise ValueError("対象期間に取引日がありません")

        indexed = self._index_by_date(enriched)

        warmup = max((s.warmup_bars for s in self.strategies), default=1)
        result = BacktestResult(initial_equity=self.broker.equity)

        for index, today in enumerate(trading_days):
            # 1) 前日の注文を当日の寄付で約定させ、残ったものを失効させる。
            #    失効を約定より先に行うと、注文が約定する機会を永遠に失う。
            self.broker.begin_day()
            fills = self.broker.settle(self._lookup(indexed, today, "open"))
            self.broker.expire_open_orders()

            closes = self._lookup(indexed, today, "close")
            self.broker.mark(closes)

            # 2) 当日の終値までで判断する
            record = self._decide(
                enriched,
                today,
                closes,
                self._lookup(indexed, today, f"atr_{ATR_PERIOD}"),
                warmup,
                index,
                fills,
            )
            result.records.append(record)
            result.fills.extend(fills)

        result.final_equity = self.broker.equity
        log.info("バックテスト完了", **{k: str(v) for k, v in result.summary().items()})
        return result

    # -- 内部 ---------------------------------------------------------------

    def _decide(
        self,
        bars: dict[str, pl.DataFrame],
        today: dt.date,
        closes: dict[str, Decimal],
        atr_values: dict[str, Decimal],
        warmup: int,
        index: int,
        fills: list[Fill],
    ) -> DayRecord:
        positions = self.broker.positions_by_symbol()
        equity = self.broker.equity

        # その日までに切り詰めた足だけを戦略に渡す（先読みの遮断）
        visible = {symbol: frame.filter(pl.col("date") <= today) for symbol, frame in bars.items()}
        visible = {s: f for s, f in visible.items() if f.height >= warmup}

        ctx = StrategyContext(as_of=today, _bars=visible, _positions=positions, equity=equity)

        signals: list[Signal] = []
        for strategy in self.strategies:
            signals.extend(strategy.on_bars(ctx))

        combined = self.combiner.combine(signals)

        # ストップを整備し、抵触分は戦略より優先して手仕舞う
        self.stops.ensure(
            positions,
            atr_values,
            today,
            atr_multiple=self.config.sizing.atr_stop_multiple,
        )
        self.stops.update_trailing(closes, atr_values)

        targets = self.sizer.size(
            combined,
            SizingContext(
                equity=equity,
                buying_power=self.broker.get_balance().buying_power,
                prices=closes,
                atr=atr_values,
                lot_sizes={
                    s: Decimal(v) for s, v in self.config.universe.lot_size_overrides.items()
                },
                positions=positions,
            ),
            entry_threshold=self.config.strategies.entry_threshold,
            exit_threshold=self.config.strategies.exit_threshold,
        )
        targets = apply_stop_priority(targets, self.stops.exit_targets(closes))

        plan = reconcile(
            targets,
            positions,
            self.broker.get_open_orders(),
            closes,
            run_id=f"bt-{index}",
            settings=ReconcileSettings(
                order_type=self.config.execution.order_type,
                limit_offset=self.config.execution.limit_offset,
                tax_type=self.config.execution.tax_account_type,
            ),
            lot_sizes={s: Decimal(v) for s, v in self.config.universe.lot_size_overrides.items()},
            topix500=set(self.config.universe.topix500_symbols),
            bought_today=self.broker.bought_today,
        )

        approved, rejected = self.risk.check_all(
            plan.orders,
            RiskContext(
                equity=equity,
                balance=self.broker.get_balance(),
                positions=positions,
                base_prices=closes,
                realized_pnl_today=Decimal(0),
            ),
        )

        placed = []
        for request in approved:
            try:
                self.broker.place(request)
                placed.append(request.client_order_id)
            except Exception as exc:
                rejected[request.symbol] = str(exc)

        return DayRecord(
            date=today,
            equity=equity,
            cash=self.broker.get_balance().cash_balance,
            signals=signals,
            combined=combined,
            targets={t.symbol: t.quantity for t in targets},
            orders=placed,
            fills=fills,
            rejected={**plan.skipped, **rejected},
        )

    @staticmethod
    def _trading_days(
        bars: dict[str, pl.DataFrame], start: dt.date | None, end: dt.date | None
    ) -> list[dt.date]:
        days: set[dt.date] = set()
        for frame in bars.values():
            days.update(frame["date"].to_list())
        selected = sorted(days)
        if start:
            selected = [d for d in selected if d >= start]
        if end:
            selected = [d for d in selected if d <= end]
        return selected

    def _precompute_indicators(self, bars: dict[str, pl.DataFrame]) -> dict[str, pl.DataFrame]:
        """全期間ぶんの指標を一度だけ計算する。

        **なぜ先回りしてよいのか**

        本システムの指標はすべて因果的（rolling / ewm / shift のように
        過去だけを見る）。したがって i 行目の値は i 行目までの足だけで
        決まり、「全期間で計算してから切る」と「切ってから計算する」は
        必ず一致する。先読みバイアスは生じない。

        これをやらないと、日数ぶん指標を計算し直すことになり、
        バックテストの所要時間が日数の二乗に比例して伸びる。
        """
        expressions: dict[str, pl.Expr] = {}
        for strategy in self.strategies:
            for expr in getattr(strategy, "indicators", list)():
                expressions[expr.meta.output_name()] = expr
        # サイジングとストップが使う ATR は戦略と無関係に必要
        expressions.setdefault(f"atr_{ATR_PERIOD}", atr(ATR_PERIOD))

        return {
            symbol: frame.with_columns(list(expressions.values())) for symbol, frame in bars.items()
        }

    @staticmethod
    def _index_by_date(
        bars: dict[str, pl.DataFrame],
    ) -> dict[str, dict[dt.date, dict[str, Decimal]]]:
        """日付で引ける形に一度だけ変換する。

        毎日 ``filter`` を掛けると銘柄数×日数の総当たりになり、
        銘柄が増えるほど急に遅くなる。最初に索引を作っておく。
        """
        indexed: dict[str, dict[dt.date, dict[str, Decimal]]] = {}
        for symbol, frame in bars.items():
            by_date: dict[dt.date, dict[str, Decimal]] = {}
            for row in frame.iter_rows(named=True):
                values = {}
                for key in ("open", "close", f"atr_{ATR_PERIOD}"):
                    value = row.get(key)
                    if value is not None:
                        values[key] = Decimal(str(value))
                by_date[row["date"]] = values
            indexed[symbol] = by_date
        return indexed

    @staticmethod
    def _lookup(
        indexed: dict[str, dict[dt.date, dict[str, Decimal]]], day: dt.date, key: str
    ) -> dict[str, Decimal]:
        return {
            symbol: values[key]
            for symbol, by_date in indexed.items()
            if (values := by_date.get(day)) is not None and key in values
        }
