"""時刻の扱いの規約。ここ以外で「今」を取らない。

方針:
    保存
        時刻は必ずタイムゾーン付き。日中足の ``ts`` は UTC（Parquet に tz が残る）、
        SQLite の ``placed_at`` 等は UTC の ISO 8601（``+00:00`` 付き）。
        暦日（``date``）は取引所の日付であり時刻ではないので tz を持たない。
    演算・判定
        UTC。取引所の現地時刻が要る判断（発注時間帯・引けの前後）は、
        その場で :attr:`Market.timezone` に変換して比べる。
    表示
        既定は UTC。設定（``WBJP_TIMEZONE``）で JST などを指定できる。
        どちらの場合も**時間帯を必ず添える**（``2026-08-29 06:20 UTC`` /
        ``2026-08-29 15:20 JST``）。時間帯の無い時刻を画面に出さない。

naive（tz 無し）な datetime を受け取ったら UTC とみなす。ローカル時刻と
みなすと、cron のサーバーと開発機で結果が変わる。
"""

from __future__ import annotations

import datetime as dt
from zoneinfo import ZoneInfo

UTC = dt.UTC


def now_utc() -> dt.datetime:
    """現在時刻（UTC、tz 付き）。"""
    return dt.datetime.now(UTC)


def today_utc() -> dt.date:
    """UTC で見た今日。

    JST の深夜〜朝 9 時は UTC がまだ前日。「今日の足があるか」の判断を
    ローカル日付でやると、サーバーの時間帯次第で結果が変わる。
    """
    return now_utc().date()


def ensure_utc(moment: dt.datetime) -> dt.datetime:
    """tz 付き UTC に揃える。naive は UTC とみなす。"""
    if moment.tzinfo is None:
        return moment.replace(tzinfo=UTC)
    return moment.astimezone(UTC)


def zone(name: str | ZoneInfo | None) -> ZoneInfo:
    """時間帯の名前（``"Asia/Tokyo"`` / ``"UTC"``）を ZoneInfo に。None は UTC。"""
    if name is None:
        return ZoneInfo("UTC")
    if isinstance(name, ZoneInfo):
        return name
    try:
        return ZoneInfo(name)
    except Exception:
        raise ValueError(
            f"未知の時間帯: {name!r}（例: UTC / Asia/Tokyo / America/New_York）"
        ) from None


def to_zone(moment: dt.datetime, tz: str | ZoneInfo | None = None) -> dt.datetime:
    """表示用に時間帯を変える。naive は UTC とみなす。"""
    return ensure_utc(moment).astimezone(zone(tz))


def fmt(moment: dt.datetime, tz: str | ZoneInfo | None = None, *, seconds: bool = False) -> str:
    """時間帯の略号を添えて書く。``2026-08-29 15:20 JST`` / ``2026-08-29 06:20 UTC``。

    略号を持たない時間帯（``Asia/Tokyo`` は JST、``America/New_York`` は EDT/EST）
    は ``%Z`` が UTC からのずれ（``+09``）になる。どちらでも時間帯は分かる。
    """
    local = to_zone(moment, tz)
    pattern = "%Y-%m-%d %H:%M:%S %Z" if seconds else "%Y-%m-%d %H:%M %Z"
    return local.strftime(pattern)


def fmt_time(moment: dt.datetime, tz: str | ZoneInfo | None = None) -> str:
    """時刻だけ。``15:20 JST``。"""
    return to_zone(moment, tz).strftime("%H:%M %Z")


def fmt_iso(text: object, tz: str | ZoneInfo | None = None) -> str:
    """保存されている ISO 8601 の文字列（UTC）を表示用に変える。

    DB の ``placed_at`` 等をそのまま出すと ``2026-08-29T06:36:37+00:00`` で、
    時間帯を読み取れる人にしか意味が無い。設定の時間帯で ``2026-08-29 15:36:37 JST``
    にする。日時として読めない値はそのまま返す（日付や None を壊さない）。
    """
    if not isinstance(text, str):
        return str(text)
    try:
        moment = dt.datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        return text
    if moment.tzinfo is None and "T" not in text:
        return text  # 日付だけ（"2026-08-29"）。時刻ではない
    return fmt(moment, tz, seconds=True)


def stamp_iso(tz: str | ZoneInfo | None = None) -> str:
    """ログ用の現在時刻。設定の時間帯で、オフセット付き ISO（``+09:00``）。"""
    return to_zone(now_utc(), tz).isoformat(timespec="microseconds")
