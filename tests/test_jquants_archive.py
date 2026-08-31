"""J-Quants 蓄積層のテスト。HTTP は偽クライアントで差し替える。"""

from __future__ import annotations

import datetime as dt
import gzip
from pathlib import Path
from typing import Any

import polars as pl
import pytest

from wbcore.data.jquants_archive import (
    ENDPOINTS,
    JST,
    Archive,
    Ingestor,
    Ledger,
    as_of,
    csv_to_frame,
    endpoint,
    rows_to_frame,
    typed,
)
from wbcore.data.jquants_provider import JQuantsProvider

BARS = endpoint("/equities/bars/daily")
FINS = endpoint("/fins/summary")
CAL = endpoint("/markets/calendar")


class FakeClient:
    """``get_all`` / ``bulk_list`` / ``bulk_download`` を持つ偽物。問い合わせを記録する。"""

    def __init__(self, responses: dict[str, list[dict[str, Any]]] | None = None) -> None:
        self.responses = responses or {}
        self.calls: list[tuple[str, dict[str, str]]] = []
        self.bulk_files: dict[str, tuple[str, bytes]] = {}

    def get_all(self, path: str, params: dict[str, str] | None = None) -> list[dict[str, Any]]:
        self.calls.append((path, dict(params or {})))
        return list(self.responses.get(path, []))

    def bulk_list(self, ep: str) -> list[dict[str, Any]]:
        return [
            {"Key": key, "LastModified": stamp, "Size": len(payload)}
            for key, (stamp, payload) in self.bulk_files.items()
            if key.startswith(ep.strip("/").replace("/", "_"))
        ]

    def bulk_download(self, key: str) -> bytes:
        return self.bulk_files[key][1]


def bar(date: str, code: str = "72030", close: str = "100") -> dict[str, Any]:
    return {
        "Date": date,
        "Code": code,
        "O": 1,
        "H": 2,
        "L": 1,
        "C": close,
        "AdjC": close,
        "AdjO": "1",
        "AdjH": "2",
        "AdjL": "1",
        "AdjVo": "10",
        "Vo": 10,
    }


def calendar_rows(start: dt.date, end: dt.date) -> list[dict[str, Any]]:
    rows, d = [], start
    while d <= end:
        rows.append({"Date": d.isoformat(), "HolDiv": "1" if d.weekday() < 5 else "0"})
        d += dt.timedelta(days=1)
    return rows


@pytest.fixture
def setup(tmp_path: Path) -> tuple[Archive, Ledger, FakeClient, Ingestor]:
    archive = Archive(tmp_path / "jquants")
    ledger = Ledger(archive.ledger_path)
    client = FakeClient()
    return archive, ledger, client, Ingestor(client, archive, ledger, run_id="t")


# --------------------------------------------------------------------------
# 目録
# --------------------------------------------------------------------------


def test_catalog_is_consistent() -> None:
    names = [e.name for e in ENDPOINTS]
    assert len(names) == len(set(names))
    for ep in ENDPOINTS:
        assert ep.date_column in ep.key or ep.key == ("DocId",)
        assert endpoint(ep.path) is ep
        assert endpoint(ep.name) is ep


def test_unknown_endpoint_lists_candidates() -> None:
    with pytest.raises(ValueError, match="equities_bars_daily"):
        endpoint("nope")


# --------------------------------------------------------------------------
# 行 → 保存形
# --------------------------------------------------------------------------


def test_rows_become_strings_except_dates() -> None:
    frame = rows_to_frame(
        [
            bar("2026-08-03", close="100.5"),
            {"Date": "2026-08-04", "Code": "72030", "Extra": {"a": 1}},
        ],
        BARS,
    )
    assert frame["Date"].dtype == pl.Date
    assert frame["C"].dtype == pl.String
    assert frame["C"].to_list() == ["100.5", None]
    assert frame["Extra"].to_list() == [None, '{"a": 1}']
    assert frame["O"].to_list() == ["1", None]  # 数値は表記を変えない


def test_csv_gz_is_read_as_strings() -> None:
    csv = b"Date,Code,AdjC,Vo\n2026-08-03,72030,100,\n2026/08/04,72030,101,5\n"
    frame = csv_to_frame(gzip.compress(csv), BARS)
    assert frame["Date"].to_list() == [dt.date(2026, 8, 3), dt.date(2026, 8, 4)]
    assert frame["Vo"].to_list() == [None, "5"]
    assert frame["Code"].dtype == pl.String


def test_typed_casts_numeric_columns_but_keeps_code() -> None:
    frame = rows_to_frame([bar("2026-08-03", close="100.5"), bar("2026-08-04", close="101")], BARS)
    out = typed(frame).collect()
    assert out["AdjC"].dtype == pl.Float64
    assert out["Code"].dtype == pl.String


# --------------------------------------------------------------------------
# 保管庫: 月ファイル・鍵で後勝ち
# --------------------------------------------------------------------------


def test_upsert_splits_by_month_and_overwrites_by_key(setup) -> None:  # type: ignore[no-untyped-def]
    archive, *_ = setup
    first = rows_to_frame([bar("2026-07-31", close="1"), bar("2026-08-03", close="2")], BARS)
    assert archive.upsert(BARS, first) == 2
    assert archive.months(BARS) == ["2026-07", "2026-08"]

    # 同じ鍵を新しい値で。変化は 1 行、増えた行は 1 行
    second = rows_to_frame([bar("2026-08-03", close="3"), bar("2026-08-04", close="4")], BARS)
    assert archive.upsert(BARS, second) == 2
    stored = archive.read(BARS)
    assert stored.height == 3
    assert stored.filter(pl.col("Date") == dt.date(2026, 8, 3))["AdjC"].item() == "3"

    # 同じ内容の取り直しは変化 0
    assert archive.upsert(BARS, second) == 0
    assert not list(archive.directory(BARS).glob("*.tmp"))


def test_upsert_keeps_new_columns_diagonally(setup) -> None:  # type: ignore[no-untyped-def]
    archive, *_ = setup
    archive.upsert(BARS, rows_to_frame([bar("2026-08-03")], BARS))
    archive.upsert(BARS, rows_to_frame([{**bar("2026-08-04"), "MktCap": "123"}], BARS))
    stored = archive.read(BARS)
    assert "MktCap" in stored.columns
    assert stored.sort("Date")["MktCap"].to_list() == [None, "123"]


def test_read_filters_by_period(setup) -> None:  # type: ignore[no-untyped-def]
    archive, *_ = setup
    archive.upsert(
        BARS, rows_to_frame([bar("2026-06-01"), bar("2026-07-01"), bar("2026-08-01")], BARS)
    )
    assert archive.read(BARS, dt.date(2026, 6, 15), dt.date(2026, 7, 15))["Date"].to_list() == [
        dt.date(2026, 7, 1)
    ]
    assert archive.dates(BARS) == [dt.date(2026, 6, 1), dt.date(2026, 7, 1), dt.date(2026, 8, 1)]


def test_upsert_requires_key_columns(setup) -> None:  # type: ignore[no-untyped-def]
    archive, *_ = setup
    with pytest.raises(ValueError, match="Code"):
        archive.upsert(BARS, rows_to_frame([{"Date": "2026-08-03"}], BARS))


# --------------------------------------------------------------------------
# 台帳
# --------------------------------------------------------------------------


def test_ledger_records_and_returns_latest(setup) -> None:  # type: ignore[no-untyped-def]
    _, ledger, *_ = setup
    t1 = dt.datetime(2026, 8, 3, 8, tzinfo=dt.UTC)
    ledger.record(BARS, "2026-08-03", source="api", rows=10, changed=10, digest="a", fetched=t1)
    ledger.record(
        BARS,
        "2026-08-03",
        source="api",
        rows=10,
        changed=0,
        digest="a",
        fetched=t1 + dt.timedelta(days=1),
    )
    last = ledger.last(BARS, "2026-08-03")
    assert last is not None and last.changed == 0
    assert ledger.targets(BARS) == ["2026-08-03"]
    assert ledger.summary()["fetches"].item() == 2


# --------------------------------------------------------------------------
# 取り込みの判断
# --------------------------------------------------------------------------


def _jst(y: int, m: int, d: int, hh: int, mm: int = 0) -> dt.datetime:
    return dt.datetime(y, m, d, hh, mm, tzinfo=JST)


def test_plan_waits_for_availability_then_fetches_once_per_interval(setup) -> None:  # type: ignore[no-untyped-def]
    archive, ledger, _, ing = setup
    # 2026-08-03（月）。カレンダーを先に入れておく
    archive.upsert(
        CAL, rows_to_frame(calendar_rows(dt.date(2026, 7, 20), dt.date(2026, 8, 10)), CAL)
    )

    def bars_jobs(now: dt.datetime) -> list[str]:
        return [t for ep, t, _ in ing.plan(now) if ep is BARS]

    # 16:30 前は当日を取らない（直近営業日 7/27〜7/31 は未取得なので対象）
    assert "2026-08-03" not in bars_jobs(_jst(2026, 8, 3, 16, 0))
    assert "2026-07-31" in bars_jobs(_jst(2026, 8, 3, 16, 0))
    # 16:30 を過ぎたら当日も
    assert "2026-08-03" in bars_jobs(_jst(2026, 8, 3, 16, 31))

    # 取ったら、しばらくは対象にならない（min_interval）
    ledger.record(
        BARS,
        "2026-08-03",
        source="api",
        rows=0,
        changed=0,
        digest="",
        fetched=_jst(2026, 8, 3, 16, 40),
    )
    assert "2026-08-03" not in bars_jobs(_jst(2026, 8, 3, 18, 0))
    # 翌日には取り直す（settle_days 内）
    assert "2026-08-03" in bars_jobs(_jst(2026, 8, 4, 17, 0))
    # 猶予を過ぎた後に取っていれば、もう取らない
    ledger.record(
        BARS,
        "2026-08-03",
        source="api",
        rows=0,
        changed=0,
        digest="",
        fetched=_jst(2026, 8, 9, 17, 0),
    )
    assert "2026-08-03" not in bars_jobs(_jst(2026, 8, 10, 17, 0))


def test_plan_skips_weekends_via_calendar_and_range_endpoints_use_from_to(setup) -> None:  # type: ignore[no-untyped-def]
    archive, _, _, ing = setup
    archive.upsert(
        CAL, rows_to_frame(calendar_rows(dt.date(2026, 7, 1), dt.date(2026, 8, 10)), CAL)
    )
    jobs = ing.plan(_jst(2026, 8, 3, 19, 0))
    bars_targets = [t for ep, t, _ in jobs if ep is BARS]
    assert "2026-08-01" not in bars_targets and "2026-08-02" not in bars_targets  # 土日
    inv = [(t, p) for ep, t, p in jobs if ep.path == "/equities/investor-types"]
    assert inv == [("2026-08-03", {"from": "2026-06-08", "to": "2026-08-03"})]
    cal = [p for ep, _, p in jobs if ep is CAL]
    assert cal == [{}]


def test_all_and_range_endpoints_skip_availability_wait_on_first_fetch(setup) -> None:  # type: ignore[no-untyped-def]
    """一度も取っていない端点（earnings-calendar 等）は公開時刻前でも取る。"""
    archive, ledger, _, ing = setup
    archive.upsert(
        CAL, rows_to_frame(calendar_rows(dt.date(2026, 7, 1), dt.date(2026, 8, 10)), CAL)
    )
    ecal = endpoint("/equities/earnings-calendar")
    morning = _jst(2026, 8, 3, 10, 0)  # 公開は 19:00
    assert any(ep is ecal for ep, _, _ in ing.plan(morning))
    # 一度取れば、次からは公開時刻を待つ
    ledger.record(ecal, "2026-08-03", source="api", rows=1, changed=1, digest="", fetched=morning)
    assert not any(ep is ecal for ep, _, _ in ing.plan(_jst(2026, 8, 3, 12, 0)))


def test_lookback_only_fills_never_fetched_days(setup) -> None:  # type: ignore[no-untyped-def]
    archive, ledger, _, ing = setup
    archive.upsert(
        CAL, rows_to_frame(calendar_rows(dt.date(2026, 7, 1), dt.date(2026, 8, 10)), CAL)
    )
    ledger.record(
        BARS,
        "2026-07-20",
        source="api",
        rows=1,
        changed=1,
        digest="",
        fetched=_jst(2026, 7, 20, 17, 0),
    )
    targets = [t for ep, t, _ in ing.plan(_jst(2026, 8, 3, 19, 0), lookback_days=20) if ep is BARS]
    assert "2026-07-20" not in targets
    assert "2026-07-21" in targets and "2026-07-14" in targets


def test_sync_ingests_and_records(setup) -> None:  # type: ignore[no-untyped-def]
    archive, ledger, client, ing = setup
    archive.upsert(
        CAL, rows_to_frame(calendar_rows(dt.date(2026, 7, 20), dt.date(2026, 8, 10)), CAL)
    )
    client.responses["/equities/bars/daily"] = [bar("2026-08-03"), bar("2026-08-03", code="67580")]
    result = ing.sync(_jst(2026, 8, 3, 19, 0), only=["equities_bars_daily"])
    results = result.ingests
    assert result.failures == []
    assert {r.target for r in results} >= {"2026-08-03", "2026-07-31"}
    assert all(r.endpoint == BARS.path for r in results)
    assert ("/equities/bars/daily", {"date": "2026-08-03"}) in client.calls
    assert ledger.last(BARS, "2026-08-03") is not None
    assert archive.read(BARS).height == 2  # 同じ応答を複数日で受けても鍵で 1 つに


def test_gaps_reports_missing_trading_days(setup) -> None:  # type: ignore[no-untyped-def]
    archive, ledger, _, ing = setup
    archive.upsert(CAL, rows_to_frame(calendar_rows(dt.date(2026, 8, 3), dt.date(2026, 8, 7)), CAL))
    archive.upsert(BARS, rows_to_frame([bar("2026-08-03"), bar("2026-08-05")], BARS))
    # 8/6 は「取ったが空だった」→ 欠けではない
    ledger.record(BARS, "2026-08-06", source="api", rows=0, changed=0, digest="")
    later = _jst(2026, 8, 10, 12, 0)
    assert ing.gaps(BARS, dt.date(2026, 8, 3), dt.date(2026, 8, 7), now=later) == [
        dt.date(2026, 8, 4),
        dt.date(2026, 8, 7),
    ]


def test_gaps_ignores_days_not_yet_available(setup) -> None:  # type: ignore[no-untyped-def]
    archive, _, _, ing = setup
    archive.upsert(CAL, rows_to_frame(calendar_rows(dt.date(2026, 8, 3), dt.date(2026, 8, 4)), CAL))
    # 8/4 の 12:00: 8/4 の bars（16:30 公開）はまだ欠けではない
    assert ing.gaps(
        BARS, dt.date(2026, 8, 3), dt.date(2026, 8, 4), now=_jst(2026, 8, 4, 12, 0)
    ) == [dt.date(2026, 8, 3)]
    assert ing.gaps(
        BARS, dt.date(2026, 8, 3), dt.date(2026, 8, 4), now=_jst(2026, 8, 4, 17, 0)
    ) == [dt.date(2026, 8, 3), dt.date(2026, 8, 4)]


# --------------------------------------------------------------------------
# 一括
# --------------------------------------------------------------------------


def test_backfill_converts_bulk_files_and_skips_unchanged(setup) -> None:  # type: ignore[no-untyped-def]
    archive, _, client, ing = setup
    csv = b"Date,Code,AdjC\n2025-01-06,72030,100\n2025-01-07,72030,101\n"
    client.bulk_files["equities_bars_daily_202501.csv.gz"] = (
        "2025-02-01T00:00:00Z",
        gzip.compress(csv),
    )
    client.bulk_files["equities_bars_daily_202412.csv.gz"] = (
        "2025-01-01T00:00:00Z",
        gzip.compress(b"Date,Code,AdjC\n2024-12-30,72030,99\n"),
    )

    results = ing.backfill(BARS, since="2025-01").ingests
    assert [r.rows for r in results] == [2]
    assert archive.months(BARS) == ["2025-01"]
    assert (archive.raw_dir(BARS) / "equities_bars_daily_202501.csv.gz").exists()

    # 2 回目は LastModified が同じなので飛ばす
    assert ing.backfill(BARS, since="2025-01").ingests == []
    # 全期間なら 2024-12 も
    assert [r.target for r in ing.backfill(BARS).ingests] == [
        "bulk:equities_bars_daily_202412.csv.gz"
    ]


def test_backfill_refuses_endpoints_without_bulk(setup) -> None:  # type: ignore[no-untyped-def]
    *_, ing = setup
    with pytest.raises(ValueError, match="sync"):
        ing.backfill(endpoint("/edinet/major-shareholders"))


# --------------------------------------------------------------------------
# 研究用の定型
# --------------------------------------------------------------------------


def test_as_of_picks_latest_disclosure_visible_at_date() -> None:
    frame = rows_to_frame(
        [
            {
                "DiscDate": "2026-05-10",
                "DiscTime": "15:00",
                "Code": "72030",
                "DiscNo": "1",
                "NP": "10",
            },
            {
                "DiscDate": "2026-08-05",
                "DiscTime": "15:00",
                "Code": "72030",
                "DiscNo": "2",
                "NP": "20",
            },
            {
                "DiscDate": "2026-05-12",
                "DiscTime": "15:00",
                "Code": "67580",
                "DiscNo": "3",
                "NP": "5",
            },
        ],
        FINS,
    )
    seen = as_of(frame, dt.date(2026, 8, 1))
    assert seen.sort("Code")["NP"].to_list() == ["5", "10"]


# --------------------------------------------------------------------------
# 足の取得との連携
# --------------------------------------------------------------------------


def test_provider_reads_from_archive_when_complete_and_writes_through(tmp_path: Path) -> None:
    archive = Archive(tmp_path / "jquants")
    archive.upsert(
        CAL, rows_to_frame(calendar_rows(dt.date(2026, 8, 1), dt.date(2026, 8, 10)), CAL)
    )

    class Session:
        def __init__(self) -> None:
            self.calls = 0

        def get(self, url: str, params: dict[str, str], timeout: int) -> Any:
            self.calls += 1

            class R:
                status_code = 200
                text = ""

                @staticmethod
                def json() -> Any:
                    return {
                        "data": [bar("2026-08-03", close="100"), bar("2026-08-04", close="101")]
                    }

            return R()

    session = Session()
    p = JQuantsProvider("k" * 16, session=session, archive=archive, rate_per_minute=0)

    # 1 回目: アーカイブに無いので API。応答はアーカイブに書かれる
    result = p.fetch_bars(["7203"], dt.date(2026, 8, 3), dt.date(2026, 8, 4))
    assert result["7203"]["close"].to_list() == [100.0, 101.0]
    assert session.calls == 1
    assert archive.read(BARS).height == 2

    # 2 回目: 揃っているので API を叩かない（4 桁 → 5 桁の突き合わせ）
    result = p.fetch_bars(["7203"], dt.date(2026, 8, 3), dt.date(2026, 8, 4))
    assert result["7203"]["close"].to_list() == [100.0, 101.0]
    assert session.calls == 1

    # 範囲が伸びたら（8/5 が無い）API
    p.fetch_bars(["7203"], dt.date(2026, 8, 3), dt.date(2026, 8, 5))
    assert session.calls == 2


def test_sync_continues_after_endpoint_failure(setup) -> None:  # type: ignore[no-untyped-def]
    """1 端点の 400 で全体が止まらない。失敗は集めて返す。"""
    from wbcore.data.provider import MarketDataError

    archive, ledger, client, ing = setup
    archive.upsert(
        CAL, rows_to_frame(calendar_rows(dt.date(2026, 7, 20), dt.date(2026, 8, 10)), CAL)
    )

    original = client.get_all

    def get_all(path: str, params: dict[str, str] | None = None):  # type: ignore[no-untyped-def]
        if path == "/equities/bars/daily":
            raise MarketDataError("HTTP 400")
        return original(path, params)

    client.get_all = get_all  # type: ignore[method-assign]
    client.responses["/markets/short-ratio"] = [
        {"Date": "2026-08-03", "S33": "0050", "SellExShortVa": "1"}
    ]
    result = ing.sync(_jst(2026, 8, 3, 19, 0), only=["equities_bars_daily", "markets_short_ratio"])
    assert [f.endpoint for f in result.failures] == ["/equities/bars/daily"] * len(result.failures)
    assert result.failures  # bars は失敗として記録され
    assert any(r.endpoint == "/markets/short-ratio" for r in result.ingests)  # 後続は動いた
    assert ledger.last(BARS, "2026-08-03") is None  # 失敗は台帳に「取れた」と書かない


def test_bulk_month_counts_as_fetched_for_gaps_and_lookback(setup) -> None:  # type: ignore[no-untyped-def]
    """一括で取り込んだ月は、日次の記録が無くても欠けや遡りの対象にしない。"""
    archive, ledger, _, ing = setup
    archive.upsert(
        CAL, rows_to_frame(calendar_rows(dt.date(2026, 7, 1), dt.date(2026, 8, 10)), CAL)
    )
    ledger.record(
        BARS, "bulk:equities_bars_daily_202607.csv.gz", source="bulk", rows=1, changed=1, digest="x"
    )
    later = _jst(2026, 8, 10, 12, 0)
    # 7 月は bulk で埋まっているので欠けは 8 月ぶんだけ
    gaps = ing.gaps(BARS, dt.date(2026, 7, 1), dt.date(2026, 8, 7), now=later)
    assert gaps and all(d.month == 8 for d in gaps)
    # 遡り（--days）も 7 月を叩き直さない
    targets = [t for ep, t, _ in ing.plan(later, lookback_days=40) if ep is BARS]
    assert targets and all(not t.startswith("2026-07") for t in targets)


def test_backfill_continues_after_broken_file(setup) -> None:  # type: ignore[no-untyped-def]
    """壊れたファイルは失敗として記録し、残りは取り込む。再実行で取り直せる。"""
    _, ledger, client, ing = setup
    client.bulk_files["equities_bars_daily_202501.csv.gz"] = ("t1", b"\x1f\x8b broken gzip")
    client.bulk_files["equities_bars_daily_202502.csv.gz"] = (
        "t2",
        gzip.compress(b"Date,Code,AdjC\n2025-02-03,72030,100\n"),
    )
    result = ing.backfill(BARS)
    assert [f.target for f in result.failures] == ["bulk:equities_bars_daily_202501.csv.gz"]
    assert [r.target for r in result.ingests] == ["bulk:equities_bars_daily_202502.csv.gz"]
    # 失敗したファイルは台帳に無いので、再実行の対象に残る
    assert ledger.last(BARS, "bulk:equities_bars_daily_202501.csv.gz") is None
