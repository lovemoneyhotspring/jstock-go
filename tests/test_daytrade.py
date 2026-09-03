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
from daytrade.quotes import CsvQuotes
from daytrade.select import Quote, pick, shares_for
from daytrade.universe import Inputs, candidates, segment_expr, to_broker_symbol
from wbcore.domain.models import OrderRequest, OrderStatus, OrderType, Side, TradeType

UTC = dt.UTC
DAY = dt.date(2026, 9, 1)
PREV = dt.date(2026, 8, 31)

# --------------------------------------------------------------------------
# 手数料と N
# --------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("day_total", "fee"),
    [
        (0, 0),
        (120_000, 0),
        (120_001, 176),
        (200_000, 176),
        (500_000, 253),
        (1_000_000, 506),
        (2_000_000, 759),
        (10_000_000, 2_783),
        (12_000_000, 2_783 + 253 * 2),
    ],
)
def test_commission_is_the_flat_rate_on_the_day_total(day_total: int, fee: int) -> None:
    """立花証券の定額コース。1 日の現物約定代金合計で段階が決まる。"""
    import polars as pl

    from daytrade.backtest import _day_fee_expr

    assert commission(Decimal(day_total)) == Decimal(fee)
    # polars 版（バックテスト）も同じ表を見ていること
    got = pl.DataFrame({"t": [float(day_total)]}).select(_day_fee_expr(pl.col("t"))).item()
    assert got == float(fee)


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


def test_capital_zero_means_watch_only() -> None:
    """資金 0 は「スクリーニングはするが買わない」: N = 0、予算 0。負は不正。"""
    config = DaytradeConfig.model_validate({"capital": {"max_capital": 0}})
    assert config.capital.enabled
    assert config.capital.positions == 0
    assert config.capital.budget_per_order == Decimal(0)
    with pytest.raises(ValueError):
        DaytradeConfig.model_validate({"capital": {"max_capital": -1}})
    assert not DaytradeConfig.model_validate({"capital": {"enabled": False}}).capital.enabled


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
    assert picks[0].fee == Decimal("379.5")  # 665,000 円を往復した日: 定額 759 円の片道分


def test_pick_skips_limit_down_at_open() -> None:
    """気配がストップ安（前日終値 1,000 円 → 制限値幅 300 円 → 700 円）なら買わない。"""
    cands = _cands([("A", 1000), ("B", 1000)])
    quotes = {"A": _q("A", 700), "B": _q("B", 900)}
    picks = pick(cands, quotes, n=3, budget=Decimal(500_000), config=SignalConfig())
    assert [p.symbol for p in picks] == ["B"]
    picks = pick(
        cands, quotes, n=3, budget=Decimal(500_000), config=SignalConfig(skip_limit_down=False)
    )
    assert [p.symbol for p in picks] == ["A", "B"]


def test_backtest_limit_width_matches_rules() -> None:
    from daytrade.backtest import limit_width_expr
    from wbcore.domain.jp_rules import price_limit_width

    bases = [
        50.0,
        99.0,
        100.0,
        999.0,
        1000.0,
        1500.0,
        3000.0,
        4999.0,
        5000.0,
        30_000.0,
        500_000.0,
        20_000_000.0,
    ]
    frame = pl.DataFrame({"base": bases}).with_columns(width=limit_width_expr(pl.col("base")))
    assert frame["width"].to_list() == [float(price_limit_width(Decimal(str(b)))) for b in bases]


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


def test_ledger_dead_orders_can_be_resent(tmp_path: Path) -> None:
    """拒否された売りは「発注済み」に数えない。close は種を変えて送り直す。"""
    with Ledger(tmp_path / "l.db") as ledger:
        ledger.record(_request("7203", Side.SELL, "r" * 32), DAY, OrderStatus.REJECTED.value)
        assert not ledger.was_placed("r" * 32)
        assert ledger.dead_count(DAY, "7203", Side.SELL) == 1
        assert ledger.dead_count(DAY, "7203", Side.BUY) == 0
        ledger.record(_request("7203", Side.SELL, "s" * 32), DAY, OrderStatus.SUBMITTED.value)
        assert ledger.was_placed("s" * 32)


def test_ledger_open_orders_exclude_dry_run_and_terminal(tmp_path: Path) -> None:
    with Ledger(tmp_path / "l.db") as ledger:
        ledger.record(_request(cid="1" * 32), DAY, DRY_RUN_STATUS)
        ledger.record(_request(cid="2" * 32), DAY, OrderStatus.SUBMITTED.value)
        ledger.record(_request(cid="3" * 32), DAY, OrderStatus.REJECTED.value)
        assert [o.client_order_id for o in ledger.open_orders()] == ["2" * 32]
        assert ledger.orders_on(DAY)[2].is_dead


# --------------------------------------------------------------------------
# 危険信号
# --------------------------------------------------------------------------


def test_regime_default_trades_everything() -> None:
    from daytrade.config import RegimeConfig
    from daytrade.regime import Signals, evaluate

    verdict = evaluate(
        RegimeConfig(),
        Signals(day=dt.date(2026, 12, 1), drift=-0.001, iv_prev=10.0, recent_pnl=-1.0),
    )
    assert verdict.trade and verdict.reasons == []
    assert verdict.notes["drift_bp"] == -10.0


def test_regime_gates_are_independent() -> None:
    from daytrade.config import RegimeConfig
    from daytrade.regime import Signals, evaluate

    config = RegimeConfig(
        skip_months=[12],
        iv_gate=Decimal(18),
        drift_gate=Decimal("-0.0003"),
        equity_curve_days=20,
        equity_curve_scale=Decimal(0),  # 0 なら「休む」= 理由に数える
    )
    s = Signals(
        day=dt.date(2026, 12, 1), iv_prev=15.0, drift=-0.0005, market_gap=0.0, recent_pnl=-100.0
    )
    verdict = evaluate(config, s)
    assert not verdict.trade
    assert len(verdict.reasons) == 4
    # 信号が無ければそのゲートは効かない。市場ギャップが大きければドリフトも効かない
    s = Signals(
        day=dt.date(2026, 11, 1), iv_prev=None, drift=-0.0005, market_gap=-0.02, recent_pnl=None
    )
    assert evaluate(config, s).trade


def test_regime_config_validates_months() -> None:
    from daytrade.config import RegimeConfig

    assert RegimeConfig(skip_months=[12, 1, 12]).skip_months == [1, 12]
    with pytest.raises(ValueError):
        RegimeConfig(skip_months=[13])


def test_realized_pnl_pairs_buys_and_sells(tmp_path: Path) -> None:
    from daytrade.ledger import realized_pnl

    with Ledger(tmp_path / "l.db") as ledger:
        ledger.record(_request("7203", Side.BUY, "b" * 32), DAY, OrderStatus.SUBMITTED.value)
        ledger.update_status(
            "b" * 32, OrderStatus.FILLED, filled_quantity=Decimal(100), avg_fill_price=Decimal(3000)
        )
        ledger.record(_request("7203", Side.SELL, "s" * 32), DAY, OrderStatus.SUBMITTED.value)
        ledger.update_status(
            "s" * 32, OrderStatus.FILLED, filled_quantity=Decimal(100), avg_fill_price=Decimal(3050)
        )
        ledger.record(_request("9984", Side.BUY, "d" * 32), DAY, DRY_RUN_STATUS)
        assert realized_pnl(ledger, [DAY, DAY - dt.timedelta(days=1)]) == {
            DAY: 5000.0,
            DAY - dt.timedelta(days=1): 0.0,
        }


def test_backtest_regime_zeroes_gated_days() -> None:
    """止めた日は損益 0、取引一覧からも消える。"""
    from daytrade.backtest import simulate
    from daytrade.config import DaytradeConfig

    days = [dt.date(2026, 11, 30), dt.date(2026, 12, 1)]
    rows = []
    for day in days:
        for code, o, c in (("10000", 1000.0, 1010.0), ("20000", 1000.0, 990.0)):
            rows.append(
                {
                    "Date": day,
                    "Code": code,
                    "O": o,
                    "C": c,
                    "prev_close": 1050.0,
                    "eligible": True,
                    "gap": o / 1050 - 1,
                }
            )
    panel = pl.DataFrame(rows)
    config = DaytradeConfig.model_validate(
        {"capital": {"max_capital": 2_000_000}, "regime": {"skip_months": [12]}}
    )
    result = simulate(panel, config)
    by_day = dict(result.daily.select("Date", "pnl").iter_rows())
    assert by_day[dt.date(2026, 12, 1)] == 0.0
    assert by_day[dt.date(2026, 11, 30)] != 0.0
    assert result.trades["Date"].unique().to_list() == [dt.date(2026, 11, 30)]


def test_regime_us_skip_band_and_vix_override() -> None:
    from daytrade.config import RegimeConfig
    from daytrade.regime import Signals, evaluate

    config = RegimeConfig(us_skip_high=Decimal("0.01"))
    day = dt.date(2026, 9, 1)
    assert not evaluate(config, Signals(day=day, us_ret=0.004, vix=15.0)).trade
    assert evaluate(config, Signals(day=day, us_ret=-0.004, vix=15.0)).trade
    assert evaluate(config, Signals(day=day, us_ret=0.012, vix=15.0)).trade
    assert evaluate(config, Signals(day=day, us_ret=0.004, vix=30.0)).trade  # 高 VIX は例外
    assert evaluate(config, Signals(day=day, us_ret=None, vix=None)).trade  # 取れなければ効かない
    with pytest.raises(ValueError):
        RegimeConfig(us_skip_low=Decimal("0.01"), us_skip_high=Decimal("0.005"))


def test_usmarket_sessions_and_asof() -> None:
    from daytrade.usmarket import as_of_frame, sessions_from

    closes = pl.DataFrame(
        {
            "Date": [dt.date(2026, 8, 27), dt.date(2026, 8, 28), dt.date(2026, 8, 31)],
            "spx": [100.0, 101.0, 99.99],
            "vix": [15.0, 16.0, 20.0],
        }
    )
    sessions = sessions_from(closes)
    assert sessions["Date"].to_list() == [dt.date(2026, 8, 28), dt.date(2026, 8, 31)]
    assert abs(sessions["spx_ret"][0] - 0.01) < 1e-9
    # 東証 9/1 の朝には NY 8/31 のセッションが、9/1（月曜の NY 休場後の火曜）にも 8/31 が当たる
    days = pl.DataFrame({"Date": [dt.date(2026, 8, 31), dt.date(2026, 9, 1), dt.date(2026, 9, 2)]})
    joined = as_of_frame(sessions, days)
    assert joined["spx_ret"].to_list()[0] == pytest.approx(0.01)  # 8/31 の朝 → NY 8/28
    assert joined["vix"].to_list()[1] == 20.0  # 9/1 の朝 → NY 8/31
    assert joined["vix"].to_list()[2] == 20.0


# --------------------------------------------------------------------------
# CLI: open（csv の気配・一時的な state で発注経路を通す）
# --------------------------------------------------------------------------


def _cli_env(tmp_path: Path, max_capital: int) -> tuple[Path, dict[str, str]]:
    """設定ディレクトリ・plan・気配 CSV を tmp に作り、環境変数で state/data を隔離する。"""
    import json

    cfg = tmp_path / "cfg"
    cfg.mkdir()
    (cfg / "daytrade.toml").write_text(
        f"[capital]\nmax_capital = {max_capital}\n[regime]\nskip_months = []\n", encoding="utf-8"
    )
    state = tmp_path / "state"
    plans = state / "daytrade"
    plans.mkdir(parents=True)
    frame = _cands([("A", 1000), ("B", 1000), ("C", 1000), ("D", 1000)]).with_columns(
        segment=pl.lit("prime"),
        prev_close=pl.col("prev_close").cast(pl.Float64),
        turnover_med=pl.lit(5e8),
        mkt_cap=pl.lit(9000.0),
        cap_tercile=pl.lit(3, dtype=pl.Int32),
        earn_prev=pl.lit(False),
        disc_today=pl.lit(False),
        alert=pl.lit(False),
    )
    frame.write_parquet(plans / f"plan-{DAY}.parquet")
    meta = {
        "day": DAY.isoformat(),
        "prev_day": PREV.isoformat(),
        "positions": 3,
        "budget_per_order": "666666",
        "iv_prev": None,
        "iv_gate": "0",
        "drift": None,
        "candidates": 4,
        "eligible": 4,
        "created_at": "2026-08-31T12:00:00+00:00",
    }
    (plans / f"plan-{DAY}.json").write_text(json.dumps(meta), encoding="utf-8")
    (tmp_path / "q.csv").write_text("symbol,price\nA,950\nB,970\nC,990\nD,1010\n", encoding="utf-8")
    # WBJP_LOG_DIR も tmp に向ける。.env が絶対パスで指していると、ログが本物の state/logs に
    # 漏れて（テストが JSONL を読めず）落ちる
    env = {
        "WBJP_STATE_DIR": str(state),
        "WBJP_DATA_DIR": str(tmp_path / "data"),
        "WBJP_LOG_DIR": str(state / "logs"),
        "WBJP_ENV": "uat",
    }
    return cfg, env


def _open(tmp_path: Path, max_capital: int) -> tuple[int, str, list[dict[str, object]]]:
    import json

    from typer.testing import CliRunner

    from daytrade.cli import app

    cfg, env = _cli_env(tmp_path, max_capital)
    result = CliRunner().invoke(
        app,
        [
            "open",
            "--date",
            DAY.isoformat(),
            "--quote-source",
            "csv",
            "--quote-file",
            str(tmp_path / "q.csv"),
            "--config-dir",
            str(cfg),
        ],
        env=env,
    )
    log_path = Path(env["WBJP_STATE_DIR"]) / "logs" / "daytrade-uat.jsonl"
    records = (
        [json.loads(line) for line in log_path.read_text(encoding="utf-8").splitlines()]
        if log_path.is_file()
        else []
    )
    return result.exit_code, result.stdout, records


def test_open_dry_run_records_orders_and_logs_context(tmp_path: Path) -> None:
    code, out, records = _open(tmp_path, 2_000_000)
    assert code == 0, out
    with Ledger(tmp_path / "state" / "daytrade-uat.db") as ledger:
        buys = ledger.orders_on(DAY, Side.BUY)
    assert [o.symbol for o in buys] == ["A", "B", "C"] and all(o.is_dry_run for o in buys)
    codes = [r.get("code") for r in records]
    for expected in (
        "daytrade.config",
        "daytrade.quotes",
        "daytrade.regime",
        "daytrade.ranking",
        "daytrade.pick",
        "daytrade.order",
        "daytrade.run",
    ):
        assert expected in codes, expected
    ranking = next(r for r in records if r.get("code") == "daytrade.ranking")
    assert [row["symbol"] for row in ranking["rows"]] == ["A", "B", "C"]  # D はギャップ正で対象外
    config = next(r for r in records if r.get("code") == "daytrade.config")
    assert config["positions"] == 3 and config["phase"] == "open"


def test_open_with_zero_capital_screens_but_never_orders(tmp_path: Path) -> None:
    """資金 0: 候補は出すが、台帳に注文（dry-run 含む）を書かない。"""
    code, out, records = _open(tmp_path, 0)
    assert code == 0, out
    assert "買わない" in out
    with Ledger(tmp_path / "state" / "daytrade-uat.db") as ledger:
        assert ledger.orders_on(DAY) == []
    assert not any(r.get("code") == "daytrade.order" for r in records)
    skip = [r for r in records if r.get("code") == "daytrade.skip"]
    assert skip and skip[-1]["reason"] == "no_capital"


# --------------------------------------------------------------------------
# D + C: ボラ逆比例の配分と資産曲線での縮小
# --------------------------------------------------------------------------


def test_pick_inverse_vol_gives_more_to_calm_stock() -> None:
    from daytrade.select import rank, weights

    cands = _cands([("A", 1000), ("B", 1000), ("C", 1000)]).with_columns(
        vol20=pl.Series([0.04, 0.02, None])  # C はボラ不明 → 下限 2% 扱い
    )
    quotes = {"A": _q("A", 950), "B": _q("B", 960), "C": _q("C", 970)}
    ranked = rank(cands, quotes, SignalConfig())
    assert [r.symbol for r in ranked] == ["A", "B", "C"]
    w = weights(ranked, "inverse_vol")
    assert w[1] == w[2] and w[0] < w[1] and abs(sum(w) - 1) < Decimal("0.0001")
    picks = pick(
        cands, quotes, n=3, budget=Decimal(666_666), config=SignalConfig(), weighting="inverse_vol"
    )
    q = {p.symbol: p.quantity for p in picks}
    # 総予算 200 万を 1:2:2 で按分 → A 40 万（400 株）、B/C 80 万（800 株）
    assert q == {"A": Decimal(400), "B": Decimal(800), "C": Decimal(800)}
    equal = {
        p.symbol: p.quantity
        for p in pick(cands, quotes, n=3, budget=Decimal(666_666), config=SignalConfig())
    }
    assert equal == {"A": Decimal(700), "B": Decimal(600), "C": Decimal(600)}


def test_regime_equity_curve_scales_instead_of_skipping() -> None:
    from daytrade.config import RegimeConfig
    from daytrade.regime import Signals, evaluate

    day = dt.date(2026, 9, 1)
    v = evaluate(
        RegimeConfig(equity_curve_days=20, equity_curve_scale=Decimal("0.5")),
        Signals(day=day, recent_pnl=-1.0),
    )
    assert v.trade and v.scale == 0.5 and "縮小" in v.scale_reason
    v = evaluate(
        RegimeConfig(equity_curve_days=20, equity_curve_scale=Decimal(0)),
        Signals(day=day, recent_pnl=-1.0),
    )
    assert not v.trade
    v = evaluate(RegimeConfig(equity_curve_days=20), Signals(day=day, recent_pnl=1.0))
    assert v.trade and v.scale == 1.0


def test_backtest_scale_halves_pnl_after_losses() -> None:
    from daytrade.backtest import simulate
    from daytrade.config import DaytradeConfig

    days = [dt.date(2026, 6, 1) + dt.timedelta(days=i) for i in range(6)]
    rows = []
    for i, day in enumerate(days):
        # 最初の 3 日は負け、その後は勝ち。直近 2 日の損益が 0 以下なら半分
        c = 990.0 if i < 3 else 1010.0
        rows.append(
            {
                "Date": day,
                "Code": "10000",
                "O": 1000.0,
                "C": c,
                "prev_close": 1050.0,
                "eligible": True,
                "gap": -0.0476,
                "vol20": 0.02,
            }
        )
    panel = pl.DataFrame(rows)
    config = DaytradeConfig.model_validate(
        {
            "capital": {"max_capital": 2_000_000, "order_budget": 2_000_000, "weighting": "equal"},
            "regime": {"equity_curve_days": 2, "equity_curve_scale": 0.5},
        }
    )
    result = simulate(panel, config)
    scales = result.daily.sort("Date")["scale"].to_list()
    assert scales[:2] == [1.0, 1.0]  # 履歴が足りない
    assert scales[2] == 0.5 and scales[3] == 0.5  # 負けが続いた後は半分
    assert scales[-1] == 1.0  # 勝ちが続けば戻る


# --------------------------------------------------------------------------
# jp_gap_fade_margin（信用売り）
# --------------------------------------------------------------------------


def _margin_panel(days: list[dt.date]) -> pl.DataFrame:
    """ロング候補（ギャップダウン）1 つとショート候補（ギャップアップ）2 つ。
    ショート候補のうち 1 つは貸借銘柄でない。"""
    rows = []
    for day in days:
        rows += [
            # ロング: −5% で寄って +1% 戻す
            {
                "Date": day,
                "Code": "10000",
                "O": 1000.0,
                "C": 1010.0,
                "prev_close": 1052.6,
                "gap": 1000 / 1052.6 - 1,
                "eligible": True,
                "shortable": True,
                "vol20": 0.02,
            },
            # ショート: +5% で寄って引けに 990 → 売り方の利益
            {
                "Date": day,
                "Code": "20000",
                "O": 1000.0,
                "C": 990.0,
                "prev_close": 952.4,
                "gap": 1000 / 952.4 - 1,
                "eligible": True,
                "shortable": True,
                "vol20": 0.02,
            },
            # 同じ形だが貸借銘柄でない → 売れない
            {
                "Date": day,
                "Code": "30000",
                "O": 1000.0,
                "C": 900.0,
                "prev_close": 952.4,
                "gap": 1000 / 952.4 - 1,
                "eligible": True,
                "shortable": False,
                "vol20": 0.02,
            },
        ]
    return pl.DataFrame(rows).with_columns(
        short_eligible=pl.col("eligible") & pl.col("shortable"),
        next_open=pl.lit(None, dtype=pl.Float64),
    )


def _margin_config(**margin: object):
    from daytrade.config import DaytradeConfig

    return DaytradeConfig.model_validate(
        {
            "capital": {"max_capital": 1_000_000, "order_budget": 1_000_000, "weighting": "equal"},
            "margin": {
                "enabled": True,
                "max_capital": 1_000_000,
                "order_budget": 1_000_000,
                "weighting": "equal",
                "min_gap": 0.03,
                "extra_cost_bp": 0,
                "long_via_margin": True,
                "long_extra_cost_bp": 0,
                **margin,
            },
        }
    )


def test_margin_short_pnl_is_open_minus_close_and_only_shortable() -> None:
    from daytrade.backtest import simulate_margin

    result = simulate_margin(_margin_panel([dt.date(2026, 6, 1)]), _margin_config())
    short = result.short_trades
    assert short["Code"].to_list() == ["20000"]  # 貸借銘柄でない 30000 は入らない
    assert short["gross"][0] == 1000 * (1000.0 - 990.0)  # 1,000 株 × (O − C)
    long = result.long_trades
    assert long["Code"].to_list() == ["10000"]
    assert long["fees"][0] == 0.0  # 信用買いは手数料 0 円
    day = result.daily.row(0, named=True)
    assert day["pnl"] == day["long_pnl"] + day["short_pnl"]


def test_margin_seesaw_follows_long_equity_curve() -> None:
    """ロング側が資産曲線で縮小された日だけショートを建てる（通常 0 / 弱 1.5）。"""
    from daytrade.backtest import simulate_margin
    from daytrade.config import DaytradeConfig

    days = [dt.date(2026, 6, 1) + dt.timedelta(days=i) for i in range(5)]
    panel = _margin_panel(days)
    # ロングを最初の 3 日負けさせる（C < O）。ショート側の行はそのまま
    panel = panel.with_columns(
        C=pl.when((pl.col("Code") == "10000") & (pl.col("Date") <= days[2]))
        .then(990.0)
        .otherwise(pl.col("C"))
    )
    config = DaytradeConfig.model_validate(
        {
            **_margin_config(multiplier_normal=0.0, multiplier_long_weak=1.5).model_dump(),
            "regime": {"equity_curve_days": 2, "equity_curve_scale": 0.5},
        }
    )
    result = simulate_margin(panel, config)
    daily = result.daily.sort("Date")
    assert daily["long_scale"].to_list()[:4] == [1.0, 1.0, 0.5, 0.5]
    assert daily["short_multiplier"].to_list()[:4] == [0.0, 0.0, 1.5, 1.5]
    # 通常日はショートの損益 0、縮小日は 1.5 倍
    assert daily["short_pnl"][0] == 0.0
    assert daily["short_pnl"][2] == 1.5 * 1000 * (1000.0 - 990.0)
    assert daily["short_n"][0] == 0 and daily["short_n"][2] == 1


def test_margin_config_defaults_off_and_validates() -> None:
    from daytrade.config import DaytradeConfig, MarginConfig

    assert not MarginConfig().enabled and MarginConfig().positions == 0
    # jp_gap_up_short の既定: プライム・ギャップ +5%・常時・売り禁だけ除外
    assert MarginConfig().segments == ["prime"] and MarginConfig().min_gap == Decimal("0.05")
    assert MarginConfig().multiplier_normal == 1 and MarginConfig().exclude_jsf_stop
    with pytest.raises(ValueError):
        DaytradeConfig.model_validate({"margin": {"multiplier_long_weak": -1}})
    with pytest.raises(ValueError):
        DaytradeConfig.model_validate({"margin": {"min_gap": 2}})
    with pytest.raises(ValueError):
        DaytradeConfig.model_validate({"margin": {"segments": ["tokyo"]}})
    with pytest.raises(ValueError):
        DaytradeConfig.model_validate({"margin": {"carry_penalty": 1.5}})


def test_margin_carry_penalty_closes_at_next_open_when_limit_up() -> None:
    """引けがストップ高の売建は翌寄りで返済したことにする（係数で按分）。"""
    from daytrade.backtest import simulate_margin

    day = dt.date(2026, 6, 1)
    # 前日終値 952.4 → 値幅 ±150 → ストップ高 1102.4。寄付 1000 で売り、引け 1102.4（張り付き）、翌寄り 1150
    panel = _margin_panel([day]).with_columns(
        C=pl.when(pl.col("Code") == "20000").then(1102.4).otherwise(pl.col("C")),
        next_open=pl.when(pl.col("Code") == "20000").then(1150.0).otherwise(None),
    )
    full = simulate_margin(panel, _margin_config(carry_penalty=1.0)).short_trades.row(0, named=True)
    assert full["carried"]
    assert full["gross"] == pytest.approx(1000 * (1000.0 - 1150.0))  # 翌寄りで返済
    half = simulate_margin(panel, _margin_config(carry_penalty=0.5)).short_trades.row(0, named=True)
    assert half["gross"] == pytest.approx(1000 * ((1000.0 - 1102.4) + 0.5 * (1102.4 - 1150.0)))
    none = simulate_margin(panel, _margin_config(carry_penalty=0)).short_trades.row(0, named=True)
    assert none["gross"] == pytest.approx(1000 * (1000.0 - 1102.4)) and none["carried"]
    # 引けがストップ高でなければ翌寄りは無関係
    normal = simulate_margin(_margin_panel([day]), _margin_config()).short_trades.row(0, named=True)
    assert not normal["carried"]


def test_margin_short_universe_is_separate_from_long() -> None:
    """ショートの母集団は [margin] の条件（区分・売り禁）で決まり、ロングの eligible とは独立。"""
    from daytrade.config import MarginConfig, UniverseConfig
    from daytrade.universe import candidates

    codes = {
        # 時価総額は同順位を避けて全て違う値にする（3 分位: 500/5000 が下位、6000/7000 が中位、8000/9000 が上位）
        "10000": (1000, 5e8, 9000),  # プライム・貸借・大型 → 長短とも対象
        "20000": (1000, 5e8, 500),  # プライム・貸借・最小型 → ロングは分位で除外、ショートは対象
        "30000": (
            1000,
            5e8,
            8000,
        ),  # スタンダード・貸借 → 区分で両方除外（margin.segments = prime）
        "40000": (
            1000,
            5e8,
            7000,
        ),  # プライム・貸借・売り禁 → ロングは規制で、ショートは売り禁で除外
        "50000": (1000, 5e8, 6000),  # プライム・貸借・注意喚起 → ロングは規制で除外、ショートは対象
        "60000": (1000, 5e8, 5000),  # プライム・信用銘柄（貸借でない）・小型 → 両方除外
    }
    segs = {c: "プライム" for c in codes}
    segs["30000"] = "スタンダード"
    master = _master(segs).with_columns(Mrgn=pl.Series(["2", "2", "2", "2", "2", "1"]))
    alert = pl.DataFrame(
        {
            "Code": ["40000", "50000"],
            "PubDate": [PREV, PREV],
            "PubReason": [
                '{"DailyPublication": "0", "PrecautionByJSF": "0", "RestrictedByJSF": "1"}',
                '{"DailyPublication": "0", "PrecautionByJSF": "1", "RestrictedByJSF": "0"}',
            ],
        }
    )
    frame = candidates(
        Inputs(_bars(codes), master, _empty(), _empty(), alert),
        DAY,
        PREV,
        UniverseConfig(),
        MarginConfig(enabled=True, max_capital=1_000_000),
    )
    assert set(frame.filter(pl.col("eligible"))["Code"]) == {"10000"}
    assert set(frame.filter(pl.col("short_eligible"))["Code"]) == {"10000", "20000", "50000"}
    assert set(frame.filter(pl.col("jsf_stop"))["Code"]) == {"40000"}
    # margin を渡さない／無効なら short_eligible は全て偽
    off = candidates(
        Inputs(_bars(codes), master, _empty(), _empty(), alert), DAY, PREV, UniverseConfig()
    )
    assert not off["short_eligible"].any()


def test_margin_long_shrink_off_keeps_long_full_but_triggers_short() -> None:
    """long_shrink=false: 合図の日もロングは 1.0 のまま、ショートだけ建つ。"""
    from daytrade.backtest import simulate_margin
    from daytrade.config import DaytradeConfig

    days = [dt.date(2026, 6, 1) + dt.timedelta(days=i) for i in range(5)]
    panel = _margin_panel(days).with_columns(
        C=pl.when((pl.col("Code") == "10000") & (pl.col("Date") <= days[2]))
        .then(990.0)
        .otherwise(pl.col("C"))
    )
    config = DaytradeConfig.model_validate(
        {
            **_margin_config(
                multiplier_normal=0.0, multiplier_long_weak=1.0, long_shrink=False
            ).model_dump(),
            "regime": {"equity_curve_days": 2, "equity_curve_scale": 0.5},
        }
    )
    daily = simulate_margin(panel, config).daily.sort("Date")
    assert daily["long_scale"].to_list()[:4] == [1.0, 1.0, 1.0, 1.0]
    assert daily["short_multiplier"].to_list()[:4] == [0.0, 0.0, 1.0, 1.0]


# --------------------------------------------------------------------------
# 信用（発注側）: 台帳の脚・ショートの順位・paper の売建
# --------------------------------------------------------------------------


def _req(
    symbol: str, side: Side, trade: TradeType, qty: int = 100, seed: str = "s"
) -> OrderRequest:
    from wbcore.domain.models import OrderType, make_client_order_id

    return OrderRequest(
        client_order_id=make_client_order_id(seed, symbol, side, Decimal(qty)),
        symbol=symbol,
        side=side,
        order_type=OrderType.MARKET,
        quantity=Decimal(qty),
        trade=trade,
    )


def test_ledger_adds_trade_column_to_old_db_and_keeps_legs(tmp_path: Path) -> None:
    import sqlite3

    from daytrade.ledger import Ledger, realized_pnl

    path = tmp_path / "old.db"
    with sqlite3.connect(path) as conn:  # trade 列の無い旧スキーマ
        conn.execute(
            "CREATE TABLE orders (client_order_id TEXT PRIMARY KEY, broker_order_id TEXT, day TEXT NOT NULL,"
            " symbol TEXT NOT NULL, side TEXT NOT NULL, quantity TEXT NOT NULL,"
            " filled_quantity TEXT NOT NULL DEFAULT '0', status TEXT NOT NULL, price TEXT,"
            " avg_fill_price TEXT, reason TEXT, placed_at TEXT NOT NULL, updated_at TEXT)"
        )
        conn.execute(
            "INSERT INTO orders VALUES ('old', NULL, '2026-09-01', '7203', 'BUY', '100', '100',"
            " 'FILLED', NULL, '1000', '', 'x', NULL)"
        )
    day = dt.date(2026, 9, 1)
    with Ledger(path) as ledger:
        old = ledger.orders_on(day)[0]
        assert old.trade is TradeType.CASH and old.is_entry and old.leg == "long"
        # ロング（信用買い→返済売り）とショート（売建→返済買い）を同じ日に
        ledger.record(_req("7203", Side.SELL, TradeType.MARGIN_CLOSE), day, "FILLED")
        ledger.update_status(
            _req("7203", Side.SELL, TradeType.MARGIN_CLOSE).client_order_id,
            OrderStatus.FILLED,
            filled_quantity=Decimal(100),
            avg_fill_price=Decimal(1010),
        )
        s_open = _req("9984", Side.SELL, TradeType.MARGIN_OPEN)
        s_close = _req("9984", Side.BUY, TradeType.MARGIN_CLOSE)
        ledger.record(s_open, day, "FILLED")
        ledger.update_status(
            s_open.client_order_id,
            OrderStatus.FILLED,
            filled_quantity=Decimal(100),
            avg_fill_price=Decimal(5000),
        )
        ledger.record(s_close, day, "FILLED")
        ledger.update_status(
            s_close.client_order_id,
            OrderStatus.FILLED,
            filled_quantity=Decimal(100),
            avg_fill_price=Decimal(4900),
        )
        assert {o.leg for o in ledger.entries_on(day)} == {"long", "short"}
        assert {o.symbol for o in ledger.exits_on(day)} == {"7203", "9984"}
        assert realized_pnl(ledger, [day], leg="long") == {day: 1000.0}  # (1010−1000)×100
        assert realized_pnl(ledger, [day], leg="short") == {day: 10000.0}  # (5000−4900)×100
        assert realized_pnl(ledger, [day]) == {day: 11000.0}

        # 売建てたのに返済の記録が無い日は None（0 と混ぜない）
        day2 = dt.date(2026, 9, 2)
        s2 = _req("9984", Side.SELL, TradeType.MARGIN_OPEN, seed="d2")
        ledger.record(s2, day2, "FILLED")
        ledger.update_status(
            s2.client_order_id,
            OrderStatus.FILLED,
            filled_quantity=Decimal(100),
            avg_fill_price=Decimal(1),
        )
        assert realized_pnl(ledger, [day2], leg="short") == {day2: None}
        assert realized_pnl(ledger, [day2], leg="long") == {day2: 0.0}


def test_rank_short_orders_by_largest_gap_and_skips_limit_up() -> None:
    from daytrade.config import MarginConfig
    from daytrade.select import Quote, pick, rank_short

    candidates = pl.DataFrame(
        {
            "Code": ["10000", "20000", "30000", "40000"],
            "symbol": ["1000", "2000", "3000", "4000"],
            "name": ["a", "b", "c", "d"],
            "prev_close": [1000.0, 1000.0, 1000.0, 1000.0],
            "eligible": [True, True, True, True],
        }
    )
    at = dt.datetime(2026, 9, 2, 0, 0, tzinfo=dt.UTC)
    quotes = {
        "1000": Quote("1000", Decimal(1050), at),  # +5%
        "2000": Quote("2000", Decimal(1080), at),  # +8% → 1 位
        "3000": Quote("3000", Decimal(1020), at),  # +2% → min_gap 未満
        "4000": Quote("4000", Decimal(1300), at),  # +30% はストップ高（1,000 円の値幅は 300）→ 外す
    }
    config = MarginConfig(enabled=True, max_capital=1_000_000, min_gap=Decimal("0.03"))
    ranked = rank_short(candidates, quotes, config)
    assert [r.symbol for r in ranked] == ["2000", "1000"]
    picks = pick(
        candidates,
        quotes,
        n=2,
        budget=Decimal(500_000),
        config=config,
        ranked=ranked,
        side=Side.SELL,
    )
    assert [(p.symbol, p.side, p.quantity) for p in picks] == [
        ("2000", Side.SELL, Decimal(400)),
        ("1000", Side.SELL, Decimal(400)),
    ]


def test_paper_broker_short_open_and_close() -> None:
    from wbcore.broker.paper import PaperBroker

    broker = PaperBroker(
        initial_cash=Decimal(1_000_000), commission_rate=Decimal(0), slippage_rate=Decimal(0)
    )
    broker.mark({"7203": Decimal(2500)})
    open_ = _req("7203", Side.SELL, TradeType.MARGIN_OPEN, seed="o")
    broker.place(open_)
    broker.settle({"7203": Decimal(2500)})
    position = broker.positions_by_symbol()["7203"]
    assert position.quantity == Decimal(-100) and position.trade is TradeType.MARGIN_OPEN
    # 現物の売りとして出すと拒否（保有が無い）
    from wbcore.broker.base import OrderRejectedError

    with pytest.raises(OrderRejectedError):
        broker.place(_req("7203", Side.SELL, TradeType.CASH, seed="x"))
    close = _req("7203", Side.BUY, TradeType.MARGIN_CLOSE, seed="c")
    broker.place(close)
    broker.settle({"7203": Decimal(2400)})
    assert broker.positions_by_symbol() == {}
    assert broker.realized_pnl == Decimal(10000)  # 2500 で売建て 2400 で買戻し


def _cli_env_margin(tmp_path: Path) -> tuple[Path, dict[str, str]]:
    """信用の設定（ロング信用買い + ショート）で open/close の発注経路を通すための環境。"""
    import json

    cfg = tmp_path / "cfg"
    cfg.mkdir()
    (cfg / "daytrade.toml").write_text(
        "[capital]\nmax_capital = 2000000\n"
        "[regime]\nskip_months = []\n"
        "[margin]\nenabled = true\ncash = 2000000\nmax_capital = 1000000\norder_budget = 500000\n"
        "min_gap = 0.03\nmultiplier_normal = 1.0\nmultiplier_long_weak = 1.0\nlong_via_margin = true\n"
        '[execution]\nbroker = "paper"\n',
        encoding="utf-8",
    )
    state = tmp_path / "state"
    plans = state / "daytrade"
    plans.mkdir(parents=True)
    # A〜C はギャップダウン（ロング）、E/F はギャップアップ（ショート候補。F は貸借銘柄でない）
    frame = (
        _cands([("A", 1000), ("B", 1000), ("C", 1000), ("E", 1000), ("F", 1000)])
        .with_columns(
            segment=pl.lit("prime"),
            prev_close=pl.col("prev_close").cast(pl.Float64),
            turnover_med=pl.lit(5e8),
            mkt_cap=pl.lit(9000.0),
            cap_tercile=pl.lit(3, dtype=pl.Int32),
            earn_prev=pl.lit(False),
            disc_today=pl.lit(False),
            alert=pl.lit(False),
            jsf_stop=pl.lit(False),
            shortable=pl.Series([True, True, True, True, False]),
        )
        .with_columns(short_eligible=pl.col("shortable"))
    )
    frame.write_parquet(plans / f"plan-{DAY}.parquet")
    meta = {
        "day": DAY.isoformat(),
        "prev_day": PREV.isoformat(),
        "positions": 3,
        "budget_per_order": "666666",
        "iv_prev": None,
        "iv_gate": "0",
        "drift": None,
        "candidates": 5,
        "eligible": 5,
        "created_at": "2026-08-31T12:00:00+00:00",
    }
    (plans / f"plan-{DAY}.json").write_text(json.dumps(meta), encoding="utf-8")
    (tmp_path / "q.csv").write_text(
        "symbol,price\nA,950\nB,970\nC,990\nE,1060\nF,1080\n", encoding="utf-8"
    )
    # WBJP_LOG_DIR も tmp に向ける。.env が絶対パスで指していると、ログが本物の state/logs に
    # 漏れて（テストが JSONL を読めず）落ちる
    env = {
        "WBJP_STATE_DIR": str(state),
        "WBJP_DATA_DIR": str(tmp_path / "data"),
        "WBJP_LOG_DIR": str(state / "logs"),
        "WBJP_ENV": "uat",
    }
    return cfg, env


def test_open_and_close_dry_run_with_margin(tmp_path: Path) -> None:
    """ロングは信用買い、ショートは貸借銘柄のギャップ上位を売建て、close は返済（反対売買）を作る。"""
    from typer.testing import CliRunner

    from daytrade.cli import app

    cfg, env = _cli_env_margin(tmp_path)
    common = ["--date", DAY.isoformat(), "--config-dir", str(cfg)]
    runner = CliRunner()
    result = runner.invoke(
        app,
        ["open", *common, "--quote-source", "csv", "--quote-file", str(tmp_path / "q.csv")],
        env=env,
    )
    assert result.exit_code == 0, result.stdout
    with Ledger(tmp_path / "state" / "daytrade-uat.db") as ledger:
        entries = ledger.entries_on(DAY)
        longs = [o for o in entries if o.side is Side.BUY]
        shorts = [o for o in entries if o.side is Side.SELL]
        assert [o.symbol for o in longs] == ["A", "B", "C"]
        assert all(o.trade is TradeType.MARGIN_OPEN and o.leg == "long" for o in longs)
        assert [o.symbol for o in shorts] == ["E"]  # F は貸借銘柄でないので売れない
        assert shorts[0].trade is TradeType.MARGIN_OPEN and shorts[0].leg == "short"
        # N=2 の総予算 100 万円を、条件に合った 1 銘柄に按分 → 1,000,000 ÷ 1,060 → 900 株（ロングと同じ規則）
        assert shorts[0].quantity == Decimal(900)
        assert all(o.is_dry_run for o in entries)
        # dry-run のまま close: 全約定とみなして返済（反対売買）を作る
        for o in entries:
            ledger.update_status(o.client_order_id, OrderStatus.SUBMITTED)  # 本発注扱いにする
    result = runner.invoke(app, ["close", *common], env=env)
    assert result.exit_code == 0, result.stdout
    with Ledger(tmp_path / "state" / "daytrade-uat.db") as ledger:
        exits = {o.symbol: o for o in ledger.exits_on(DAY)}
        assert set(exits) == {"A", "B", "C", "E"}
        assert exits["A"].side is Side.SELL and exits["A"].trade is TradeType.MARGIN_CLOSE
        assert exits["E"].side is Side.BUY and exits["E"].trade is TradeType.MARGIN_CLOSE
        assert exits["E"].quantity == Decimal(900) and exits["E"].leg == "short"


# --------------------------------------------------------------------------
# 履歴（振り返り用、追記専用）
# --------------------------------------------------------------------------


def _invoke_open(tmp_path: Path, cfg: Path, env: dict[str, str]):  # type: ignore[no-untyped-def]
    from typer.testing import CliRunner

    from daytrade.cli import app

    return CliRunner().invoke(
        app,
        [
            "open",
            "--date",
            DAY.isoformat(),
            "--quote-source",
            "csv",
            "--quote-file",
            str(tmp_path / "q.csv"),
            "--config-dir",
            str(cfg),
        ],
        env=env,
    )


def test_open_appends_history_every_run_and_never_overwrites(tmp_path: Path) -> None:
    """気配の全件・順位表の全行・実行の要約が実行ごとに 1 ファイルずつ増える。

    台帳の dry-run は 2 回目で消えるが、履歴は 1 回目の分も残る。
    """
    from wbcore.history import HistoryStore

    cfg, env = _cli_env(tmp_path, 2_000_000)
    result = _invoke_open(tmp_path, cfg, env)
    assert result.exit_code == 0, result.stdout
    store = HistoryStore(tmp_path / "state" / "daytrade" / "history")

    ranking = store.read("ranking")
    assert ranking["symbol"].to_list() == ["A", "B", "C"]  # D はギャップ正で順位表に入らない
    assert ranking["picked"].to_list() == [True, True, True]
    assert ranking["side"].unique().to_list() == ["BUY"]
    # 総予算 200 万を 3 等分 → 950 円は 700 株、970 / 990 円は 600 株（単元切り捨て）
    assert ranking["quantity"].to_list() == [700.0, 600.0, 600.0]
    quotes = store.read("quotes")
    assert quotes["symbol"].to_list() == ["A", "B", "C", "D"]
    assert quotes["usable"].all()
    assert quotes.filter(pl.col("symbol") == "D")["gap"][0] > 0  # 選ばれなかった理由が追える
    run = store.read("open_run")
    assert run.height == 1
    row = run.row(0, named=True)
    assert row["mode"] == "dry_run" and row["outcome"] == "picked" and row["trade"] is True
    assert row["long_picks"] == 3 and row["short_picks"] == 0 and row["orders"] == 3
    assert row["quotes_requested"] == 4 and row["quotes_usable"] == 4
    assert row["run_id"] == ranking["run_id"][0] == quotes["run_id"][0]

    result = _invoke_open(tmp_path, cfg, env)
    assert result.exit_code == 0, result.stdout
    assert len(store.files("ranking")) == 2
    assert store.read("open_run").height == 2
    assert store.latest("ranking", DAY)["run_id"].n_unique() == 1
    with Ledger(tmp_path / "state" / "daytrade-uat.db") as ledger:
        assert len(ledger.orders_on(DAY)) == 3  # 台帳は最新の dry-run だけ


def test_open_watch_only_records_outcome_in_history(tmp_path: Path) -> None:
    from wbcore.history import HistoryStore

    code, out, _ = _open(tmp_path, 0)
    assert code == 0, out
    run = HistoryStore(tmp_path / "state" / "daytrade" / "history").read("open_run")
    row = run.row(0, named=True)
    assert row["mode"] == "watch" and row["outcome"] == "no_capital" and row["orders"] is None


def test_open_history_has_both_legs_with_margin(tmp_path: Path) -> None:
    from typer.testing import CliRunner

    from daytrade.cli import app
    from wbcore.history import HistoryStore

    cfg, env = _cli_env_margin(tmp_path)
    result = CliRunner().invoke(
        app,
        [
            "open",
            "--date",
            DAY.isoformat(),
            "--config-dir",
            str(cfg),
            "--quote-source",
            "csv",
            "--quote-file",
            str(tmp_path / "q.csv"),
        ],
        env=env,
    )
    assert result.exit_code == 0, result.stdout
    store = HistoryStore(tmp_path / "state" / "daytrade" / "history")
    ranking = store.read("ranking")
    shorts = ranking.filter(pl.col("side") == "SELL")
    assert shorts["symbol"].to_list() == ["E"] and shorts["picked"].to_list() == [True]
    assert ranking.filter(pl.col("side") == "BUY")["symbol"].to_list() == ["A", "B", "C"]
    row = store.read("open_run").row(0, named=True)
    assert row["long_picks"] == 3 and row["short_picks"] == 1 and row["short_n"] == 2
    # 履歴のコマンドが読める
    listing = CliRunner().invoke(app, ["history"], env=env)
    assert listing.exit_code == 0 and "ranking" in listing.stdout
    out = tmp_path / "ranking.csv"
    detail = CliRunner().invoke(
        app,
        ["history", "ranking", "--date", DAY.isoformat(), "--latest", "--csv", str(out)],
        env=env,
    )
    assert detail.exit_code == 0, detail.stdout
    lines = out.read_text(encoding="utf-8").splitlines()
    assert lines[0].startswith("day,run_id,recorded_at,side,rank,symbol")
    assert len(lines) == 1 + ranking.height
    assert any(",SELL,1,E," in line and ",500000.0" in line for line in lines[1:])  # 桁区切り無し


def test_plan_save_keeps_latest_and_appends_history(tmp_path: Path) -> None:
    """plan-<日付> は最新（上書き）、履歴は plan のたびに 1 ファイル増える。"""
    from daytrade.plan import Plan, PlanMeta, load, save
    from wbcore.history import HistoryStore

    frame = _cands([("A", 1000)])
    meta = PlanMeta(
        day=DAY.isoformat(),
        prev_day=PREV.isoformat(),
        positions=3,
        budget_per_order="666666",
        iv_prev=None,
        iv_gate="0",
        drift=0.001,
        candidates=1,
        eligible=1,
        created_at="2026-08-31T12:00:00+00:00",
    )
    store = HistoryStore(tmp_path / "history")
    plans = tmp_path / "plans"
    save(Plan(meta=meta, frame=frame), plans, history=store)
    save(
        Plan(meta=meta, frame=frame.with_columns(pl.lit(False).alias("eligible"))),
        plans,
        history=store,
    )
    latest = load(plans, DAY)
    assert latest is not None and latest.frame["eligible"].to_list() == [False]
    assert len(store.files("plan")) == 2
    history = store.read("plan")
    assert history["eligible"].to_list() == [True, False]
    meta_rows = store.read("plan_meta")
    assert meta_rows.height == 2 and meta_rows["prev_day"].to_list() == [PREV, PREV]
    assert meta_rows["budget_per_order"].to_list() == [666666.0, 666666.0]
    assert meta_rows["drift"].to_list() == [0.001, 0.001]


# --------------------------------------------------------------------------
# 候補の結果の評価（evaluate / review）
# --------------------------------------------------------------------------


def _write_bars(data_dir: Path, day: dt.date, rows: dict[str, tuple[float, float]]) -> None:
    """アーカイブに当日の日足を書く。rows は code → (始値, 終値)。"""
    from wbcore.data.jquants_archive import Archive, endpoint, rows_to_frame

    ep = endpoint("equities_bars_daily")
    records = [
        {
            "Date": day.isoformat(),
            "Code": code,
            "O": str(o),
            "H": str(max(o, c) + 5),
            "L": str(min(o, c) - 5),
            "C": str(c),
            "UL": "0",
            "LL": "0",
            "Vo": "1000",
        }
        for code, (o, c) in rows.items()
    ]
    Archive(data_dir / "jquants").upsert(ep, rows_to_frame(records, ep))


def test_evaluate_reconstructs_from_open_and_scores_every_candidate(tmp_path: Path) -> None:
    """9:00 の順位表が無い日は plan × 始値で順位を作り直し、選んだ銘柄も次点も評価する。"""
    from typer.testing import CliRunner

    from daytrade.cli import app
    from wbcore.history import HistoryStore

    cfg, env = _cli_env_margin(tmp_path)
    # A〜C はギャップダウン（ロング候補）、D は上、E は +6%（ショート候補）、F は貸借でない
    _write_bars(
        tmp_path / "data",
        DAY,
        {
            "A0": (950, 960),  # +1.05%
            "B0": (970, 960),  # −1.03%
            "C0": (990, 1000),  # +1.01%
            "D0": (1010, 1000),
            "E0": (1060, 1040),  # 売建なら +1.89%
            "F0": (1080, 1100),
        },
    )
    common = ["--date", DAY.isoformat(), "--config-dir", str(cfg)]
    result = CliRunner().invoke(app, ["evaluate", *common], env=env)
    assert result.exit_code == 0, result.stdout
    assert "作り直しました" in result.stdout

    store = HistoryStore(tmp_path / "state" / "daytrade" / "history")
    ev = store.read("evaluation")
    assert ev["ranking_source"].unique().to_list() == ["archive_open"]
    longs = ev.filter(pl.col("side") == "BUY").sort("rank")
    assert longs["symbol"].to_list() == ["A", "B", "C"]
    assert longs["picked"].to_list() == [True, True, True]
    assert longs["rank_group"].to_list() == ["picked"] * 3
    # 始値 950 → 終値 960 は +105 bp、費用（信用買い 5 bp）を引いて +100 bp
    a = longs.row(0, named=True)
    assert abs(a["gross_bp"] - 105.26) < 0.1 and abs(a["net_bp"] - 100.26) < 0.1
    assert (
        a["hypo_quantity"] == 700 and abs(a["hypo_pnl"] - (700 * 10 - 700 * 950 * 5 / 1e4)) < 0.01
    )
    assert a["traded"] is False and a["actual_pnl"] is None
    b = longs.row(1, named=True)
    assert b["net_bp"] < 0
    shorts = ev.filter(pl.col("side") == "SELL")
    assert shorts["symbol"].to_list() == ["E"] and shorts["picked"].to_list() == [True]
    e = shorts.row(0, named=True)
    assert e["gross_bp"] > 0 and abs(e["gross_bp"] - (1 - 1040 / 1060) * 1e4) < 0.1
    assert e["cost_bp"] == 5.0  # margin.extra_cost_bp の既定


def test_evaluate_uses_recorded_ranking_and_review_aggregates(tmp_path: Path) -> None:
    """open の記録があればそれを使う（quotes）。review は日別に picked / next / all を並べる。"""
    from typer.testing import CliRunner

    from daytrade.cli import app
    from wbcore.history import HistoryStore

    cfg, env = _cli_env_margin(tmp_path)
    _write_bars(
        tmp_path / "data",
        DAY,
        {
            "A0": (950, 970),
            "B0": (970, 980),
            "C0": (990, 985),
            "D0": (1010, 1000),
            "E0": (1060, 1070),
        },
    )
    runner = CliRunner()
    common = ["--date", DAY.isoformat(), "--config-dir", str(cfg)]
    opened = runner.invoke(
        app,
        ["open", *common, "--quote-source", "csv", "--quote-file", str(tmp_path / "q.csv")],
        env=env,
    )
    assert opened.exit_code == 0, opened.stdout
    result = runner.invoke(app, ["evaluate", *common], env=env)
    assert result.exit_code == 0, result.stdout
    store = HistoryStore(tmp_path / "state" / "daytrade" / "history")
    ev = store.read("evaluation")
    assert ev["ranking_source"].unique().to_list() == ["quotes"]
    assert ev["ranking_run_id"].n_unique() == 1 and ev["ranking_run_id"][0] is not None
    # 9:00 の気配（950）と始値（950）が同じなので gap と gap_open が一致する
    a = ev.filter(pl.col("symbol") == "A").row(0, named=True)
    assert abs(a["gap"] - a["gap_open"]) < 1e-9
    # E の売建は 1060 → 1070 で負け（−94 bp − 費用）
    e = ev.filter(pl.col("symbol") == "E").row(0, named=True)
    assert e["net_bp"] < -90

    # 2 回目の evaluate は上書きせず増える
    assert runner.invoke(app, ["evaluate", *common], env=env).exit_code == 0
    assert len(store.files("evaluation")) == 2

    out = tmp_path / "review.csv"
    reviewed = runner.invoke(app, ["review", "--csv", str(out)], env=env)
    assert reviewed.exit_code == 0, reviewed.stdout
    lines = out.read_text(encoding="utf-8").splitlines()
    assert lines[0].startswith("day,side,source,picked_n,picked_bp,next_bp,all_bp,picked_pnl")
    assert len(lines) == 3  # BUY と SELL の 1 日ずつ（最後の evaluate だけ）
    buy = next(line for line in lines[1:] if ",BUY," in line)
    assert ",quotes,3," in buy


def test_evaluate_without_bars_or_plan_skips_quietly(tmp_path: Path) -> None:
    from typer.testing import CliRunner

    from daytrade.cli import app

    cfg, env = _cli_env_margin(tmp_path)
    common = ["--config-dir", str(cfg)]
    # 日足が無い
    result = CliRunner().invoke(app, ["evaluate", "--date", DAY.isoformat(), *common], env=env)
    assert result.exit_code == 0 and "日足がまだ" in result.stdout
    # 日足はあるが plan も順位表も無い日
    other = dt.date(2026, 9, 2)
    _write_bars(tmp_path / "data", other, {"A0": (1000, 1010)})
    result = CliRunner().invoke(app, ["evaluate", "--date", other.isoformat(), *common], env=env)
    assert result.exit_code == 0 and "評価できません" in result.stdout
    review = CliRunner().invoke(app, ["review"], env=env)
    assert review.exit_code == 0 and "まだありません" in review.stdout
