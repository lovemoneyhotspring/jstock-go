"""デイトレ（``daytrade``）のテスト。純粋関数と台帳、CLI の骨格。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import polars as pl
import pytest

from daytrade.config import DaytradeConfig, SignalConfig, UniverseConfig
from daytrade.fees import commission, positions_for, round_trip_bp
from daytrade.ledger import DRY_RUN_STATUS, Ledger
from daytrade.quotes import CsvQuotes, parse_snapshot
from daytrade.select import Quote, pick, shares_for
from daytrade.universe import Inputs, candidates, segment_expr, to_broker_symbol
from wbcore.domain.models import OrderRequest, OrderStatus, OrderType, Side

UTC = dt.UTC
DAY = dt.date(2026, 9, 1)
PREV = dt.date(2026, 8, 31)

# --------------------------------------------------------------------------
# 手数料と N
# --------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("amount", "fee"),
    [
        (50_000, 55),
        (100_000, 99),
        (200_000, 115),
        (600_000, 275),
        (1_000_000, 275),
        (1_200_000, 535),
        (5_000_000, 640),
        (40_000_000, 1070),
    ],
)
def test_commission_brackets(amount: int, fee: int) -> None:
    assert commission(Decimal(amount)) == Decimal(fee)


def test_round_trip_bp_is_lower_for_bigger_orders() -> None:
    assert round_trip_bp(Decimal(1_000_000)) < round_trip_bp(Decimal(200_000))
    assert round_trip_bp(Decimal(0)) == 0


@pytest.mark.parametrize(
    ("capital", "n"),
    [
        (600_000, 1),
        (1_000_000, 1),
        (1_500_000, 2),
        (2_000_000, 3),
        (3_000_000, 4),
        (10_000_000, 10),
    ],
)
def test_positions_rounds_to_nearest_and_caps(capital: int, n: int) -> None:
    """200 万 ÷ 67 万 = 2.98 は 3（研究の結論）。上限は max_positions。"""
    assert positions_for(Decimal(capital), Decimal(670_000), 10) == n


def test_positions_rejects_too_small_capital() -> None:
    with pytest.raises(ValueError):
        positions_for(Decimal(300_000), Decimal(670_000), 10)


def test_config_budget_per_order_uses_n() -> None:
    config = DaytradeConfig.model_validate({"capital": {"max_capital": 2_000_000}})
    assert config.capital.positions == 3
    assert config.capital.budget_per_order == Decimal(666_666)


# --------------------------------------------------------------------------
# 母集団
# --------------------------------------------------------------------------


def test_segment_expr_maps_old_and_new_names() -> None:
    frame = pl.DataFrame(
        {
            "MktNm": [
                "プライム",
                "東証一部",
                "スタンダード",
                "東証二部",
                "JASDAQ スタンダード",
                "グロース",
                "マザーズ",
                "JASDAQ グロース",
                "その他",
                None,
            ]
        }
    ).with_columns(seg=segment_expr())
    assert frame["seg"].to_list() == [
        "prime",
        "prime",
        "standard",
        "standard",
        "standard",
        "growth",
        "growth",
        "growth",
        "other",
        "other",
    ]


def test_to_broker_symbol() -> None:
    assert to_broker_symbol("72030") == "7203"
    assert to_broker_symbol("130A0") == "130A"
    assert to_broker_symbol("7203") == "7203"


def _bars(codes: dict[str, tuple[float, float, float]], days: int = 25) -> pl.DataFrame:
    """銘柄 → (終値, 売買代金, 時価総額) を ``days`` 日ぶん並べる。"""
    rows = []
    for i in range(days):
        date = PREV - dt.timedelta(days=days - 1 - i)
        for code, (close, va, cap) in codes.items():
            rows.append(
                {
                    "Date": date,
                    "Code": code,
                    "C": str(close),
                    "Va": str(va),
                    "MktCap": str(cap),
                    "AdjFactor": "1.0",
                }
            )
    return pl.DataFrame(rows)


def _master(codes: dict[str, str]) -> pl.DataFrame:
    return pl.DataFrame(
        {
            "Date": [PREV] * len(codes),
            "Code": list(codes),
            "CoName": [f"会社{c}" for c in codes],
            "MktNm": list(codes.values()),
            "ProdCat": ["011"] * len(codes),
        }
    )


def _empty() -> pl.DataFrame:
    return pl.DataFrame({"Code": pl.Series([], dtype=pl.String)})


def test_candidates_apply_every_exclusion() -> None:
    """流動性・区分・時価総額分位・決算・日々公表の各条件が効くこと。"""
    codes = {
        "10000": (1000, 5e8, 9000),  # プライム、流動性あり、大型 → 対象
        "20000": (1000, 5e8, 5000),  # プライム、中型 → 対象
        "30000": (1000, 5e8, 1000),  # プライム、小型（下位 1/3）→ 除外
        "40000": (1000, 1e7, 9000),  # プライム、流動性不足 → 除外
        "50000": (1000, 5e8, 9000),  # グロース → 除外
        "60000": (1000, 5e8, 9000),  # 前日引け後に決算 → 除外
        "70000": (1000, 5e8, 9000),  # 当日決算予定 → 除外
        "80000": (1000, 5e8, 9000),  # 日々公表 → 除外
        "90000": (1000, 5e8, 500),  # プライム、最小型 → 除外
    }
    segs = {c: "プライム" for c in codes}
    segs["50000"] = "グロース"
    fins = pl.DataFrame(
        {"DiscDate": [PREV, PREV], "DiscTime": ["15:30:00", "10:00:00"], "Code": ["60000", "10000"]}
    )
    sched = pl.DataFrame({"Code": ["70000"], "SchDate": [DAY]})
    alert = pl.DataFrame({"Code": ["80000"], "PubDate": [PREV]})
    frame = candidates(
        Inputs(_bars(codes), _master(segs), fins, sched, alert), DAY, PREV, UniverseConfig()
    )
    eligible = set(frame.filter(pl.col("eligible"))["Code"].to_list())
    assert eligible == {"10000", "20000"}
    row = frame.filter(pl.col("Code") == "10000").row(0, named=True)
    assert row["symbol"] == "1000"
    assert row["prev_close"] == 1000.0
    # 場中（10:00）の開示は「前日引け後」に数えない
    assert not row["earn_prev"]


def test_candidates_require_fresh_bars() -> None:
    codes = {"10000": (1000, 5e8, 9000)}
    stale = _bars(codes).filter(pl.col("Date") < PREV)
    with pytest.raises(ValueError, match="足がありません"):
        candidates(
            Inputs(stale, _master({"10000": "プライム"}), _empty(), _empty(), _empty()),
            DAY,
            PREV,
            UniverseConfig(),
        )


# --------------------------------------------------------------------------
# 選定とサイジング
# --------------------------------------------------------------------------


def _cands(rows: list[tuple[str, float]]) -> pl.DataFrame:
    return pl.DataFrame(
        {
            "Code": [f"{s}0" for s, _ in rows],
            "symbol": [s for s, _ in rows],
            "name": [f"名{s}" for s, _ in rows],
            "prev_close": [p for _, p in rows],
            "eligible": [True] * len(rows),
        }
    )


def _q(symbol: str, price: float) -> Quote:
    return Quote(
        symbol=symbol, price=Decimal(str(price)), at=dt.datetime(2026, 9, 1, 0, 0, tzinfo=UTC)
    )


def test_shares_for_rounds_down_to_lot() -> None:
    assert shares_for(Decimal(666_666), Decimal(3000)) == Decimal(200)
    assert shares_for(Decimal(666_666), Decimal(7000)) == Decimal(0)
    assert shares_for(Decimal(666_666), Decimal(3000), Decimal(1)) == Decimal(222)


def test_pick_orders_by_gap_and_skips_unaffordable() -> None:
    cands = _cands([("A", 1000), ("B", 1000), ("C", 1000), ("D", 8000), ("E", 1000)])
    quotes = {
        "A": _q("A", 990),  # -1.0%
        "B": _q("B", 950),  # -5.0%
        "C": _q("C", 1010),  # +1.0% → 対象外
        "D": _q("D", 7000),  # -12.5% だが 1 単元 70 万 > 予算 → 飛ばす
        "E": _q("E", 970),  # -3.0%
    }
    picks = pick(cands, quotes, n=2, budget=Decimal(666_666), config=SignalConfig())
    assert [p.symbol for p in picks] == ["B", "E"]
    assert picks[0].rank == 2  # D が 1 位だが買えないので順位は残す
    assert picks[0].quantity == Decimal(700)
    assert picks[0].gap == Decimal("-0.0500")
    assert picks[0].fee == Decimal(275)


def test_pick_respects_gap_bounds_and_missing_quotes() -> None:
    cands = _cands([("A", 1000), ("B", 1000)])
    quotes = {"A": _q("A", 850)}  # B は気配なし
    config = SignalConfig(min_gap=Decimal("-0.10"))
    assert pick(cands, quotes, n=3, budget=Decimal(500_000), config=config) == []
    config = SignalConfig(min_gap=Decimal("-0.20"))
    assert [p.symbol for p in pick(cands, quotes, n=3, budget=Decimal(500_000), config=config)] == [
        "A"
    ]


# --------------------------------------------------------------------------
# 気配
# --------------------------------------------------------------------------


def test_parse_snapshot_prefers_price_then_open_then_mid() -> None:
    payload = [
        {"symbol": "7203", "price": "3000", "open": "2990", "last_trade_time": 1756684800000},
        {"symbol": "9984", "open": "5000"},
        {"symbol": "6758", "bid": "4000", "ask": "4010"},
        {"symbol": "0000"},
    ]
    quotes = {q.symbol: q for q in parse_snapshot(payload)}
    assert quotes["7203"].price == Decimal(3000)
    assert quotes["7203"].at == dt.datetime(2025, 9, 1, 0, 0, tzinfo=UTC)
    assert quotes["9984"].price == Decimal(5000)
    assert quotes["6758"].price == Decimal(4005)
    assert "0000" not in quotes


def test_csv_quotes(tmp_path: Path) -> None:
    path = tmp_path / "q.csv"
    path.write_text(
        "symbol,price,at\n7203,3000,2026-09-01T00:00:00+00:00\n9984,abc,\n", encoding="utf-8"
    )
    quotes = CsvQuotes(path).fetch(["7203", "9984", "6758"])
    assert set(quotes) == {"7203"}
    assert quotes["7203"].at == dt.datetime(2026, 9, 1, 0, 0, tzinfo=UTC)


# --------------------------------------------------------------------------
# 台帳
# --------------------------------------------------------------------------


def _request(symbol: str = "7203", side: Side = Side.BUY, cid: str = "a" * 32) -> OrderRequest:
    return OrderRequest(
        client_order_id=cid,
        symbol=symbol,
        side=side,
        order_type=OrderType.MARKET,
        quantity=Decimal(100),
        reason="t",
    )


def test_ledger_remembers_orders_across_instances(tmp_path: Path) -> None:
    path = tmp_path / "ledger.db"
    with Ledger(path) as ledger:
        ledger.record(_request(), DAY, DRY_RUN_STATUS, price=Decimal(3000))
        assert not ledger.was_placed("a" * 32)
        ledger.record(_request(), DAY, OrderStatus.PENDING.value, price=Decimal(3000))
        ledger.update_status(
            "a" * 32,
            OrderStatus.FILLED,
            filled_quantity=Decimal(100),
            avg_fill_price=Decimal(2990),
            broker_order_id="B1",
        )
    with Ledger(path) as ledger:
        assert ledger.was_placed("a" * 32)
        buys = ledger.orders_on(DAY, Side.BUY)
        assert len(buys) == 1
        assert buys[0].filled_quantity == Decimal(100)
        assert buys[0].avg_fill_price == Decimal(2990)
        assert buys[0].broker_order_id == "B1"
        assert not buys[0].is_open
        assert ledger.orders_on(DAY, Side.SELL) == []
        assert ledger.orders_on(DAY - dt.timedelta(days=1)) == []


def test_ledger_open_orders_exclude_dry_run_and_terminal(tmp_path: Path) -> None:
    with Ledger(tmp_path / "l.db") as ledger:
        ledger.record(_request(cid="1" * 32), DAY, DRY_RUN_STATUS)
        ledger.record(_request(cid="2" * 32), DAY, OrderStatus.SUBMITTED.value)
        ledger.record(_request(cid="3" * 32), DAY, OrderStatus.REJECTED.value)
        assert [o.client_order_id for o in ledger.open_orders()] == ["2" * 32]
        assert ledger.orders_on(DAY)[2].is_dead
