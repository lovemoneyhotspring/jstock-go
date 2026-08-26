"""積立戦術の登録簿と、戦術⇔銘柄の対応設定の検証。"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import numpy as np
import polars as pl
import pytest

from wbjp.accumulate import (
    AccumulateConfig,
    AccumulationSettings,
    BearStack,
    Constant,
    DrawdownLadder,
    StackLadder,
    Tactic,
    TradingWindow,
    available,
    build_plan,
    create,
    load,
    registry,
)
from wbjp.accumulate.config import FILENAME

CONFIG = """
monthly_budget = 30_000

[[tactics]]
id = "配列4倍"
tactic = "bear_stack"
symbols = ["1305.T", "1591.T", "^GSPC", "^IXIC"]
multiplier = 4

[[tactics]]
id = "基準"
tactic = "constant"
symbols = ["^N225"]
"""


def _write(tmp_path: Path, body: str) -> Path:
    (tmp_path / FILENAME).write_text(body, encoding="utf-8")
    return tmp_path


def _bars(kind: str, n: int = 700) -> pl.DataFrame:
    closes = np.linspace(200.0, 100.0, n) if kind == "down" else np.linspace(100.0, 200.0, n)
    return pl.DataFrame(
        {
            "date": [dt.date(2020, 1, 1) + dt.timedelta(days=i) for i in range(n)],
            "close": list(closes),
        }
    )


# --- 登録簿 -------------------------------------------------------------


def test_all_tactics_are_registered() -> None:
    assert available() == ["bear_stack", "constant", "drawdown_ladder", "stack_ladder"]


def test_create_passes_parameters_through() -> None:
    tactic = create("bear_stack", {"multiplier": 2, "slow": 100})
    assert isinstance(tactic, BearStack)
    assert tactic.value == 2.0
    assert tactic.warmup_bars == 100


def test_unknown_tactic_lists_the_alternatives() -> None:
    with pytest.raises(ValueError, match="未知の戦術 'nonsense'。利用可能:"):
        create("nonsense")


def test_bad_parameter_is_reported_as_config_error() -> None:
    with pytest.raises(ValueError, match="パラメータが不正です"):
        create("bear_stack", {"nonexistent": 1})


def test_registering_a_duplicate_name_is_rejected() -> None:
    class Clash(Tactic):
        name = "bear_stack"

        def multiplier(self) -> pl.Expr:
            return pl.lit(1.0)

        @property
        def warmup_bars(self) -> int:
            return 1

    with pytest.raises(ValueError, match="既に BearStack が使用しています"):
        registry.register(Clash)


def test_tactic_without_name_is_rejected_at_definition() -> None:
    with pytest.raises(TypeError, match="name を定義してください"):

        class Nameless(Tactic):
            def multiplier(self) -> pl.Expr:
                return pl.lit(1.0)

            @property
            def warmup_bars(self) -> int:
                return 1


# --- 戦術のパラメータ検証 -------------------------------------------------


@pytest.mark.parametrize(
    "factory",
    [
        lambda: BearStack(multiplier=0.5),
        lambda: StackLadder({6: 0.5}),
        lambda: DrawdownLadder([0.1], [0.5]),
    ],
)
def test_multiplier_below_one_is_rejected(factory) -> None:
    with pytest.raises(ValueError, match=r"倍率は 1\.0 以上"):
        factory()


def test_stack_ladder_accepts_string_keys_from_toml() -> None:
    """TOML のインラインテーブルのキーは文字列で届く。"""
    tactic = StackLadder({"5": 2.0, "6": 4.0})
    assert tactic.table == {6: 4.0, 5: 2.0}


def test_stack_ladder_rejects_out_of_range_score() -> None:
    with pytest.raises(ValueError, match="弱気スコアは 0〜6"):
        StackLadder({7: 2.0})


def test_drawdown_ladder_requires_matching_lengths() -> None:
    with pytest.raises(ValueError, match="長さが違います"):
        DrawdownLadder([0.1, 0.2], [2.0])


def test_drawdown_ladder_requires_ascending_levels() -> None:
    with pytest.raises(ValueError, match="浅い順"):
        DrawdownLadder([0.3, 0.1], [2.0, 3.0])


def test_stack_ladder_with_only_six_matches_bear_stack() -> None:
    bars = _bars("down")
    settings = [
        AccumulationSettings(Decimal(25_000), t) for t in (BearStack(4), StackLadder({6: 4.0}))
    ]
    plans = [build_plan(bars, s) for s in settings]
    assert plans[0]["amount"].to_list() == plans[1]["amount"].to_list()


def test_downtrend_gate_reduces_capital_needed() -> None:
    bars = _bars("down", 900)
    plain = build_plan(bars, AccumulationSettings(Decimal(25_000), DrawdownLadder()))
    gated = build_plan(
        bars, AccumulationSettings(Decimal(25_000), DrawdownLadder(require_downtrend=True))
    )
    assert gated["extra"].sum() < plain["extra"].sum()


def test_constant_multiplier_is_one_everywhere() -> None:
    frame = _bars("up").with_columns(Constant().multiplier())
    assert (frame["multiplier"] == 1.0).all()


# --- 設定（戦術⇔銘柄） ---------------------------------------------------


def test_load_maps_every_symbol_to_its_tactic(tmp_path: Path) -> None:
    config = load(_write(tmp_path, CONFIG))
    assert config.monthly_budget == Decimal(30_000)
    assert config.symbols == ["1305.T", "1591.T", "^GSPC", "^IXIC", "^N225"]
    built = config.build()
    assert isinstance(built["1591.T"], BearStack)
    assert built["1591.T"].value == 4.0
    assert isinstance(built["^N225"], Constant)


def test_tactic_for_unassigned_symbol_is_none(tmp_path: Path) -> None:
    config = load(_write(tmp_path, CONFIG))
    assert config.tactic_for("7203.T") is None


def test_same_tactic_can_appear_twice_with_different_params(tmp_path: Path) -> None:
    body = """
[[tactics]]
id = "強め"
tactic = "bear_stack"
symbols = ["A"]
multiplier = 4

[[tactics]]
id = "弱め"
tactic = "bear_stack"
symbols = ["B"]
multiplier = 2
"""
    built = load(_write(tmp_path, body)).build()
    assert built["A"].value == 4.0
    assert built["B"].value == 2.0


def test_symbol_assigned_twice_is_rejected(tmp_path: Path) -> None:
    body = (
        CONFIG
        + """
[[tactics]]
id = "重複"
tactic = "constant"
symbols = ["1305.T"]
"""
    )
    with pytest.raises(ValueError, match="二重買付"):
        load(_write(tmp_path, body))


def test_overlap_is_allowed_when_comparing(tmp_path: Path) -> None:
    body = (
        CONFIG
        + """
[[tactics]]
id = "重複"
tactic = "constant"
symbols = ["1305.T"]
"""
    )
    config = load(_write(tmp_path, body), allow_overlap=True)
    assert len(config.active) == 3


def test_disabled_tactic_is_ignored(tmp_path: Path) -> None:
    body = (
        CONFIG
        + """
[[tactics]]
id = "止めてある"
tactic = "constant"
symbols = ["1305.T"]
enabled = false
"""
    )
    config = load(_write(tmp_path, body))  # 重複しているが enabled=false なので通る
    assert "止めてある" not in [t.id for t in config.active]


def test_duplicate_id_is_rejected(tmp_path: Path) -> None:
    body = (
        CONFIG
        + """
[[tactics]]
id = "配列4倍"
tactic = "constant"
symbols = ["9984.T"]
"""
    )
    with pytest.raises(ValueError, match="id が重複"):
        load(_write(tmp_path, body))


def test_empty_symbols_is_rejected() -> None:
    with pytest.raises(ValueError, match="symbols"):
        AccumulateConfig.model_validate(
            {"tactics": [{"id": "x", "tactic": "constant", "symbols": []}]}
        )


def test_duplicate_symbol_within_one_tactic_is_rejected() -> None:
    with pytest.raises(ValueError, match="重複しています"):
        AccumulateConfig.model_validate(
            {"tactics": [{"id": "x", "tactic": "constant", "symbols": ["A", "A"]}]}
        )


def test_symbols_are_stripped() -> None:
    config = AccumulateConfig.model_validate(
        {"tactics": [{"id": "x", "tactic": "constant", "symbols": [" A ", "B"]}]}
    )
    assert config.symbols == ["A", "B"]


def test_unknown_top_level_key_is_rejected() -> None:
    with pytest.raises(ValueError, match=r"extra_forbidden|Extra inputs"):
        AccumulateConfig.model_validate({"tactics": [], "typo": 1})


def test_bad_tactic_name_reports_which_entry(tmp_path: Path) -> None:
    body = """
[[tactics]]
id = "打ち間違い"
tactic = "bare_stack"
symbols = ["A"]
"""
    with pytest.raises(ValueError, match=r"\[打ち間違い\] 未知の戦術"):
        load(_write(tmp_path, body)).build()


def test_missing_file_is_reported(tmp_path: Path) -> None:
    with pytest.raises(FileNotFoundError, match="積立の設定が見つかりません"):
        load(tmp_path)


def test_shipped_config_is_valid() -> None:
    """同梱の config/accumulate.toml が実際に読めること。"""
    config = load(Path("config"))
    assert config.symbols
    for symbol, tactic in config.build().items():
        assert symbol
        assert tactic.warmup_bars >= 1
        assert tactic.describe()


def test_stack_ladder_rejects_inverted_multipliers() -> None:
    with pytest.raises(ValueError, match="弱気スコアが大きいほど倍率も大きく"):
        StackLadder({3: 4.0, 6: 1.5})


def test_drawdown_ladder_rejects_inverted_multipliers() -> None:
    with pytest.raises(ValueError, match="下落が深いほど倍率も大きく"):
        DrawdownLadder([0.1, 0.2], [4.0, 2.0])


def test_deepest_matching_rung_wins() -> None:
    """複数の段に該当する日は、最も深い段の倍率が使われる。"""
    bars = _bars("down", 700)
    frame = bars.with_columns(StackLadder({3: 1.5, 5: 2.0, 6: 4.0}).multiplier())
    settled = frame.tail(400)
    assert settled["multiplier"].max() == 4.0
    assert set(settled["multiplier"].unique().to_list()) <= {1.0, 1.5, 2.0, 4.0}


# --- 発注時間帯 ---------------------------------------------------------

JST = dt.timezone(dt.timedelta(hours=9))


def test_window_defaults_to_the_afternoon_slot() -> None:
    for tactic in (Constant(), BearStack(), StackLadder(), DrawdownLadder()):
        assert tactic.window.enabled
        assert tactic.window.describe() == "14:00〜15:00"


def test_window_gates_only_the_configured_hour() -> None:
    tactic = BearStack()
    day = dt.date(2026, 8, 26)
    assert not tactic.allows_order(dt.datetime.combine(day, dt.time(9, 30)))
    assert not tactic.allows_order(dt.datetime.combine(day, dt.time(13, 59)))
    assert tactic.allows_order(dt.datetime.combine(day, dt.time(14, 0)))
    assert tactic.allows_order(dt.datetime.combine(day, dt.time(14, 59)))
    assert not tactic.allows_order(dt.datetime.combine(day, dt.time(15, 0)))


def test_window_can_be_switched_off() -> None:
    tactic = BearStack(window=False)
    assert not tactic.window.enabled
    assert tactic.allows_order(dt.datetime(2026, 8, 26, 3, 0))
    assert tactic.window.describe() == "制限なし"


def test_window_is_read_from_config(tmp_path: Path) -> None:
    body = """
[[tactics]]
id = "既定"
tactic = "bear_stack"
symbols = ["A"]

[[tactics]]
id = "制限なし"
tactic = "bear_stack"
symbols = ["B"]
window = false

[[tactics]]
id = "後場寄り"
tactic = "bear_stack"
symbols = ["C"]
window = { start = "12:30", end = "13:00" }
"""
    built = load(_write(tmp_path, body)).build()
    assert built["A"].window.describe() == "14:00〜15:00"
    assert built["B"].window.describe() == "制限なし"
    assert built["C"].window.describe() == "12:30〜13:00"


def test_naive_datetimes_are_treated_as_tokyo_time() -> None:
    tactic = BearStack()
    naive = dt.datetime(2026, 8, 26, 14, 30)
    aware_utc = dt.datetime(2026, 8, 26, 5, 30, tzinfo=dt.UTC)  # = 14:30 JST
    assert tactic.allows_order(naive)
    assert tactic.allows_order(aware_utc)


@pytest.mark.parametrize(
    ("value", "message"),
    [
        ({"start": "16:00", "end": "17:00"}, "立会時間外"),
        ({"start": "11:00", "end": "12:00"}, "立会時間外"),
        ({"start": "15:00", "end": "14:00"}, "開始が終了以降"),
        ({"start": "25:00", "end": "15:00"}, "HH:MM"),
        ({"start": "afternoon", "end": "15:00"}, "HH:MM"),
        ({"start": 14, "end": "15:00"}, "文字列で"),
        ({"strat": "14:00"}, "未知のキー"),
        ("14:00-15:00", "false か"),
    ],
)
def test_invalid_window_is_rejected(value: object, message: str) -> None:
    with pytest.raises(ValueError, match=message):
        TradingWindow.parse(value)


def test_next_open_skips_to_the_window_start() -> None:
    window = TradingWindow()
    morning = dt.datetime(2026, 8, 26, 9, 30, tzinfo=JST)
    assert window.next_open(morning).time() == dt.time(14, 0)
    assert window.next_open(morning).date() == dt.date(2026, 8, 26)
    # 時間帯を過ぎていれば翌日
    evening = dt.datetime(2026, 8, 26, 16, 0, tzinfo=JST)
    assert window.next_open(evening).date() == dt.date(2026, 8, 27)
    # 時間内ならその時刻自身
    inside = dt.datetime(2026, 8, 26, 14, 30, tzinfo=JST)
    assert window.next_open(inside) == inside


def test_window_does_not_change_the_plan() -> None:
    """時間帯は投下額に影響しない。日足では時間内を再現できないため。"""
    bars = _bars("down", 900)
    budget = Decimal(25_000)
    with_window = build_plan(bars, AccumulationSettings(budget, BearStack()))
    without = build_plan(bars, AccumulationSettings(budget, BearStack(window=False)))
    assert with_window["amount"].to_list() == without["amount"].to_list()
