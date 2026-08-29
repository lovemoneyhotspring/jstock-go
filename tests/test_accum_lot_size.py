"""売買単位はブローカーの銘柄マスタを信じ、設定は控えにする。"""

from __future__ import annotations

import datetime as dt
from collections.abc import Iterable
from decimal import Decimal

from accum.execute import Contribution, resolve_lot_sizes
from accum.registry import TACTICS
from wbcore.broker.base import BrokerError
from wbcore.broker.paper import PaperBroker
from wbcore.domain.models import Market


class MasterBroker(PaperBroker):
    name = "master"

    def __init__(self, answers: dict[str, Decimal], *, fail: bool = False) -> None:
        super().__init__()
        self.answers, self.fail = answers, fail
        self.asked: list[str] = []

    def lot_sizes(self, symbols: Iterable[str]) -> dict[str, Decimal]:
        self.asked.extend(symbols)
        if self.fail:
            raise BrokerError("照会失敗")
        return {s: self.answers[s] for s in self.asked if s in self.answers}


def _contribution(symbol: str) -> Contribution:
    return Contribution(
        symbol=symbol,
        market=Market.JP,
        date=dt.date(2026, 8, 28),
        close=Decimal(830),
        amount=Decimal(25_000),
        multiplier=1,
        reason="",
        tactic=TACTICS.create("constant"),
    )


def test_api_wins_over_a_wrong_override_and_records_the_mismatch() -> None:
    broker = MasterBroker({"452A": Decimal("10.0000000000"), "1591": Decimal(1)})
    lots = resolve_lot_sizes(
        [_contribution("452A.T"), _contribution("1591.T"), _contribution("563A.T")],
        {"452A.T": 10, "1591.T": 10, "563A.T": 1},
        lambda market: broker,
    )
    assert broker.asked == ["452A", "1591", "563A"]  # ブローカー表記で照会
    assert lots.sizes == {"452A.T": 10, "1591.T": 1, "563A.T": 1}  # 563A は控えのまま
    assert lots.mismatches == {"1591.T": (10, 1)}
    assert lots.failures == {}


def test_lookup_failure_keeps_overrides_and_does_not_raise() -> None:
    broker = MasterBroker({}, fail=True)
    lots = resolve_lot_sizes([_contribution("452A.T")], {"452A.T": 10}, lambda market: broker)
    assert lots.sizes == {"452A.T": 10}
    assert Market.JP in lots.failures


def test_paper_broker_knows_no_lot_sizes() -> None:
    assert PaperBroker().lot_sizes(["452A"]) == {}
