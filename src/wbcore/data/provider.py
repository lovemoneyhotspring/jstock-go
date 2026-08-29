"""市場データ取得の抽象。

**なぜブローカーと分けるのか**

Webull の市場データ API は米国株にしか対応しておらず、日本株の足を
返さない（``US_STOCK`` / ``US_ETF`` のみ）。つまり「発注は Webull、
価格は別ソース」という構成が避けられない。

そこでこの層を抽象化しておく。データ源を差し替えても、戦略・エンジン・
ブローカーには一切影響が出ない。yfinance の仕様変更や、将来 J-Quants
や kabu ステーションへ移る場合にも、実装を1つ足すだけで済む。
"""

from __future__ import annotations

import datetime as dt
from abc import ABC, abstractmethod

import polars as pl

#: 足データの正規スキーマ。どの実装もこの形で返す。
BAR_SCHEMA: dict[str, pl.DataType] = {
    "date": pl.Date(),
    "open": pl.Float64(),
    "high": pl.Float64(),
    "low": pl.Float64(),
    "close": pl.Float64(),
    "volume": pl.Float64(),
}

BAR_COLUMNS = list(BAR_SCHEMA)


class MarketDataError(RuntimeError):
    """市場データの取得に失敗した。"""


class MarketDataProvider(ABC):
    """日足を供給するもの。"""

    #: ログや設定で使う識別子。
    name: str = ""

    @abstractmethod
    def fetch_daily_bars(
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
            銘柄コード → 足の DataFrame。:data:`BAR_SCHEMA` の形で、
            ``date`` 昇順。取得できなかった銘柄はキーごと省く
            （空の DataFrame ではなく不在にすることで、
            「データが無い」と「値動きが無い」を区別できる）。
        """

    def __repr__(self) -> str:
        return f"<{type(self).__name__} name={self.name!r}>"


def empty_bars() -> pl.DataFrame:
    """正規スキーマの空フレーム。"""
    return pl.DataFrame(schema=BAR_SCHEMA)


def normalize_bars(frame: pl.DataFrame) -> pl.DataFrame:
    """任意の足フレームを正規スキーマに揃える。

    - 必要な列だけを、決まった順序で残す
    - 型を揃える
    - ``date`` 昇順に並べ、重複日を後勝ちで1本にまとめる
    - 価格が欠けている行を落とす

    重複除去が要るのは、増分取得したデータと既存データを継ぎ足すと
    境界日が二重になるため。放置すると指標の窓がずれる。

    Raises:
        MarketDataError: 必要な列が足りないとき。
    """
    missing = set(BAR_COLUMNS) - set(frame.columns)
    if missing:
        raise MarketDataError(f"足データに列が不足しています: {sorted(missing)}")

    return (
        frame.select([pl.col(name).cast(dtype, strict=False) for name, dtype in BAR_SCHEMA.items()])
        .drop_nulls(subset=["date", "open", "high", "low", "close"])
        .sort("date")
        .unique(subset=["date"], keep="last", maintain_order=True)
    )
