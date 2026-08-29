"""足の供給。「細かい基準足から合成」と「その間隔で直接取った足」をつなぐ。

戦略やエンジンは :meth:`BarFeed.read` に「この間隔で」と頼むだけで、
どこから来た足かを知らない。

二層構造:
    基準足（``base``、例: 1 分足）
        取り込みの基準。保存されている範囲では、要求された間隔の足を
        ここから :func:`~wbcore.data.resample.resample` で合成する。
    直接取得の足（要求された間隔そのもの）
        基準足で覆えない過去を補う。日足なら数十年。

両方ある区間は**直接取得の足を優先**する。日足は分割調整済み
（yfinance ``auto_adjust``）だが分足は無調整なので、分割日をまたぐと
合成した日足が段差を持つ。直接取った足の方が信頼できる。

基準足を設定しなければ、従来どおりその間隔の保存を読むだけ。
"""

from __future__ import annotations

import datetime as dt
from pathlib import Path

import polars as pl

from wbcore.data.provider import Interval, MarketDataProvider, normalize_bars
from wbcore.data.resample import can_resample, resample
from wbcore.data.store import BarStore
from wbcore.domain.models import Market
from wbcore.logging import get_logger

log = get_logger(__name__)


class BarFeed:
    """間隔を指定して足を読む窓口。"""

    def __init__(self, root: Path, market: Market, *, base: Interval | None = None) -> None:
        self.root = Path(root)
        self.market = market
        self.base = base

    def store(self, interval: Interval) -> BarStore:
        return BarStore(self.root, interval)

    def _derives(self, interval: Interval) -> bool:
        """この間隔を基準足から合成するか。"""
        return (
            self.base is not None
            and self.base is not interval
            and can_resample(self.base, interval)
        )

    def read(
        self,
        symbols: list[str],
        interval: Interval,
        start: dt.date | None = None,
        end: dt.date | None = None,
    ) -> dict[str, pl.DataFrame]:
        """``interval`` の足を銘柄ごとに返す。空の銘柄は含めない。"""
        native = self.store(interval).read_many(symbols, start, end)
        if not self._derives(interval):
            return native
        assert self.base is not None

        derived = {
            symbol: resample(frame, self.base, interval, market=self.market)
            for symbol, frame in self.store(self.base).read_many(symbols, start, end).items()
        }
        merged: dict[str, pl.DataFrame] = {}
        for symbol in sorted(set(native) | set(derived)):
            parts = [f for f in (derived.get(symbol), native.get(symbol)) if f is not None]
            # 後勝ちなので、直接取得の足を後ろに置く（重なる区間はそちらを採る）
            frame = normalize_bars(pl.concat(parts, how="vertical_relaxed"), interval)
            if frame.height > 0:
                merged[symbol] = frame
        return merged

    def sync(
        self,
        provider: MarketDataProvider,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
        interval: Interval,
        *,
        force: bool = False,
    ) -> dict[str, int]:
        """要求された間隔の足を揃える。

        基準足を合成に使うなら、基準足も直接取得の足も両方更新する。
        基準足は取得元の遡れる範囲までしか取れない（取得元が丸める）。

        Returns:
            銘柄 → ``interval`` で読める足の本数。
        """
        if self._derives(interval):
            assert self.base is not None
            if provider.supports(self.base):
                self.store(self.base).sync(provider, symbols, start, end, force=force)
            else:
                log.warning(
                    "取得元が基準足に対応していないため、直接取得の足だけを使います",
                    provider=provider.name,
                    base=self.base.value,
                )
        self.store(interval).sync(provider, symbols, start, end, force=force)
        return {symbol: frame.height for symbol, frame in self.read(symbols, interval).items()}
