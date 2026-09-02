"""バックテスト結果の分析値（ドローダウン期間・トレード統計・シャープレシオ）。

:class:`~wbjp.engine.backtest.BacktestResult` の資産曲線と約定列だけから出す。
外部ライブラリには頼らない（以前は Backtrader のアナライザに任せていた）。
"""

from __future__ import annotations

import math
from collections import deque
from dataclasses import dataclass
from decimal import Decimal
from itertools import pairwise

from wbcore.domain.models import Fill, Side

#: 年率換算に使う 1 年の営業日数（東証）。
TRADING_DAYS = 245


@dataclass(frozen=True, slots=True)
class ClosedTrade:
    """買いと売りを突き合わせて閉じた 1 往復（FIFO）。"""

    symbol: str
    quantity: Decimal
    cost: Decimal  # 買いの約定代金＋手数料（数量ぶん）
    proceeds: Decimal  # 売りの約定代金−手数料（数量ぶん）

    @property
    def pnl(self) -> Decimal:
        return self.proceeds - self.cost


def closed_trades(fills: list[Fill]) -> list[ClosedTrade]:
    """約定列を銘柄ごとに FIFO で突き合わせ、閉じた往復にする。

    売りが保有を超えた分（空売り）は対象外。手数料は数量比で按分する。
    """
    open_lots: dict[str, deque[tuple[Decimal, Decimal]]] = {}  # symbol → (数量, 単価+手数料/株)
    trades: list[ClosedTrade] = []
    for fill in fills:
        if fill.quantity <= 0:
            continue
        lots = open_lots.setdefault(fill.symbol, deque())
        per_share_fee = fill.fee / fill.quantity
        if fill.side is Side.BUY:
            lots.append((fill.quantity, fill.price + per_share_fee))
            continue
        remaining = fill.quantity
        while remaining > 0 and lots:
            lot_qty, lot_cost = lots[0]
            take = min(lot_qty, remaining)
            trades.append(
                ClosedTrade(
                    symbol=fill.symbol,
                    quantity=take,
                    cost=take * lot_cost,
                    proceeds=take * (fill.price - per_share_fee),
                )
            )
            remaining -= take
            if take == lot_qty:
                lots.popleft()
            else:
                lots[0] = (lot_qty - take, lot_cost)
    return trades


def longest_drawdown(equity: list[Decimal]) -> int:
    """直近の最高値を下回り続けた最長の本数（0 なら常に高値更新）。"""
    peak = Decimal("-Infinity")
    longest = current = 0
    for value in equity:
        if value >= peak:
            peak = value
            current = 0
        else:
            current += 1
            longest = max(longest, current)
    return longest


def sharpe_ratio(equity: list[Decimal], *, periods_per_year: int = TRADING_DAYS) -> float | None:
    """資産曲線の 1 本ごとのリターンから年率シャープレシオ（無リスク金利 0）。

    リターンが 2 本未満、または標準偏差が 0 なら None。標本標準偏差（n−1）。
    """
    returns = [float(b / a - 1) for a, b in pairwise(equity) if a > 0]
    if len(returns) < 2:
        return None
    mean = sum(returns) / len(returns)
    variance = sum((r - mean) ** 2 for r in returns) / (len(returns) - 1)
    if variance <= 0:
        return None
    return mean / math.sqrt(variance) * math.sqrt(periods_per_year)


def analyze(equity: list[Decimal], fills: list[Fill]) -> dict[str, object]:
    """表示用の平らな辞書。値の無いものは "-"。"""
    trades = closed_trades(fills)
    won = sum(1 for t in trades if t.pnl > 0)
    sharpe = sharpe_ratio(equity)
    out: dict[str, object] = {
        "最長ドローダウン期間 (本)": longest_drawdown(equity),
        "決済トレード数": len(trades),
        "勝率": f"{won / len(trades):.1%}" if trades else "-",
        "平均損益/トレード": (
            f"{sum((t.pnl for t in trades), start=Decimal(0)) / len(trades):.2f}" if trades else "-"
        ),
        "シャープレシオ (年率)": f"{sharpe:.2f}" if sharpe is not None else "-",
    }
    return out
