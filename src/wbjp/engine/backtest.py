"""バックテスト。

**本番と同じ部品を使う。** 戦略・合成・サイジング・リコンサイル・リスクは
すべてライブと共通で、差し替わるのは :class:`~wbcore.broker.paper.PaperBroker`
だけ。バックテスト専用の売買ロジックは1行も書かない。

これを守らないと「バックテストでは動いたのに本番で挙動が違う」が起きる。

1日の流れ:
    1. 前の足で出した注文を、この足の**寄付**で約定させる
    2. この足の終値までの足で判断する（未来は見えない）
    3. 合成 → サイジング → 差分 → リスク判定 → 発注
    4. 注文は次の足に持ち越す

判断は終値、約定は次の足の寄付。この1本のずれが実運用の姿であり、
同じ足の終値で約定させると取れない価格で売買できることになる。

構成:
    :class:`DecisionPipeline`
        「終値までの足 + 口座の状態 → 出す注文・取り消す注文」の純粋な判断。
        ブローカーを知らない。
    :class:`BacktestRunner`
        :class:`~wbcore.broker.paper.PaperBroker` で約定させる自前エンジン。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field
from decimal import Decimal

import polars as pl

from wbcore.broker.paper import PaperBroker
from wbcore.domain.market_rules import MarketRules, rules_for
from wbcore.domain.models import (
    Balance,
    CombinedSignal,
    Fill,
    Order,
    OrderRequest,
    Position,
    Signal,
    TargetPosition,
)
from wbcore.indicators.ohlcv import atr, donchian_low, ema, sma
from wbcore.logging import get_logger
from wbjp.config import FileConfig
from wbjp.engine.analysis import analyze
from wbjp.engine.reconcile import ReconcileSettings, reconcile
from wbjp.portfolio.sizer import SizingContext, build_sizer
from wbjp.risk.limits import RiskContext, RiskManager
from wbjp.risk.stops import StopBook, apply_stop_priority
from wbjp.strategy.base import Strategy, StrategyContext
from wbjp.strategy.combiner import build_combiner

log = get_logger(__name__)

#: サイジングとストップに使う ATR の期間（足の本数）。
ATR_PERIOD = 14

#: 足を一意にする鍵（日付）。
BarKey = dt.date

#: 銘柄 → 日付 → 列名 → 値。日付で引ける形に変換した足。
BarIndex = dict[str, dict[BarKey, dict[str, Decimal]]]


@dataclass(frozen=True, slots=True)
class DayRecord:
    """1本の足ぶんの記録。デバッグと検証のすべてがここに残る。"""

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
    #: 追加の分析値（:func:`wbjp.engine.analysis.analyze`）。
    analysis: dict[str, object] = field(default_factory=dict)

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
        days = len({r.date for r in self.records})
        out: dict[str, object] = {"日数": days}
        if len(self.records) != days:
            out["判断回数"] = len(self.records)
        out.update(
            {
                "初期資産": self.initial_equity,
                "最終資産": self.final_equity,
                "総リターン": f"{self.total_return:.2%}",
                "最大ドローダウン": f"{self.max_drawdown:.2%}",
                "約定回数": len(self.fills),
                "決済回数": len(wins),
            }
        )
        return out


# --------------------------------------------------------------------------
# 判断パイプライン（エンジン非依存）
# --------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class AccountSnapshot:
    """判断時点の口座の状態。ブローカーの実装から切り離した最小限の写し。"""

    positions: dict[str, Position]
    balance: Balance
    open_orders: list[Order]
    bought_today: set[str] = field(default_factory=set)

    @property
    def equity(self) -> Decimal:
        return self.balance.cash_balance + self.balance.market_value


@dataclass(frozen=True, slots=True)
class DecisionPlan:
    """1本の足での判断の結果。エンジンはこれを機械的にブローカーへ流すだけ。"""

    signals: list[Signal]
    combined: dict[str, CombinedSignal]
    targets: list[TargetPosition]
    #: リスク判定を通った発注。
    place: list[OrderRequest]
    rejected: dict[str, str]


class DecisionPipeline:
    """終値までの足と口座の状態から、出す注文を決める。

    ライブとバックテストが共有する売買判断の全部。ブローカーには触らず、
    「どの注文を出すか・消すか」を返すだけなので、約定モデルを差し替えても
    判断そのものは 1 bit も変わらない。

    """

    def __init__(self, strategies: list[Strategy], config: FileConfig) -> None:
        self.strategies = strategies
        self.config = config
        self.combiner = build_combiner(
            config.strategies.combiner,
            {s.name: s.weight for s in config.strategies.enabled},
        )
        self.sizer = build_sizer(config.sizing)
        self.rules = rules_for(
            config.universe.market, topix500=set(config.universe.topix500_symbols)
        )
        self.risk = RiskManager(config.risk, config.universe.symbols, self.rules)
        self.stops = StopBook()
        self.warmup = max((s.warmup_bars for s in strategies), default=1)

    @property
    def key(self) -> str:
        """足を一意にする列。"""
        return "date"

    # -- 足の前処理 ---------------------------------------------------------

    @property
    def trend_exit_col(self) -> str:
        return f"trend_exit_{self.config.stops.trend_exit_kind}_{self.config.stops.trend_exit_sma}"

    def _trend_exit_expr(self, period: int) -> pl.Expr:
        kind = self.config.stops.trend_exit_kind
        if kind == "ema":
            expr = ema(period)
        elif kind == "donchian":
            expr = donchian_low(period)  # 当日を除く N 本安値
        else:
            expr = sma(period)
        return expr.alias(self.trend_exit_col)

    @property
    def lookup_keys(self) -> list[str]:
        keys = ["open", "high", "low", "close", f"atr_{ATR_PERIOD}"]
        if self.config.stops.trend_exit_sma is not None:
            keys.append(self.trend_exit_col)
        if self.config.regime.enabled:
            keys.extend(self._regime_cols)
        return keys

    @property
    def _regime_cols(self) -> list[str]:
        r = self.config.regime
        return [f"sma_{r.sma_long}", f"sma_{r.sma_mid}", f"_regime_slope_{r.sma_long}"]

    def regime_exposure(self, indexed: BarIndex, point: BarKey) -> tuple[str, Decimal]:
        """その足の相場レジームと露出上限。無効なら常に強気・1.0。

        指数の足が無い足（ウォームアップ中）は判断できないので、
        **弱気扱い** にして建てない。判断材料が無いときに全力で建てるのは
        レジーム制御を入れた意図に反する。
        """
        r = self.config.regime
        if not r.enabled:
            return "bull", Decimal(1)
        row = indexed.get(r.benchmark, {}).get(point)
        if not row:
            return "bear", r.exposure_bear
        close = row.get("close")
        long_ma, mid_ma, slope = (row.get(c) for c in self._regime_cols)
        if close is None or long_ma is None or mid_ma is None or slope is None:
            return "bear", r.exposure_bear
        if close < long_ma:
            return "bear", r.exposure_bear
        if close < mid_ma or slope <= 0:
            return "caution", r.exposure_caution
        return "bull", r.exposure_bull

    def precompute_indicators(self, bars: dict[str, pl.DataFrame]) -> dict[str, pl.DataFrame]:
        """全期間ぶんの指標を一度だけ計算する。

        **なぜ先回りしてよいのか**

        本システムの指標はすべて因果的（rolling / ewm / shift のように
        過去だけを見る）。したがって i 行目の値は i 行目までの足だけで
        決まり、「全期間で計算してから切る」と「切ってから計算する」は
        必ず一致する。先読みバイアスは生じない。

        これをやらないと、足の本数ぶん指標を計算し直すことになり、
        バックテストの所要時間が本数の二乗に比例して伸びる。
        """
        expressions: dict[str, pl.Expr] = {}
        for strategy in self.strategies:
            for expr in getattr(strategy, "indicators", list)():
                expressions[expr.meta.output_name()] = expr
        # サイジングとストップが使う ATR は戦略と無関係に必要
        expressions.setdefault(f"atr_{ATR_PERIOD}", atr(ATR_PERIOD))
        trend_exit_sma = self.config.stops.trend_exit_sma
        if trend_exit_sma is not None:
            expressions.setdefault(self.trend_exit_col, self._trend_exit_expr(trend_exit_sma))
        if self.config.regime.enabled:
            r = self.config.regime
            long_col, mid_col, slope_col = self._regime_cols
            expressions.setdefault(long_col, sma(r.sma_long))
            expressions.setdefault(mid_col, sma(r.sma_mid))
            expressions[slope_col] = (
                pl.col("close").rolling_mean(r.sma_long)
                - pl.col("close").rolling_mean(r.sma_long).shift(r.slope_lookback)
            ).alias(slope_col)

        return {
            symbol: frame.with_columns(list(expressions.values())) for symbol, frame in bars.items()
        }

    def index_by_date(self, bars: dict[str, pl.DataFrame]) -> BarIndex:
        """鍵で引ける形に一度だけ変換する。

        毎回 ``filter`` を掛けると銘柄数×本数の総当たりになり、
        銘柄が増えるほど急に遅くなる。最初に索引を作っておく。
        """
        keys = self.lookup_keys
        key_column = self.key
        indexed: BarIndex = {}
        for symbol, frame in bars.items():
            by_key: dict[BarKey, dict[str, Decimal]] = {}
            for row in frame.iter_rows(named=True):
                values = {}
                for key in keys:
                    value = row.get(key)
                    if value is not None:
                        values[key] = Decimal(str(value))
                by_key[row[key_column]] = values
            indexed[symbol] = by_key
        return indexed

    @staticmethod
    def lookup(indexed: BarIndex, point: BarKey, key: str) -> dict[str, Decimal]:
        return {
            symbol: values[key]
            for symbol, by_key in indexed.items()
            if (values := by_key.get(point)) is not None and key in values
        }

    def trading_days(
        self, bars: dict[str, pl.DataFrame], start: dt.date | None, end: dt.date | None
    ) -> list[BarKey]:
        """判断する足の鍵を時刻順に。``start`` / ``end`` は暦日で切る。"""
        points: set[BarKey] = set()
        for frame in bars.values():
            selected = frame
            if start:
                selected = selected.filter(pl.col("date") >= start)
            if end:
                selected = selected.filter(pl.col("date") <= end)
            points.update(selected[self.key].to_list())
        return sorted(points)

    def trend_values(self, indexed: BarIndex, point: BarKey) -> dict[str, Decimal]:
        if self.config.stops.trend_exit_sma is None:
            return {}
        return self.lookup(indexed, point, self.trend_exit_col)

    # -- 判断 ---------------------------------------------------------------

    def decide(
        self,
        bars: dict[str, pl.DataFrame],
        indexed: BarIndex,
        today: BarKey,
        account: AccountSnapshot,
        *,
        order_id_seed: str,
    ) -> DecisionPlan:
        """この足の終値までで判断し、発注の計画を返す。"""
        point = today
        day = point

        closes = self.lookup(indexed, point, "close")
        atr_values = self.lookup(indexed, point, f"atr_{ATR_PERIOD}")
        trend_values = self.trend_values(indexed, point)

        positions = account.positions
        equity = account.equity
        lot_sizes = {s: Decimal(v) for s, v in self.config.universe.lot_size_overrides.items()}

        # この足までに切り詰めた足だけを戦略に渡す（先読みの遮断）
        visible = {
            symbol: frame.filter(pl.col(self.key) <= point) for symbol, frame in bars.items()
        }
        visible = {s: f for s, f in visible.items() if f.height >= self.warmup}

        ctx = StrategyContext(
            as_of=day,
            _bars=visible,
            _positions=positions,
            equity=equity,
        )

        signals: list[Signal] = []
        for strategy in self.strategies:
            signals.extend(strategy.on_bars(ctx))

        combined = self.combiner.combine(signals)

        # 相場レジーム: 弱気なら全手仕舞い＋新規禁止、警戒なら露出を絞る
        regime, exposure = self.regime_exposure(indexed, point)
        held_symbols = {s for s, p in positions.items() if p.quantity > 0}
        if exposure == 0:
            combined = {
                s: CombinedSignal(s, -1.0, reason=f"レジーム {regime}: 全手仕舞い")
                for s in held_symbols
            }
        elif exposure < 1:
            gross = sum((p.market_value for p in positions.values()), start=Decimal(0))
            if equity > 0 and gross / equity >= exposure:
                combined = {s: c for s, c in combined.items() if s in held_symbols}
        sizing_equity = equity * exposure

        # ストップを整備し、抵触分は戦略より優先して手仕舞う
        self.stops.ensure(
            positions,
            atr_values,
            day,
            atr_multiple=self.config.sizing.atr_stop_multiple,
            trailing=self.config.stops.trailing,
            initial_stop_pct=self.config.stops.initial_stop_pct,
            trailing_atr_multiple=self.config.stops.trailing_atr_multiple,
            trailing_pct=self.config.stops.trailing_pct,
        )
        self.stops.update_breakeven(closes, self.config.stops.breakeven_after_r)
        self.stops.update_trailing(closes, atr_values)

        quantities = {s: p.quantity for s, p in positions.items()}
        targets = self.sizer.size(
            combined,
            SizingContext(
                equity=sizing_equity,
                buying_power=account.balance.buying_power,
                prices=closes,
                atr=atr_values,
                lot_sizes=lot_sizes,
                positions=positions,
                default_lot_size=self.rules.default_lot_size,
            ),
            entry_threshold=self.config.strategies.entry_threshold,
            exit_threshold=self.config.strategies.exit_threshold,
        )
        targets = apply_stop_priority(
            targets,
            self.stops.take_profit_targets(
                closes,
                quantities,
                lot_sizes,
                target_r=self.config.stops.take_profit_r,
                fraction=self.config.stops.take_profit_fraction,
                default_lot_size=self.rules.default_lot_size,
            )
            + self.stops.runner_targets(
                closes, quantities, trend_values, always=self.config.stops.trend_exit_always
            )
            + self.stops.time_exit_targets(
                closes,
                day,
                stale_days=self.config.stops.stale_exit_days,
                max_days=self.config.stops.max_hold_days,
            )
            + self.stops.exit_targets(closes),
        )

        plan = reconcile(
            targets,
            positions,
            account.open_orders,
            closes,
            order_id_seed=order_id_seed,
            settings=ReconcileSettings(
                order_type=self.config.execution.order_type,
                limit_offset=self.config.execution.limit_offset,
                tax_type=self.config.execution.tax_account_type,
            ),
            lot_sizes=lot_sizes,
            bought_today=account.bought_today,
            rules=self.rules,
        )

        risk_ctx = RiskContext(
            equity=equity,
            balance=account.balance,
            positions=positions,
            base_prices=closes,
            realized_pnl_today=Decimal(0),
        )
        approved, rejected = self.risk.check_all(plan.orders, risk_ctx)

        return DecisionPlan(
            signals=signals,
            combined=combined,
            targets=targets,
            place=approved,
            rejected={**plan.skipped, **rejected},
        )


# --------------------------------------------------------------------------
# 自前エンジン
# --------------------------------------------------------------------------


class BacktestRunner:
    """足ごとのバックテスト（:class:`~wbcore.broker.paper.PaperBroker` で約定）。

    成行は常に「翌日の寄付」で約定し、未約定はその日で失効する
    （実運用より保守的＝約定しにくい側に倒す方針）。

    指値の約定判定は ``fill_model`` で切り替える。``"open"``（既定）は寄付だけで
    判定し、``"intrabar"`` は高安も見る。判断ロジックは同じなので、2 つを
    突き合わせれば約定モデルの違いだけが結果に出る（検証装置）。
    """

    def __init__(
        self,
        strategies: list[Strategy],
        config: FileConfig,
        *,
        initial_cash: Decimal = Decimal(1_000_000),
        commission_rate: Decimal | None = None,
        fill_model: str = "open",
    ) -> None:
        self.strategies = strategies
        self.config = config
        self.pipeline = DecisionPipeline(strategies, config)
        self.fill_model = fill_model

        broker_kwargs: dict[str, object] = {
            "initial_cash": initial_cash,
            "currency": self.rules.currency,
            "fill_model": fill_model,
        }
        if commission_rate is not None:
            broker_kwargs["commission_rate"] = commission_rate
        self.broker = PaperBroker(**broker_kwargs)  # type: ignore[arg-type]

    # 旧来の属性名を残す（テスト・ツールからの参照用）
    @property
    def rules(self) -> MarketRules:
        return self.pipeline.rules

    @property
    def stops(self) -> StopBook:
        return self.pipeline.stops

    @property
    def risk(self) -> RiskManager:
        return self.pipeline.risk

    def run(
        self,
        bars: dict[str, pl.DataFrame],
        start: dt.date | None = None,
        end: dt.date | None = None,
        *,
        cash_yield: pl.DataFrame | None = None,
    ) -> BacktestResult:
        """バックテストを実行する。

        Args:
            bars: 銘柄 → 日足（``date`` 昇順）。
            start: この日以降を売買対象にする。前の足はウォームアップに使う。
        """
        if not bars:
            raise ValueError("足データが空です")
        for symbol, frame in bars.items():
            if "date" not in frame.columns:
                raise ValueError(f"{symbol} の足に 'date' 列がありません")
        # 待機資金の年利（%）の日足。^IRX など。与えると現金に日割りで利息を付ける
        yields: dict[dt.date, Decimal] = {}
        if cash_yield is not None and cash_yield.height:
            yields = {
                d: Decimal(str(c)) / 100
                for d, c in cash_yield.select("date", "close").iter_rows()
                if c is not None
            }
        last_yield = Decimal(0)

        enriched = self.pipeline.precompute_indicators(bars)
        points = self.pipeline.trading_days(enriched, start, end)
        if not points:
            raise ValueError("対象期間に取引日がありません")

        indexed = self.pipeline.index_by_date(enriched)
        lookup = self.pipeline.lookup

        result = BacktestResult(initial_equity=self.broker.equity)

        previous_day: dt.date | None = None
        for index, point in enumerate(points):
            today = point

            # 0) 暦日が変わったときだけ行う処理: 利息・当日買付の記録の更新
            if today != previous_day:
                if yields:
                    last_yield = yields.get(today, last_yield)
                    if previous_day is not None:
                        self.broker.accrue_interest(last_yield, (today - previous_day).days)
                self.broker.begin_day()
                previous_day = today

            # 1) 前の足の注文をこの足の寄付で約定させ、残ったものを失効させる。
            #    失効を約定より先に行うと、注文が約定する機会を永遠に失う。
            if self.fill_model == "intrabar":
                fills = self.broker.settle(
                    lookup(indexed, point, "open"),
                    highs=lookup(indexed, point, "high"),
                    lows=lookup(indexed, point, "low"),
                )
            else:
                fills = self.broker.settle(lookup(indexed, point, "open"))
            self.broker.expire_open_orders()
            self.broker.mark(lookup(indexed, point, "close"))

            # 2) この足の終値までで判断する
            record = self._decide(enriched, indexed, point, index, fills)
            result.records.append(record)
            result.fills.extend(fills)

        result.final_equity = self.broker.equity
        result.analysis = analyze([r.equity for r in result.records], result.fills)
        log.info("バックテスト完了", **{k: str(v) for k, v in result.summary().items()})
        return result

    # -- 内部 ---------------------------------------------------------------

    def _decide(
        self,
        bars: dict[str, pl.DataFrame],
        indexed: BarIndex,
        point: BarKey,
        index: int,
        fills: list[Fill],
    ) -> DayRecord:
        account = AccountSnapshot(
            positions=self.broker.positions_by_symbol(),
            balance=self.broker.get_balance(),
            open_orders=self.broker.get_open_orders(),
            bought_today=self.broker.bought_today,
        )
        plan = self.pipeline.decide(bars, indexed, point, account, order_id_seed=f"bt-{index}")

        rejected = dict(plan.rejected)
        placed = []
        for request in plan.place:
            try:
                self.broker.place(request)
                placed.append(request.client_order_id)
            except Exception as exc:
                rejected[request.symbol] = str(exc)

        return DayRecord(
            date=point,
            equity=account.equity,
            cash=account.balance.cash_balance,
            signals=plan.signals,
            combined=plan.combined,
            targets={t.symbol: t.quantity for t in plan.targets},
            orders=placed,
            fills=fills,
            rejected=rejected,
        )
