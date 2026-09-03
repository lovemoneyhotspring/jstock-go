"""判断 → 実績 の突き合わせを、スイング（wbjp）と積立（accum）にも広げた分。

デイトレは ``tests/test_daytrade.py`` が同じ型で確かめている。ここで見るのは
「選んだものは、選ばなかったものより良かったか」を答えられるか——比べる相手が
無いと相場全体の上下と区別できないので、対照群が集計に出ることを確かめる。
"""

from __future__ import annotations

import datetime as dt
from pathlib import Path

import polars as pl
import pytest

from accum import evaluate as accum_evaluate
from wbcore.data.store import BarStore
from wbjp import evaluate as wbjp_evaluate

DAY = dt.date(2026, 9, 3)


def _bars(closes: dict[dt.date, float]) -> pl.DataFrame:
    days = sorted(closes)
    return pl.DataFrame(
        {
            "date": days,
            "open": [closes[d] for d in days],
            "high": [closes[d] for d in days],
            "low": [closes[d] for d in days],
            "close": [closes[d] for d in days],
            "volume": [1000] * len(days),
        }
    )


def _store_with(tmp_path: Path, series: dict[str, dict[dt.date, float]]) -> BarStore:
    store = BarStore(tmp_path / "bars")
    for symbol, closes in series.items():
        store.write(symbol, _bars(closes))
    return store


def _weekdays(start: dt.date, count: int) -> list[dt.date]:
    days: list[dt.date] = []
    day = start
    while len(days) < count:
        if day.weekday() < 5:
            days.append(day)
        day += dt.timedelta(days=1)
    return days


class TestForwardClose:
    """実績は暦日ではなく**足の本数**で数える（休場を挟んでも意味が変わらない）。"""

    def test_counts_bars_not_calendar_days(self) -> None:
        days = _weekdays(DAY, 6)
        frame = _bars({d: 100.0 + i for i, d in enumerate(days)})
        found = wbjp_evaluate.forward_close(frame, days[0], horizon=3)
        assert found == (days[3], 103.0)

    def test_not_enough_bars_gives_none(self) -> None:
        days = _weekdays(DAY, 3)
        frame = _bars({d: 100.0 for d in days})
        assert wbjp_evaluate.forward_close(frame, days[0], horizon=5) is None


class TestWbjpScreenEvaluation:
    def test_adopted_is_compared_against_the_ones_not_taken(self, tmp_path: Path) -> None:
        """採用・次点・圏外が揃って初めて、選定が効いたか分かる。"""
        days = _weekdays(DAY, 6)
        entry = days[0]
        store = _store_with(
            tmp_path,
            {
                # 採用した銘柄は 5 本先で +10%
                "A.T": {d: 100.0 * (1.1 if i >= 5 else 1.0) for i, d in enumerate(days)},
                # 次点は横ばい
                "B.T": {d: 100.0 for d in days},
                # 圏外は下落
                "C.T": {d: 100.0 * (0.9 if i >= 5 else 1.0) for i, d in enumerate(days)},
            },
        )
        screens = pl.DataFrame(
            {
                "rank": [1, 2, 3],
                "symbol": ["A.T", "B.T", "C.T"],
                "score": [0.9, 0.6, 0.1],
                "passed": [True, True, False],
                "adopted": [True, False, False],
                "close": [100.0, 100.0, 100.0],
                "run_id": ["r1", "r1", "r1"],
            }
        )
        result = wbjp_evaluate.evaluate(screens, store, day=entry, horizon=5)
        assert result.height == 3

        summary = wbjp_evaluate.summarize(result)
        by_group = {r["group"]: r for r in summary.iter_rows(named=True)}
        assert set(by_group) == {"adopted", "passed", "rest"}
        assert by_group["adopted"]["avg_ret_bp"] == pytest.approx(1000.0)
        assert by_group["passed"]["avg_ret_bp"] == pytest.approx(0.0)
        assert by_group["rest"]["avg_ret_bp"] == pytest.approx(-1000.0)

    def test_symbols_without_later_bars_are_kept_but_unscored(self, tmp_path: Path) -> None:
        """実績が出ていない銘柄を落とすと、集計が「評価できたものだけ」に偏る。"""
        days = _weekdays(DAY, 6)
        store = _store_with(tmp_path, {"A.T": {d: 100.0 for d in days}})
        screens = pl.DataFrame(
            {
                "rank": [1, 2],
                "symbol": ["A.T", "MISSING.T"],
                "score": [0.9, 0.8],
                "passed": [True, True],
                "adopted": [True, True],
                "close": [100.0, 100.0],
                "run_id": ["r1", "r1"],
            }
        )
        result = wbjp_evaluate.evaluate(screens, store, day=days[0], horizon=5)
        assert result.height == 2
        missing = result.filter(pl.col("symbol") == "MISSING.T").row(0, named=True)
        assert missing["ret_bp"] is None
        # 集計には入らない
        assert wbjp_evaluate.summarize(result)["count"].sum() == 1

    def test_review_lines_up_groups_per_day(self, tmp_path: Path) -> None:
        days = _weekdays(DAY, 6)
        store = _store_with(
            tmp_path,
            {
                "A.T": {d: 100.0 * (1.05 if i >= 5 else 1.0) for i, d in enumerate(days)},
                "C.T": {d: 100.0 for d in days},
            },
        )
        screens = pl.DataFrame(
            {
                "rank": [1, 2],
                "symbol": ["A.T", "C.T"],
                "score": [0.9, 0.1],
                "passed": [True, False],
                "adopted": [True, False],
                "close": [100.0, 100.0],
                "run_id": ["r1", "r1"],
            }
        )
        result = wbjp_evaluate.evaluate(screens, store, day=days[0], horizon=5).with_columns(
            pl.lit(days[0]).alias("day")
        )
        table = wbjp_evaluate.review(result)
        row = table.row(0, named=True)
        assert row["adopted_bp"] == pytest.approx(500.0)
        assert row["rest_bp"] == pytest.approx(0.0)

        totals = wbjp_evaluate.review_totals(table).row(0, named=True)
        assert totals["days"] == 1
        assert totals["beat_rest_rate"] == pytest.approx(1.0)


class TestAccumDecisionEvaluation:
    """積立は売らないので、問うのは「増額した日は安かったか」。"""

    @pytest.mark.parametrize(
        ("multiplier", "expected"),
        [
            (1.0, "1.0（通常）"),
            (0.5, "1.0（通常）"),
            (1.2, "1.0〜1.5"),
            (1.8, "1.5〜2.0"),
            (3.0, "2.0 以上"),
        ],
    )
    def test_buckets(self, multiplier: float, expected: str) -> None:
        assert accum_evaluate.bucket_of(multiplier) == expected

    def test_normal_contributions_are_the_control_group(self, tmp_path: Path) -> None:
        days = _weekdays(DAY, 7)
        store = _store_with(
            tmp_path,
            {"1306.T": {d: (100.0 if i < 5 else 120.0) for i, d in enumerate(days)}},
        )
        decisions = pl.DataFrame(
            {
                "symbol": ["1306.T", "1306.T"],
                # 増額した日は安値（100）で、通常の日は戻したところ（110）で拾っている
                "judged_on": [days[0], days[1]],
                "close": [100.0, 110.0],
                "due": [50000.0, 25000.0],
                "multiplier": [2.0, 1.0],
            }
        )
        result = accum_evaluate.evaluate(decisions, store, horizon=5)
        summary = accum_evaluate.summarize(result)
        by_bucket = {r["bucket"]: r for r in summary.iter_rows(named=True)}

        # 対照群（通常の積立）が必ず出る。これが無いと増額が効いたか判断できない
        assert set(by_bucket) == {"1.0（通常）", "2.0 以上"}
        assert by_bucket["2.0 以上"]["avg_ret_bp"] == pytest.approx(2000.0)
        assert by_bucket["2.0 以上"]["due"] == pytest.approx(50000.0)
        # 安く拾えた増額の日のほうが、その後のリターンが大きい
        assert by_bucket["1.0（通常）"]["avg_ret_bp"] == pytest.approx(909.09, rel=1e-3)

    def test_decisions_without_later_bars_are_kept_but_unscored(self, tmp_path: Path) -> None:
        store = _store_with(tmp_path, {"1306.T": {d: 100.0 for d in _weekdays(DAY, 2)}})
        decisions = pl.DataFrame(
            {
                "symbol": ["1306.T"],
                "judged_on": [DAY],
                "close": [100.0],
                "due": [25000.0],
                "multiplier": [1.0],
            }
        )
        result = accum_evaluate.evaluate(decisions, store, horizon=20)
        assert result.height == 1
        assert result.row(0, named=True)["ret_bp"] is None
        assert accum_evaluate.summarize(result).is_empty()


def test_accum_decision_frame_keeps_the_shape_when_empty() -> None:
    from accum.history import DECISION_SCHEMA, decision_frame

    frame = decision_frame([])
    assert frame.height == 0
    assert list(frame.columns) == list(DECISION_SCHEMA)
