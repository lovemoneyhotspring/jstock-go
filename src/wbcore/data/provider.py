"""市場データ取得の抽象。

**なぜブローカーと分けるのか**

証券会社の市場データ API は、扱える市場が発注できる市場と一致するとは
限らない（日本株を発注できても足は返さない、など）。つまり「発注は証券会社、
価格は別ソース」という構成が避けられない。

そこでこの層を抽象化しておく。データ源を差し替えても、戦略・エンジン・
ブローカーには一切影響が出ない。取得元の仕様変更や、別のデータ源へ
移る場合にも、実装を1つ足すだけで済む。

扱うのは**日足だけ**。J-Quants も FRED も日足しか返さず、立花証券の API にも
足の履歴は無い。

**データ源を足すには**

1. :class:`MarketDataProvider` を継承し、``name`` / :meth:`fetch_bars` を書く
2. 設定から選べるようにするなら :meth:`MarketDataProvider.connect` も書き、
   :data:`wbcore.data.registry.PROVIDERS` に登録する

スキーマ: ``date`` を鍵にする（:data:`BAR_SCHEMA`）。
"""

from __future__ import annotations

import datetime as dt
from abc import ABC, abstractmethod
from typing import ClassVar, Self

import polars as pl

from wbcore.credentials import Environment
from wbcore.domain.models import Market

#: 日足の正規スキーマ。どの実装もこの形で返す。
BAR_SCHEMA: dict[str, pl.DataType] = {
    "date": pl.Date(),
    "open": pl.Float64(),
    "high": pl.Float64(),
    "low": pl.Float64(),
    "close": pl.Float64(),
    "volume": pl.Float64(),
}

BAR_COLUMNS = list(BAR_SCHEMA)

_PRICE_COLUMNS = ("open", "high", "low", "close")


class MarketDataError(RuntimeError):
    """市場データの取得に失敗した。"""


class MarketDataProvider(ABC):
    """足を供給するもの。"""

    #: 設定（``universe.data_provider``）とログで使う識別子。サブクラスで必ず定義する。
    name: ClassVar[str] = ""

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
    ) -> dict[str, pl.DataFrame]:
        """日足を取得する。

        Args:
            symbols: 銘柄コード（日本株は ``"7203"`` のような4桁）。
            start: 取得開始日（この日を含む）。
            end: 取得終了日（この日を含む）。

        Returns:
            銘柄コード → 足の DataFrame。:data:`BAR_SCHEMA` の形で日付昇順。
            取得できなかった銘柄はキーごと省く（空の DataFrame ではなく
            不在にすることで、「データが無い」と「値動きが無い」を区別できる）。
        """

    def fetch_daily_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
    ) -> dict[str, pl.DataFrame]:
        """:meth:`fetch_bars` の別名（互換のために残す）。"""
        return self.fetch_bars(symbols, start, end)

    def __repr__(self) -> str:
        return f"<{type(self).__name__} name={self.name!r}>"


def empty_bars() -> pl.DataFrame:
    """正規スキーマの空フレーム。"""
    return pl.DataFrame(schema=BAR_SCHEMA)


def normalize_bars(frame: pl.DataFrame) -> pl.DataFrame:
    """任意の足フレームを正規スキーマに揃える。

    - 必要な列だけを、決まった順序で残す
    - 型を揃える
    - 日付昇順に並べ、重複を後勝ちで1本にまとめる
    - 価格が欠けている行を落とす

    重複除去が要るのは、増分取得したデータと既存データを継ぎ足すと
    境界が二重になるため。放置すると指標の窓がずれる。

    Raises:
        MarketDataError: 必要な列が足りないとき。
    """
    missing = set(BAR_COLUMNS) - set(frame.columns)
    if missing:
        raise MarketDataError(f"足データに列が不足しています: {sorted(missing)}")

    return (
        frame.select([pl.col(name).cast(dtype, strict=False) for name, dtype in BAR_SCHEMA.items()])
        .drop_nulls(subset=["date", *_PRICE_COLUMNS])
        .sort("date")
        .unique(subset=["date"], keep="last", maintain_order=True)
    )
