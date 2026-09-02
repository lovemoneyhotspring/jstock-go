"""前夜の米国市場（S&P500・VIX）。寄付前に分かる危険信号の材料。

米国の引けは 6:00 JST。9:00 の ``open`` が取りに行く（20:30 の plan には無い）。
取得元は FRED（:mod:`wbcore.data.fred_provider`。``SP500`` / ``VIXCLS`` の日次終値）。
取れなければ None を返し、ゲートは効かない。
バックテスト用に ``data/daytrade/us.parquet`` へ溜める（取得元から取り直せるので data 側）。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import polars as pl

from wbcore.data.fred_provider import fetch_closes
from wbcore.logging import get_logger

log = get_logger(__name__)

#: FRED の系列 ID → 列名。
SYMBOLS = {"SP500": "spx", "VIXCLS": "vix"}


@dataclass(frozen=True, slots=True)
class UsSession:
    """米国のある取引日の要約。"""

    date: dt.date
    spx_ret: float
    vix: float


def _download(start: dt.date, end: dt.date) -> pl.DataFrame:
    """FRED から終値を取り、``Date`` / ``spx`` / ``vix`` の表にする。"""
    frames: list[pl.DataFrame] = []
    for series, name in SYMBOLS.items():
        closes = fetch_closes(series, start, end)
        if closes.height == 0:
            continue
        frames.append(closes.select(pl.col("date").alias("Date"), pl.col("close").alias(name)))
    if not frames:
        return pl.DataFrame({"Date": pl.Series([], dtype=pl.Date)})
    out = frames[0]
    for frame in frames[1:]:
        out = out.join(frame, on="Date", how="full", coalesce=True)
    return out.sort("Date")


def sessions_from(frame: pl.DataFrame) -> pl.DataFrame:
    """終値の表（``Date`` / ``spx`` / ``vix``）→ ``Date`` / ``spx_ret`` / ``vix``。"""
    if frame.height == 0 or "spx" not in frame.columns:
        return pl.DataFrame(
            {
                "Date": pl.Series([], dtype=pl.Date),
                "spx_ret": pl.Series([], dtype=pl.Float64),
                "vix": pl.Series([], dtype=pl.Float64),
            }
        )
    out = frame.sort("Date").with_columns(spx_ret=pl.col("spx") / pl.col("spx").shift(1) - 1)
    if "vix" not in out.columns:
        out = out.with_columns(vix=pl.lit(None, dtype=pl.Float64))
    return out.select("Date", "spx_ret", "vix").filter(pl.col("spx_ret").is_not_null())


def history(cache: Path, start: dt.date, end: dt.date) -> pl.DataFrame:
    """``start``〜``end`` の米国セッション。キャッシュが足りなければ取り直す。"""
    frame: pl.DataFrame | None = None
    if cache.is_file():
        frame = pl.read_parquet(cache)
        have_start: Any = frame.select(pl.col("Date").min()).item() if frame.height else None
        have_end: Any = frame.select(pl.col("Date").max()).item() if frame.height else None
        if (
            not isinstance(have_start, dt.date)
            or not isinstance(have_end, dt.date)
            or have_start > start
            or have_end < end - dt.timedelta(days=4)
        ):
            frame = None
    if frame is None:
        frame = _download(start - dt.timedelta(days=10), end)
        cache.parent.mkdir(parents=True, exist_ok=True)
        frame.write_parquet(cache)
    return sessions_from(frame)


def latest_before(day: dt.date, *, cache: Path | None = None) -> UsSession | None:
    """判定日 ``day`` の寄付前に確定している最新の米国セッション（NY 日付 ≤ day−1）。"""
    try:
        frame = _download(day - dt.timedelta(days=14), day - dt.timedelta(days=1))
    except Exception as exc:  # 取得元の障害で寄付の判断を止めない
        log.warning("米国市場の取得に失敗", code="daytrade.us_missing", error=str(exc))
        return None
    sessions = sessions_from(frame).filter(pl.col("Date") <= day - dt.timedelta(days=1))
    if sessions.height == 0:
        return None
    row: dict[str, Any] = sessions.row(-1, named=True)
    return UsSession(date=row["Date"], spx_ret=float(row["spx_ret"]), vix=float(row["vix"] or 0.0))


def as_of_frame(sessions: pl.DataFrame, days: pl.DataFrame) -> pl.DataFrame:
    """東証の日付ごとに、前日以前の最新の米国セッションを当てる（バックテスト用）。"""
    if sessions.height == 0:
        return days.with_columns(
            spx_ret=pl.lit(None, dtype=pl.Float64), vix=pl.lit(None, dtype=pl.Float64)
        )
    left = (
        days.select("Date")
        .with_columns(us_asof=pl.col("Date") - pl.duration(days=1))
        .sort("us_asof")
    )
    right = sessions.rename({"Date": "us_asof"}).sort("us_asof")
    return (
        left.join_asof(right, on="us_asof", strategy="backward", tolerance="6d")
        .select("Date", "spx_ret", "vix")
        .sort("Date")
    )
