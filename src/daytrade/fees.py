"""日本株（現物）の手数料と、資金から銘柄数を決める規則。

手数料は立花証券の**定額コース**（:data:`wbcore.broker.tachibana.FLAT_RATE_TABLE`。
1 日の現物約定代金の**合計**で段階が決まり、12 万円まで 0 円、20 万円まで 176 円、
50 万円まで 253 円、100 万円まで 506 円、以後 100 万円ごとに 253 円）。信用取引は
手数料 0 円で、こちらは :mod:`daytrade.backtest` が ``commission=False`` で扱う。

1 日の合計で決まるので「1 注文をいくらにするか」で bp は大きくは変わらないが、
合計が段階の境目を越えるかどうかで変わる。**銘柄数は資金を 1 注文の目安額で
割って決める**規則（:func:`positions_for`）はそのまま。
"""

from __future__ import annotations

from decimal import ROUND_HALF_UP, Decimal

from wbcore.broker.tachibana import FLAT_RATE_STEP, FLAT_RATE_TABLE, flat_rate_commission

__all__ = [
    "FLAT_RATE_STEP",
    "FLAT_RATE_TABLE",
    "commission",
    "order_fee_estimate",
    "positions_for",
    "round_trip_bp",
]


def commission(day_total: Decimal) -> Decimal:
    """その日の現物約定代金の合計 ``day_total`` に対する手数料（1 日分の総額）。0 以下は 0。"""
    return flat_rate_commission(day_total)


def order_fee_estimate(amount: Decimal) -> Decimal:
    """1 注文の片道手数料の見込み。その注文だけを往復した日として（合計 = 2 × 代金）見積もる。"""
    if amount <= 0:
        return Decimal(0)
    return commission(amount * 2) / 2


def round_trip_bp(amount: Decimal) -> Decimal:
    """1 注文だけを往復したときの手数料を bp で。約定代金が 0 以下なら 0。"""
    if amount <= 0:
        return Decimal(0)
    return commission(amount * 2) / amount * 10_000


def positions_for(max_capital: Decimal, order_budget: Decimal, max_positions: int) -> int:
    """資金から銘柄数 N を決める。

    ``max_capital ÷ order_budget`` を四捨五入し、1 以上 ``max_positions`` 以下に収める
    （200 万 ÷ 67 万 = 2.98 → 3）。1 注文あたりの予算は呼び出し側で ``max_capital ÷ N``
    にする（余りを均等に配る）。

    Raises:
        ValueError: 資金が 1 注文の目安に届かないとき。
    """
    if max_capital <= 0 or order_budget <= 0:
        raise ValueError("max_capital と order_budget は正の値")
    if max_positions < 1:
        raise ValueError("max_positions は 1 以上")
    n = int((max_capital / order_budget).to_integral_value(rounding=ROUND_HALF_UP))
    if n < 1:
        raise ValueError(
            f"資金 {max_capital:,.0f} 円が 1 注文の目安 {order_budget:,.0f} 円の半分に届きません。"
            "order_budget を下げるか資金を増やしてください"
        )
    return min(n, max_positions)
