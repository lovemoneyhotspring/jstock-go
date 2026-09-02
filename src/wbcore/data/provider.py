"""市場データ取得の抽象。

**なぜブローカーと分けるのか**

証券会社の市場データ API は、扱える市場が発注できる市場と一致するとは
限らない（日本株を発注できても足は返さない、など）。つまり「発注は証券会社、
価格は別ソース」という構成が避けられない。

そこでこの層を抽象化しておく。データ源を差し替えても、戦略・エンジン・
ブローカーには一切影響が出ない。取得元の仕様変更や、別のデータ源へ
移る場合にも、実装を1つ足すだけで済む。

**足の間隔（:class:`Interval`）**

日足だけでなく分足・時間足も同じ抽象で扱う。取得元ごとに対応する間隔が
違う（今ある J-Quants / FRED はどちらも日足のみ）ので、各実装は
``intervals`` で対応範囲を申告し、範囲外は :class:`MarketDataError` で弾く。

**データ源を足すには**

1. :class:`MarketDataProvider` を継承し、``name`` / ``intervals`` / :meth:`fetch_bars` を書く
2. 設定から選べるようにするなら :meth:`MarketDataProvider.connect` も書き、
   :data:`wbcore.data.registry.PROVIDERS` に登録する

スキーマ:
    日足は ``date`` を鍵にする（:data:`BAR_SCHEMA`）。
    日中足は ``ts``（UTC の日時）を鍵にし、``date`` も持つ
    （:data:`INTRADAY_BAR_SCHEMA`）。``date`` を残すのは、日単位で動く
    既存の指標・レジーム判定・「基準日」の切り詰めがそのまま使えるようにするため。
"""

from __future__ import annotations

import datetime as dt
from abc import ABC, abstractmethod
from enum import StrEnum
from typing import ClassVar, Self

import polars as pl

from wbcore.credentials import Environment
from wbcore.domain.models import Market


class Interval(StrEnum):
    """足の間隔。値は設定ファイルの表記（``1d`` / ``5m`` …）。"""

    M1 = "1m"
    M5 = "5m"
    M15 = "15m"
    M30 = "30m"
    H1 = "1h"
    D1 = "1d"

    @property
    def duration(self) -> dt.timedelta:
        return _DURATIONS[self]

    @property
    def is_intraday(self) -> bool:
        return self is not Interval.D1

    @property
    def time_column(self) -> str:
        """この間隔の足を一意にする列。"""
        return "ts" if self.is_intraday else "date"

    @classmethod
    def parse(cls, value: str | Interval) -> Interval:
        """文字列から引く。未知なら候補を添えて弾く。"""
        if isinstance(value, Interval):
            return value
        try:
            return cls(value)
        except ValueError:
            raise ValueError(
                f"未知の足の間隔 {value!r}。利用可能: {[i.value for i in cls]}"
            ) from None

    def bars_in(self, window: dt.timedelta | str) -> int:
        """時間の窓を、この間隔での足の本数に直す。

        「15分の移動平均」は 5 分足なら 3 本、1 分足なら 15 本。戦略が窓を
        時間で持てば、足の間隔を変えても意味が変わらない。

        >>> Interval.M5.bars_in("1h")
        12
        >>> Interval.D1.bars_in(dt.timedelta(days=25))
        25

        Raises:
            ValueError: 窓がこの間隔より短いとき（0 本になってしまう）。
        """
        span = parse_duration(window) if isinstance(window, str) else window
        bars = int(span / self.duration)
        if bars < 1:
            raise ValueError(f"窓 {span} は {self.value} 足 1 本より短い")
        return bars


_DURATION_UNITS: dict[str, dt.timedelta] = {
    "m": dt.timedelta(minutes=1),
    "min": dt.timedelta(minutes=1),
    "h": dt.timedelta(hours=1),
    "d": dt.timedelta(days=1),
}


def parse_duration(value: str | dt.timedelta) -> dt.timedelta:
    """``"15m"`` / ``"2h"`` / ``"3d"`` のような表記を timedelta にする。

    >>> parse_duration("90m")
    datetime.timedelta(seconds=5400)
    """
    if isinstance(value, dt.timedelta):
        return value
    text = value.strip().lower()
    for unit in sorted(_DURATION_UNITS, key=len, reverse=True):
        if text.endswith(unit) and text[: -len(unit)].strip().isdigit():
            return int(text[: -len(unit)]) * _DURATION_UNITS[unit]
    raise ValueError(f"時間の表記が不正です: {value!r}（例: 15m / 2h / 3d）")


_DURATIONS: dict[Interval, dt.timedelta] = {
    Interval.M1: dt.timedelta(minutes=1),
    Interval.M5: dt.timedelta(minutes=5),
    Interval.M15: dt.timedelta(minutes=15),
    Interval.M30: dt.timedelta(minutes=30),
    Interval.H1: dt.timedelta(hours=1),
    Interval.D1: dt.timedelta(days=1),
}

#: 日足の正規スキーマ。どの実装もこの形で返す。
BAR_SCHEMA: dict[str, pl.DataType] = {
    "date": pl.Date(),
    "open": pl.Float64(),
    "high": pl.Float64(),
    "low": pl.Float64(),
    "close": pl.Float64(),
    "volume": pl.Float64(),
}

#: 日中足の正規スキーマ。``ts`` は UTC。``date`` は UTC で見た暦日
#: （東証 09:00–15:30 JST も NYSE 09:30–16:00 ET も UTC の同じ日に収まる）。
INTRADAY_BAR_SCHEMA: dict[str, pl.DataType] = {
    "ts": pl.Datetime("us", "UTC"),
    **BAR_SCHEMA,
}

BAR_COLUMNS = list(BAR_SCHEMA)

_PRICE_COLUMNS = ("open", "high", "low", "close")


def bar_schema(interval: Interval = Interval.D1) -> dict[str, pl.DataType]:
    """間隔に応じた正規スキーマ。"""
    return INTRADAY_BAR_SCHEMA if interval.is_intraday else BAR_SCHEMA


class MarketDataError(RuntimeError):
    """市場データの取得に失敗した。"""


class MarketDataProvider(ABC):
    """足を供給するもの。"""

    #: 設定（``universe.data_provider``）とログで使う識別子。サブクラスで必ず定義する。
    name: ClassVar[str] = ""

    #: 供給できる足の間隔。既定は日足のみ。
    intervals: ClassVar[frozenset[Interval]] = frozenset({Interval.D1})

    @classmethod
    def connect(cls, env: Environment, *, market: Market) -> Self:
        """環境と市場から組み立てる。設定ファイルの名前で選ぶときに使う。

        認証情報が要る取得元はここで解決する。テスト用の取得元（CSV など）は
        設定から選ぶ意味が無いので、既定では弾く。
        """
        raise MarketDataError(
            f"{cls.name} は設定からは選べません。コードから直接組み立ててください"
        )

    @abstractmethod
    def fetch_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
        *,
        interval: Interval = Interval.D1,
    ) -> dict[str, pl.DataFrame]:
        """足を取得する。

        Args:
            symbols: 銘柄コード（日本株は ``"7203"`` のような4桁）。
            start: 取得開始日（この日を含む）。
            end: 取得終了日（この日を含む）。
            interval: 足の間隔。対応していなければ :class:`MarketDataError`。

        Returns:
            銘柄コード → 足の DataFrame。:func:`bar_schema` の形で、
            時刻昇順。取得できなかった銘柄はキーごと省く
            （空の DataFrame ではなく不在にすることで、
            「データが無い」と「値動きが無い」を区別できる）。
        """

    def fetch_daily_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
    ) -> dict[str, pl.DataFrame]:
        """日足を取得する。:meth:`fetch_bars` の日足版（互換のために残す）。"""
        return self.fetch_bars(symbols, start, end, interval=Interval.D1)

    def supports(self, interval: Interval) -> bool:
        return interval in self.intervals

    def _require(self, interval: Interval) -> None:
        """対応していない間隔を弾く。黙って日足を返すと戦略が別の足で動く。"""
        if interval not in self.intervals:
            supported = sorted(i.value for i in self.intervals)
            raise MarketDataError(
                f"{self.name} は {interval.value} 足に対応していません。対応: {supported}"
            )

    def __repr__(self) -> str:
        return f"<{type(self).__name__} name={self.name!r}>"


def empty_bars(interval: Interval = Interval.D1) -> pl.DataFrame:
    """正規スキーマの空フレーム。"""
    return pl.DataFrame(schema=bar_schema(interval))


def normalize_bars(frame: pl.DataFrame, interval: Interval = Interval.D1) -> pl.DataFrame:
    """任意の足フレームを正規スキーマに揃える。

    - 必要な列だけを、決まった順序で残す
    - 型を揃える（日中足の ``ts`` は UTC に統一。tz 無しは UTC とみなす）
    - 日中足で ``date`` が無ければ ``ts`` から作る
    - 時刻昇順に並べ、重複を後勝ちで1本にまとめる
    - 価格が欠けている行を落とす

    重複除去が要るのは、増分取得したデータと既存データを継ぎ足すと
    境界が二重になるため。放置すると指標の窓がずれる。

    Raises:
        MarketDataError: 必要な列が足りないとき。
    """
    key = interval.time_column
    required = set(BAR_COLUMNS) - {"date"} | {key}
    missing = required - set(frame.columns)
    if missing:
        raise MarketDataError(f"足データに列が不足しています: {sorted(missing)}")

    if interval.is_intraday:
        frame = frame.with_columns(_to_utc(frame, "ts"))
        if "date" not in frame.columns:
            frame = frame.with_columns(pl.col("ts").dt.date().alias("date"))

    schema = bar_schema(interval)
    return (
        frame.select([pl.col(name).cast(dtype, strict=False) for name, dtype in schema.items()])
        .drop_nulls(subset=[key, *_PRICE_COLUMNS])
        .sort(key)
        .unique(subset=[key], keep="last", maintain_order=True)
    )


def _to_utc(frame: pl.DataFrame, column: str) -> pl.Expr:
    """日時列を UTC の tz 付きに揃える式。"""
    dtype = frame.schema[column]
    expr = pl.col(column)
    if dtype == pl.String:
        expr = expr.str.to_datetime(strict=False)
        dtype = pl.Datetime("us")
    if isinstance(dtype, pl.Datetime) and dtype.time_zone is None:
        return expr.dt.replace_time_zone("UTC").cast(pl.Datetime("us", "UTC"))
    return expr.dt.convert_time_zone("UTC").cast(pl.Datetime("us", "UTC"))
