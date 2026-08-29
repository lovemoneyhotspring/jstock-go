"""Backtrader を約定エンジンに使うバックテスト。

**何のためにあるか**

自前の :class:`~wbjp.engine.backtest.BacktestRunner` は判断も約定も自分で
書いている。約定モデル（翌日寄付・滑り・逆指値のトリガー）にバグがあると、
判断が正しくても成績が歪む。そこで約定と口座管理だけを、広く使われている
Backtrader の Cerebro/Broker に差し替えた第2エンジンを用意し、
同じ判断・同じ足で結果を突き合わせる。**2つのエンジンが一致しなければ
どちらかが間違っている**、という検証装置。

**共通化しているもの**

売買判断は :class:`~wbjp.engine.backtest.DecisionPipeline` を丸ごと使う。
戦略・合成・サイジング・ストップ・リコンサイル・リスク判定のいずれも
ここでは再実装しない。Backtrader の Strategy はただの橋渡しで、
「判断が返した OrderRequest を Backtrader の注文に翻訳する」ことしかしない。

**約定モデルの対応**

=================  ====================================  ===========================
注文               PaperBroker                           Backtrader
=================  ====================================  ===========================
成行 (MARKET)      翌日寄付 × (1 ± 滑り)                  翌バー寄付 + ``slip_open``
指値 (LIMIT)       寄付が指値以内なら寄付で約定           バー内で指値に届けば約定
逆指値 (STOP)      寄付 ≦ 逆指値なら寄付、安値で到達なら   同じ（``Stop`` 注文）
                   逆指値、いずれも滑りあり
DAY の失効         翌日の約定処理のあと                   橋渡しが翌 ``next()`` で取消
=================  ====================================  ===========================

指値だけは Backtrader の方が楽観的（日足でも高安で判定する）。
突き合わせに使うときは ``execution.order_type = "market"`` にすること。

Backtrader は float で計算するため、金額は最後に Decimal へ戻して
セント/円に丸める。1円未満の差は正常。
"""

from __future__ import annotations

import datetime as dt
from decimal import ROUND_HALF_UP, Decimal
from typing import Any

import backtrader as bt
import polars as pl

from wbcore.broker.base import OrderRejectedError
from wbcore.broker.paper import DEFAULT_COMMISSION_RATE, DEFAULT_SLIPPAGE_RATE
from wbcore.clock import ensure_utc
from wbcore.domain.models import (
    Balance,
    Fill,
    Order,
    OrderRequest,
    OrderStatus,
    OrderType,
    Position,
    Side,
    TimeInForce,
)
from wbcore.logging import get_logger
from wbjp.config import FileConfig
from wbjp.engine.backtest import (
    AccountSnapshot,
    BacktestResult,
    BarIndex,
    DayRecord,
    DecisionPipeline,
)
from wbjp.risk.stops import StopBook
from wbjp.strategy.base import Strategy

log = get_logger(__name__)

_EXECTYPE: dict[OrderType, int] = {
    OrderType.MARKET: bt.Order.Market,
    OrderType.LIMIT: bt.Order.Limit,
    OrderType.STOP_LOSS: bt.Order.Stop,
    OrderType.STOP_LOSS_LIMIT: bt.Order.StopLimit,
}


class BacktraderRunner:
    """Backtrader で約定させる日足バックテスト。

    :class:`~wbjp.engine.backtest.BacktestRunner` と同じ引数・同じ戻り値。
    """

    def __init__(
        self,
        strategies: list[Strategy],
        config: FileConfig,
        *,
        initial_cash: Decimal = Decimal(1_000_000),
        commission_rate: Decimal | None = None,
        slippage_rate: Decimal = DEFAULT_SLIPPAGE_RATE,
    ) -> None:
        self.strategies = strategies
        self.config = config
        self.pipeline = DecisionPipeline(strategies, config)
        self.initial_cash = initial_cash
        self.slippage_rate = slippage_rate
        if commission_rate is not None:
            self.commission_rate = commission_rate
        elif config.universe.market.value == "US":
            self.commission_rate = Decimal(0)  # PaperBroker と同じ扱い
        else:
            self.commission_rate = DEFAULT_COMMISSION_RATE

    @property
    def stops(self) -> StopBook:
        return self.pipeline.stops

    def run(
        self,
        bars: dict[str, pl.DataFrame],
        start: dt.date | None = None,
        end: dt.date | None = None,
    ) -> BacktestResult:
        if not bars:
            raise ValueError("足データが空です")

        if self.pipeline.interval.is_intraday:
            raise ValueError(
                "Backtrader エンジンは日足のみ対応です。日中足は --engine native を使ってください"
            )
        enriched = self.pipeline.precompute_indicators(bars)
        trading_days = self.pipeline.trading_days(enriched, start, end)
        if not trading_days:
            raise ValueError("対象期間に取引日がありません")
        indexed = self.pipeline.index_by_date(enriched)

        cerebro = bt.Cerebro(stdstats=False)
        cerebro.broker.setcash(float(self.initial_cash))
        # 約定代金に対する比率手数料（株式向け）
        cerebro.broker.setcommission(commission=float(self.commission_rate))
        # 成行・逆指値は寄付で不利な方向に滑らせ、指値は滑らせない（PaperBroker と同じ）。
        # slip_out=True: 滑った価格が当日の高安を外れても、そのまま約定させる。
        # False にすると高安で頭打ちになり、寄付が高値の日に滑りがゼロになって
        # PaperBroker と約定価格が食い違う（実データで 207 件中 20 件ほど）。
        cerebro.broker.set_slippage_perc(
            float(self.slippage_rate),
            slip_open=True,
            slip_limit=False,
            slip_match=True,
            slip_out=True,
        )

        for symbol, frame in sorted(enriched.items()):
            cerebro.adddata(_to_feed(frame, end), name=symbol)

        cerebro.addanalyzer(bt.analyzers.DrawDown, _name="drawdown")
        cerebro.addanalyzer(bt.analyzers.TradeAnalyzer, _name="trades")
        cerebro.addanalyzer(
            bt.analyzers.SharpeRatio,
            _name="sharpe",
            timeframe=bt.TimeFrame.Days,
            riskfreerate=0.0,
            annualize=True,
        )
        cerebro.addstrategy(
            _Bridge,
            pipeline=self.pipeline,
            bars=enriched,
            indexed=indexed,
            start=start,
            end=end,
            currency=self.pipeline.rules.currency,
        )

        strategy = cerebro.run()[0]
        result: BacktestResult = strategy.result
        result.final_equity = _money(cerebro.broker.getvalue(), self.pipeline.rules.currency)
        result.analysis = _collect_analysis(strategy)
        log.info(
            "バックテスト完了 (backtrader)", **{k: str(v) for k, v in result.summary().items()}
        )
        return result


# --------------------------------------------------------------------------
# 橋渡し
# --------------------------------------------------------------------------


class _Bridge(bt.Strategy):  # type: ignore[misc]
    """DecisionPipeline の判断を Backtrader の注文に翻訳する。

    売買ロジックは持たない。持ってはいけない。
    """

    params = (
        ("pipeline", None),
        ("bars", None),
        ("indexed", None),
        ("start", None),
        ("end", None),
        ("currency", "JPY"),
    )

    def __init__(self) -> None:
        self.pipeline: DecisionPipeline = self.p.pipeline
        self.bars: dict[str, pl.DataFrame] = self.p.bars
        self.indexed: BarIndex = self.p.indexed
        self.currency: str = self.p.currency
        self.by_symbol: dict[str, Any] = {d._name: d for d in self.datas}

        #: 未約定の注文。client_order_id → (Backtrader の注文, 元のリクエスト)
        self.pending: dict[str, tuple[Any, OrderRequest]] = {}
        self.fills_today: list[Fill] = []
        self.bought_today: set[str] = set()
        self.rejected_today: dict[str, str] = {}
        self.counter = 0
        self.result = BacktestResult(initial_equity=_money(self.broker.getvalue(), self.currency))

    # -- 通知 ---------------------------------------------------------------

    def notify_order(self, order: Any) -> None:
        request: OrderRequest | None = order.info.get("request")
        if request is None:  # 自分が出していない注文は無い筈だが、念のため
            return
        cid = request.client_order_id

        if order.status == order.Completed:
            executed = order.executed
            fill = Fill(
                client_order_id=cid,
                symbol=request.symbol,
                side=request.side,
                quantity=Decimal(abs(executed.size)),
                price=Decimal(str(executed.price)),
                fee=_money(executed.comm, self.currency),
                # Backtrader の時刻は tz 無し。規約（時刻は必ず時間帯付き）に合わせ UTC を付ける
                filled_at=ensure_utc(bt.num2date(executed.dt)),
            )
            self.fills_today.append(fill)
            if request.side is Side.BUY:
                self.bought_today.add(request.symbol)
            self.pending.pop(cid, None)
        elif order.status in (order.Canceled, order.Expired):
            self.pending.pop(cid, None)
        elif order.status in (order.Margin, order.Rejected):
            self.pending.pop(cid, None)
            self.rejected_today[request.symbol] = (
                f"backtrader が約定を拒否 ({order.getstatusname()})"
            )

    # -- 1日 ----------------------------------------------------------------

    def next(self) -> None:
        # 足が更新された銘柄の日付が「今日」。更新の無い銘柄は前の日付のまま。
        today: dt.date = max(d.datetime.date(0) for d in self.datas)

        # 1) 当日の寄付で約定しなかった DAY 注文を失効させる（GTC の逆指値は残す）
        for cid, (order, request) in list(self.pending.items()):
            if request.time_in_force is not TimeInForce.GTC:
                self.cancel(order)
                self.pending.pop(cid, None)

        in_range = (self.p.start is None or today >= self.p.start) and (
            self.p.end is None or today <= self.p.end
        )
        if not in_range:
            self._end_of_day()
            return

        # 2) 当日の終値までで判断する
        account = self._snapshot()
        plan = self.pipeline.decide(
            self.bars, self.indexed, today, account, order_id_seed=f"bt-{self.counter}"
        )

        for stale in plan.cancel:
            entry = self.pending.pop(stale.client_order_id, None)
            if entry is not None:
                self.cancel(entry[0])

        rejected = {**self.rejected_today, **plan.rejected}
        placed: list[str] = []
        for request in plan.place:
            try:
                self._submit(request)
                placed.append(request.client_order_id)
            except Exception as exc:
                rejected[request.symbol] = str(exc)

        self.result.records.append(
            DayRecord(
                date=today,
                equity=account.equity,
                cash=account.balance.cash_balance,
                signals=plan.signals,
                combined=plan.combined,
                targets={t.symbol: t.quantity for t in plan.targets},
                orders=placed,
                fills=list(self.fills_today),
                rejected=rejected,
            )
        )
        self.result.fills.extend(self.fills_today)
        self.counter += 1
        self._end_of_day()

    def _end_of_day(self) -> None:
        self.fills_today = []
        self.bought_today = set()
        self.rejected_today = {}

    # -- 翻訳 ---------------------------------------------------------------

    def _snapshot(self) -> AccountSnapshot:
        positions: dict[str, Position] = {}
        for symbol, data in self.by_symbol.items():
            pos = self.getposition(data)
            if pos.size <= 0:
                continue
            quantity = Decimal(pos.size)
            positions[symbol] = Position(
                symbol=symbol,
                quantity=quantity,
                available_quantity=quantity,
                cost_price=Decimal(str(pos.price)),
                last_price=Decimal(str(data.close[0])),
                currency=self.currency,
            )

        cash = _money(self.broker.getcash(), self.currency)
        value = _money(self.broker.getvalue(), self.currency)
        balance = Balance(
            currency=self.currency,
            cash_balance=cash,
            buying_power=cash,
            market_value=value - cash,
            unrealized_pnl=sum((p.unrealized_pnl for p in positions.values()), start=Decimal(0)),
        )
        open_orders = [_to_order(order, request) for order, request in self.pending.values()]
        return AccountSnapshot(
            positions=positions,
            balance=balance,
            open_orders=open_orders,
            bought_today=set(self.bought_today),
        )

    def _submit(self, request: OrderRequest) -> None:
        data = self.by_symbol.get(request.symbol)
        if data is None:
            raise OrderRejectedError(f"{request.symbol}: 足データが無い")
        if request.quantity != request.quantity.to_integral_value():
            raise OrderRejectedError(f"{request.symbol}: 端数株 {request.quantity} は扱えない")
        size = int(request.quantity)

        if request.side is Side.SELL:
            held = self.getposition(data).size
            if size > held:
                # 空売りはしない（PaperBroker と同じ）
                raise OrderRejectedError(
                    f"{request.symbol}: 保有 {held} 株に対し {size} 株の売り注文"
                )
        elif request.order_type.is_stop:
            raise OrderRejectedError("買いの逆指値はこのシステムでは扱わない")

        kwargs: dict[str, Any] = {
            "data": data,
            "size": size,
            "exectype": _EXECTYPE[request.order_type],
            # 失効は自前で管理する（Backtrader の DAY は日足だと翌バー前に切れる）
            "valid": None,
        }
        if request.order_type is OrderType.LIMIT:
            kwargs["price"] = float(request.limit_price)  # type: ignore[arg-type]
        elif request.order_type is OrderType.STOP_LOSS:
            kwargs["price"] = float(request.stop_price)  # type: ignore[arg-type]
        elif request.order_type is OrderType.STOP_LOSS_LIMIT:
            kwargs["price"] = float(request.stop_price)  # type: ignore[arg-type]
            kwargs["plimit"] = float(request.limit_price)  # type: ignore[arg-type]

        order = self.buy(**kwargs) if request.side is Side.BUY else self.sell(**kwargs)
        order.addinfo(request=request)
        self.pending[request.client_order_id] = (order, request)


# --------------------------------------------------------------------------
# 補助
# --------------------------------------------------------------------------


def _to_feed(frame: pl.DataFrame, end: dt.date | None) -> Any:
    """polars の日足を Backtrader のデータフィードにする。"""
    selected = frame.select(["date", "open", "high", "low", "close", "volume"])
    if end is not None:
        selected = selected.filter(pl.col("date") <= end)
    pdf = selected.to_pandas()
    pdf["date"] = pdf["date"].astype("datetime64[ns]")
    pdf = pdf.set_index("date")
    return bt.feeds.PandasData(dataname=pdf, openinterest=None)


def _to_order(order: Any, request: OrderRequest) -> Order:
    """未約定の Backtrader 注文を、リコンサイルが読める形にする。"""
    return Order(
        client_order_id=request.client_order_id,
        broker_order_id=str(order.ref),
        symbol=request.symbol,
        side=request.side,
        order_type=request.order_type,
        quantity=request.quantity,
        filled_quantity=Decimal(0),
        status=OrderStatus.SUBMITTED,
        limit_price=request.limit_price,
        stop_price=request.stop_price,
        time_in_force=request.time_in_force,
    )


def _money(value: float, currency: str) -> Decimal:
    unit = Decimal("1") if currency == "JPY" else Decimal("0.01")
    return Decimal(str(value)).quantize(unit, rounding=ROUND_HALF_UP)


def _collect_analysis(strategy: Any) -> dict[str, object]:
    """アナライザの出力を、表示しやすい平らな辞書にする。"""
    out: dict[str, object] = {}
    try:
        dd = strategy.analyzers.drawdown.get_analysis()
        out["最大ドローダウン (backtrader)"] = f"{dd['max']['drawdown'] / 100:.2%}"
        out["最大ドローダウン期間 (バー)"] = int(dd["max"]["len"])
    except KeyError, AttributeError:
        pass
    try:
        trades = strategy.analyzers.trades.get_analysis()
        total = trades.get("total", {}).get("closed", 0)
        won = trades.get("won", {}).get("total", 0)
        out["決済トレード数"] = total
        out["勝率"] = f"{won / total:.1%}" if total else "-"
        pnl = trades.get("pnl", {}).get("net", {})
        if pnl:
            out["平均損益/トレード"] = f"{pnl.get('average', 0.0):.2f}"
    except KeyError, AttributeError, ZeroDivisionError:
        pass
    try:
        sharpe = strategy.analyzers.sharpe.get_analysis().get("sharperatio")
        out["シャープレシオ (年率)"] = f"{sharpe:.2f}" if sharpe is not None else "-"
    except KeyError, AttributeError:
        pass
    return out
