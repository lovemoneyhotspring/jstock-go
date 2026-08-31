"""前夜の候補生成（``daytrade plan``）とその保存。

アーカイブから 1 日ぶんの断片を切り出して :func:`daytrade.universe.candidates` に渡し、
``state/daytrade/plan-<日付>.parquet`` と ``.json``（メタ情報）に残す。
9:00 の ``open`` はこれを読むだけで、アーカイブを開かない。
"""

from __future__ import annotations

import datetime as dt
import json
from dataclasses import asdict, dataclass
from pathlib import Path

import polars as pl

from daytrade.calendar import TradingCalendar
from daytrade.config import DaytradeConfig
from daytrade.universe import Inputs, candidates, num
from wbcore.data.jquants_archive import Archive, endpoint


@dataclass(frozen=True, slots=True)
class PlanMeta:
    day: str
    prev_day: str
    positions: int
    budget_per_order: str
    iv_prev: float | None
    iv_gate: str
    #: TOPIX の日中ドリフト（前日まで、``regime.drift_days`` 日平均）
    drift: float | None
    candidates: int
    eligible: int
    created_at: str


@dataclass(frozen=True, slots=True)
class Plan:
    meta: PlanMeta
    frame: pl.DataFrame

    @property
    def day(self) -> dt.date:
        return dt.date.fromisoformat(self.meta.day)

    @property
    def eligible(self) -> pl.DataFrame:
        return self.frame.filter(pl.col("eligible"))


def plan_paths(directory: Path, day: dt.date) -> tuple[Path, Path]:
    stem = directory / f"plan-{day.isoformat()}"
    return stem.with_suffix(".parquet"), stem.with_suffix(".json")


def load_inputs(
    archive: Archive, prev_day: dt.date, day: dt.date, config: DaytradeConfig
) -> Inputs:
    """アーカイブから 1 日ぶんの断片を切り出す。"""
    lookback = prev_day - dt.timedelta(days=config.universe.turnover_days * 2 + 10)
    bars = archive.read(endpoint("equities_bars_daily"), lookback, prev_day)
    master_all = archive.read(endpoint("equities_master"), prev_day - dt.timedelta(days=10), day)
    if master_all.height == 0:
        raise ValueError("銘柄一覧（equities/master）がありません。jquants sync を先に")
    latest = master_all.filter(pl.col("Date") <= day).select(pl.col("Date").max()).item()
    master = master_all.filter(pl.col("Date") == latest)
    fins = archive.read(endpoint("fins_summary"), prev_day, prev_day)
    sched = archive.read(endpoint("fins_earnings_date"), day - dt.timedelta(days=120), day)
    if sched.height and "SchDate" in sched.columns:
        sched = sched.filter(pl.col("SchDate") == day, pl.col("PubDate") < day)
    alert = archive.read(endpoint("markets_margin_alert"), prev_day, prev_day)
    return Inputs(bars=bars, master=master, fins=fins, earnings_dates=sched, margin_alert=alert)


def iv_on(archive: Archive, day: dt.date) -> float | None:
    """その日の日経 225 オプション ``BaseVol`` の中央値（無ければ None）。"""
    opt = archive.read(endpoint("derivatives_bars_daily_options_225"), day, day)
    if opt.height == 0 or "BaseVol" not in opt.columns:
        return None
    value = opt.select(num("BaseVol").median()).item()
    return float(value) if value is not None else None


def build(
    archive: Archive,
    config: DaytradeConfig,
    day: dt.date,
    *,
    calendar: TradingCalendar | None = None,
    now: dt.datetime | None = None,
) -> Plan:
    """判定日 ``day`` の候補を作る。"""
    cal = calendar or TradingCalendar.from_archive(archive)
    prev_day = cal.previous_trading_day(day)
    inputs = load_inputs(archive, prev_day, day, config)
    frame = candidates(inputs, day, prev_day, config.universe)
    from daytrade.regime import topix_drift

    meta = PlanMeta(
        day=day.isoformat(),
        prev_day=prev_day.isoformat(),
        positions=config.capital.positions,
        budget_per_order=str(config.capital.budget_per_order),
        iv_prev=iv_on(archive, prev_day),
        iv_gate=str(config.regime.iv_gate),
        drift=topix_drift(archive, prev_day, config.regime.drift_days),
        candidates=frame.height,
        eligible=int(frame["eligible"].sum()),
        created_at=(now or dt.datetime.now(dt.UTC)).isoformat(timespec="seconds"),
    )
    return Plan(meta=meta, frame=frame)


def save(plan: Plan, directory: Path) -> tuple[Path, Path]:
    parquet, meta = plan_paths(directory, plan.day)
    directory.mkdir(parents=True, exist_ok=True)
    tmp = parquet.with_suffix(".parquet.tmp")
    plan.frame.write_parquet(tmp)
    tmp.replace(parquet)
    meta.write_text(json.dumps(asdict(plan.meta), ensure_ascii=False, indent=1), encoding="utf-8")
    return parquet, meta


def load(directory: Path, day: dt.date) -> Plan | None:
    parquet, meta = plan_paths(directory, day)
    if not parquet.is_file() or not meta.is_file():
        return None
    raw = json.loads(meta.read_text(encoding="utf-8"))
    raw.setdefault("drift", None)  # 古い plan（信号を持たない）も読める
    return Plan(meta=PlanMeta(**raw), frame=pl.read_parquet(parquet))
