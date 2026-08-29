"""足の供給。「細かい基準足から合成」と「その間隔で直接取った足」をつなぐ。

戦略やエンジンは :meth:`BarFeed.read` に「この間隔で」と頼むだけで、
どこから来た足かを知らない。

三層構造:
    基準足（``base``、例: 1 分足）
        取り込みの基準。保存されている範囲では、要求された間隔の足を
        ここから :func:`~wbcore.data.resample.resample` で合成する。
    直接取得の足（要求された間隔そのもの）
        基準足で覆えない過去を補う。日足なら数十年。
    日足からの見立て（``fill_from_daily``）
        日中足がまったく無い日（分足が取れる前の期間、取得元が日足しか
        返さない期間）は、日足 1 本を「その日の引けに閉じる 1 本の日中足」
        として補う。連続した系列が要る指標のウォームアップや、履歴の
        切れ目を無くすためのもの。**保存はしない**。読み出すときだけ作り、
        ``synthetic`` 列で本物と区別できるようにする。

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
from wbcore.domain.session import session_close
from wbcore.logging import get_logger

log = get_logger(__name__)

#: 日足から見立てた足に立つ印。本物の足は False。保存されることはない。
SYNTHETIC = "synthetic"


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
        *,
        fill_from_daily: bool = True,
    ) -> dict[str, pl.DataFrame]:
        """``interval`` の足を銘柄ごとに返す。空の銘柄は含めない。

        Args:
            fill_from_daily: 日中足のとき、足の無い日を日足から見立てて補う。
                補った行は ``synthetic`` 列が True。日足の要求では何もしない。
        """
        native = self.store(interval).read_many(symbols, start, end)
        if self._derives(interval):
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
        else:
            merged = dict(native)

        if not interval.is_intraday or not fill_from_daily:
            return merged
        return self._fill_from_daily(symbols, interval, merged, start, end)

    def _fill_from_daily(
        self,
        symbols: list[str],
        interval: Interval,
        bars: dict[str, pl.DataFrame],
        start: dt.date | None,
        end: dt.date | None,
    ) -> dict[str, pl.DataFrame]:
        """日中足の無い日を、日足 1 本を「引けに閉じる 1 本の足」に見立てて補う。"""
        daily = self.store(Interval.D1).read_many(symbols, start, end)
        out: dict[str, pl.DataFrame] = {}
        for symbol in sorted(set(bars) | set(daily)):
            real = bars.get(symbol)
            missing = daily.get(symbol)
            if real is not None and missing is not None:
                covered = set(real["date"].to_list())
                missing = missing.filter(~pl.col("date").is_in(list(covered)))
            pieces: list[pl.DataFrame] = []
            if real is not None and real.height > 0:
                pieces.append(real.with_columns(pl.lit(False).alias(SYNTHETIC)))
            if missing is not None and missing.height > 0:
                pieces.append(self._synthesize(missing, interval))
                log.info(
                    "日中足の無い日を日足から見立てました",
                    symbol=symbol,
                    interval=interval.value,
                    days=missing.height,
                )
            if pieces:
                out[symbol] = pl.concat(pieces, how="vertical_relaxed").sort("ts")
        return out

    def _synthesize(self, daily: pl.DataFrame, interval: Interval) -> pl.DataFrame:
        """日足を「その日の引けに閉じる 1 本の日中足」にする。

        時刻は 引け − 足の間隔。こうすると足の終わりが引けに一致し、
        :func:`~wbcore.data.resample.drop_forming` や粗い足への再合成が
        本物の足と同じ規則で扱える。
        """
        close = session_close(self.market)
        tz = self.market.timezone.key
        offset = dt.timedelta(hours=close.hour, minutes=close.minute) - interval.duration
        ts = (
            pl.col("date")
            .cast(pl.Datetime("us"))
            .dt.replace_time_zone(tz)
            .dt.offset_by(f"{int(offset.total_seconds())}s")
            .dt.convert_time_zone("UTC")
        )
        return normalize_bars(daily.with_columns(ts.alias("ts")), interval).with_columns(
            pl.lit(True).alias(SYNTHETIC)
        )

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
        取得元が基準足に対応していなければ直接取得だけになり、読み出し時に
        日足から見立てて補う。

        Returns:
            銘柄 → ``interval`` で読める足の本数（見立ては含めない）。
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
        if provider.supports(interval):
            self.store(interval).sync(provider, symbols, start, end, force=force)
        else:
            log.warning(
                "取得元がこの間隔に対応していません。日足を取り、読み出し時に見立てます",
                provider=provider.name,
                interval=interval.value,
            )
        if interval is not Interval.D1 and provider.supports(Interval.D1):
            # 日足は常に揃える。日中足の無い日の見立てと、長い履歴の両方に要る
            self.store(Interval.D1).sync(provider, symbols, start, end, force=force)
        frames = self.read(symbols, interval, fill_from_daily=False)
        return {symbol: frames[symbol].height if symbol in frames else 0 for symbol in symbols}
