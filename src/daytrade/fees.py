"""Webull 証券の日本株（現物）手数料と、資金から銘柄数を決める規則。

手数料は約定代金の段階制で、1 注文 20〜100 万円は一律 275 円。1 注文が小さいほど
bp が跳ね上がる（20 万円で 11.5 bp、100 万円で 2.75 bp）ので、**銘柄数は資金を
1 注文の目安額で割って決める**。研究では 200 万円なら 67 万円 × 3 が最良だった。

出典: https://www.webull.co.jp/pricing（2026-08-31 時点、税込）。
"""

from __future__ import annotations

from decimal import ROUND_HALF_UP, Decimal

#: (約定代金の上限（この値以下）, 手数料（円）)
_BRACKETS: tuple[tuple[Decimal, Decimal], ...] = (
    (Decimal(50_000), Decimal(55)),
    (Decimal(100_000), Decimal(99)),
    (Decimal(200_000), Decimal(115)),
    (Decimal(1_000_000), Decimal(275)),
    (Decimal(1_500_000), Decimal(535)),
    (Decimal(30_000_000), Decimal(640)),
)
#: 3,000 万円超。
_ABOVE = Decimal(1_070)


def commission(amount: Decimal) -> Decimal:
    """約定代金 1 件の片道手数料（円）。0 以下は 0。"""
    if amount <= 0:
        return Decimal(0)
    for bound, fee in _BRACKETS:
        if amount <= bound:
            return fee
    return _ABOVE


def round_trip_bp(amount: Decimal) -> Decimal:
    """往復の手数料を bp で。約定代金が 0 以下なら 0。"""
    if amount <= 0:
        return Decimal(0)
    return commission(amount) * 2 / amount * 10_000


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
