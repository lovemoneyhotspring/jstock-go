"""細かい足から粗い足を合成する（1分足 → 5分足・1時間足・日足）。

**なぜ取り込みは細かい足を基準にするのか**

粗い足は細かい足から一意に作れる（始値＝最初、高値＝最大、安値＝最小、
終値＝最後、出来高＝合計）が、逆はできない。1分足を持っていれば、
戦略が要る間隔を後から自由に選べる。

**逆はできないので、履歴は別に持つ**

細かい足は遡れる期間が短い取得元が普通（1 分足で 7 日など）。過去ぶんは
その間隔で直接取った足（日足なら数十年）で補う。合成と直接取得の
つなぎ方は :class:`wbcore.data.feed.BarFeed` が担う。

**区切りは取引所の寄付に揃える**

1 時間足を「毎正時」で切ると、NYSE（09:30 開始）では 09:30–10:00 の
半端な足が先頭にできる。区切りは取引所の現地時刻で寄付からの経過時間で
決める（09:30, 10:30, …）。日足は暦日で切る。

**合成は因果的**

各足はその区間の細かい足だけから決まる。バックテストで「見えている
細かい足まで」を合成すれば、未来を含む粗い足はできない。ただし最後の
区間は途中までしか埋まっていない「形成中の足」になる。戦略が完成した
足だけを見たいときは :func:`drop_forming` で落とす。
"""

from __future__ import annotations

import datetime as dt

import polars as pl

from wbcore.data.provider import Interval, empty_bars, normalize_bars
from wbcore.domain.models import Market
from wbcore.domain.session import session_open

_AGGREGATE = [
    pl.col("open").first(),
    pl.col("high").max(),
    pl.col("low").min(),
    pl.col("close").last(),
    pl.col("volume").sum(),
]


def can_resample(source: Interval, target: Interval) -> bool:
    """``source`` から ``target`` を作れるか。粗い→細かいは不可。割り切れない組も不可。"""
    if source is target:
        return True
    if source is Interval.D1:
        return False
    if target is Interval.D1:
        return True
    ratio = target.duration / source.duration
    return ratio >= 1 and ratio == int(ratio)


def resample(
    frame: pl.DataFrame, source: Interval, target: Interval, *, market: Market
) -> pl.DataFrame:
    """``source`` 間隔の足を ``target`` 間隔に合成する。

    Args:
        frame: :func:`~wbcore.data.provider.bar_schema` の形の足（``source`` 間隔）。
        market: 区切りを寄付に揃えるための取引所。

    Raises:
        ValueError: 合成できない組（粗い→細かい、割り切れない）のとき。
    """
    if not can_resample(source, target):
        raise ValueError(f"{source.value} 足から {target.value} 足は合成できません")
    if source is target:
        return frame
    if frame.height == 0:
        return empty_bars(target)

    ordered = frame.sort("ts")
    if target is Interval.D1:
        grouped = ordered.group_by("date", maintain_order=True).agg(_AGGREGATE)
        return normalize_bars(grouped, Interval.D1)

    tz = market.timezone.key
    open_time = session_open(market)
    every = int(target.duration.total_seconds())

    local = pl.col("ts").dt.convert_time_zone(tz)
    open_at = local.dt.truncate("1d") + pl.duration(hours=open_time.hour, minutes=open_time.minute)
    elapsed = (local - open_at).dt.total_seconds()
    bucket = open_at + pl.duration(seconds=(elapsed / every).floor().cast(pl.Int64) * every)

    grouped = (
        ordered.with_columns(bucket.dt.convert_time_zone("UTC").alias("_bucket"))
        .group_by("_bucket", maintain_order=True)
        .agg(_AGGREGATE)
        .rename({"_bucket": "ts"})
    )
    return normalize_bars(grouped, target)


def drop_forming(
    frame: pl.DataFrame, target: Interval, at: dt.datetime, *, market: Market
) -> pl.DataFrame:
    """``at`` 時点でまだ閉じていない最後の足を落とす。

    日中足は「区間の終わりが ``at`` より後」なら形成中。日足は「``at`` の
    暦日と同じ日」を形成中とみなす（引けの時刻を持たないため）。
    """
    if frame.height == 0:
        return frame
    if at.tzinfo is None:
        at = at.replace(tzinfo=dt.UTC)
    if target is Interval.D1:
        today = at.astimezone(market.timezone).date()
        return frame.filter(pl.col("date") < today)
    closes_at = pl.col("ts") + pl.duration(seconds=int(target.duration.total_seconds()))
    return frame.filter(closes_at <= pl.lit(at.astimezone(dt.UTC)))
