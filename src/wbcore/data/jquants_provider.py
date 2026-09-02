"""J-Quants API（V2）から日本株の日足を取得する。

JPX が提供する公式の株価 API。非公式のスクレイピングと違って規約上の心配が無く、株式分割・併合を反映した調整済み四本値を返す。

制約:
    - **日本株と日本の指数のみ。** 米国の指数（``market=US``）は
      :class:`~wbcore.data.provider.MarketDataError` で弾く。
    - **日足のみ。** 分足は Premium プランの別端点で、ここでは扱わない。
    - **プランで遡れる期間と遅延が違う。** Free は直近 12 週が取れず、
      Light 以上で当日足が取れる。当日の判断に使うなら Light 以上が要る。
    - **レート制限がある**（Standard は 120 回/分）。1 銘柄 1 リクエスト
      なので、取得済みの足は :class:`~wbcore.data.store.BarStore` に
      キャッシュし、増分だけ取りに行く。429 は待って再試行する。
    - 指数は ``^TOPIX``（コード ``0000``）のように ``^`` ＋指数コードで
      指定する。日経 225 の現物指数は提供されていない。

蓄積との関係（:mod:`wbcore.data.jquants_archive`）:
    ``archive`` を渡すと、要求範囲がアーカイブに揃っていれば API を叩かず
    そこから読む（オフラインで動く）。API から取ったときは応答を
    アーカイブにも書く。``accum sync`` が副産物として蓄積に貢献する。

認証:
    ダッシュボードで発行した API キーを ``x-api-key`` ヘッダで送る
    （期限なし）。キーは環境変数 ``WBJP_JQUANTS_API_KEY`` か ``.env`` から
    読む（:func:`wbcore.credentials.load_api_key`）。
"""

from __future__ import annotations

import datetime as dt
import re
from typing import Any, ClassVar, Self

import polars as pl

from wbcore.credentials import Environment
from wbcore.data.jquants_client import (
    API_KEY_VAR,
    BASE_URL,
    DEFAULT_RATE_PER_MINUTE,
    JQuantsClient,
    RateLimited,
)
from wbcore.data.provider import Interval, MarketDataError, MarketDataProvider, normalize_bars
from wbcore.domain.models import Market
from wbcore.logging import get_logger

__all__ = [
    "API_KEY_VAR",
    "BASE_URL",
    "INDEX_CODES",
    "JQuantsProvider",
    "RateLimited",
    "to_jquants_code",
]

log = get_logger(__name__)

#: 名前で引ける指数。``^`` ＋ 4 桁の指数コードなら直接指定もできる。
INDEX_CODES: dict[str, str] = {
    "^TOPIX": "0000",
}

#: 東証の銘柄コード（4 桁。``452A`` / ``5A29`` のように 2・4 桁目は英字も可）と
#: J-Quants の 5 桁コード（``72030``）を受け付ける。
_CODE = re.compile(r"^[0-9][0-9A-Z]{3}[0-9]?$")


def to_jquants_code(symbol: str) -> tuple[str, bool]:
    """銘柄コードを J-Quants の ``code`` にする。

    Returns:
        ``(code, is_index)``。``7203.T`` のような ``.T`` 付きも受け付ける。

    >>> to_jquants_code("7203")
    ('7203', False)
    >>> to_jquants_code("7203.T")
    ('7203', False)
    >>> to_jquants_code("452A.T")
    ('452A', False)
    >>> to_jquants_code("^TOPIX")
    ('0000', True)
    >>> to_jquants_code("^0028")
    ('0028', True)

    Raises:
        ValueError: 東証の銘柄コードでも指数でもないとき。
    """
    symbol = symbol.strip().upper()
    if not symbol:
        raise ValueError("銘柄コードが空です")
    if symbol.startswith("^"):
        code = INDEX_CODES.get(symbol, symbol[1:])
        if not code.isdigit() or len(code) != 4:
            raise ValueError(
                f"J-Quants で取れない指数です: {symbol}（利用可能: {sorted(INDEX_CODES)} "
                "または ^ ＋ 4 桁の指数コード）"
            )
        return code, True
    code = symbol.removesuffix(".T")
    if not _CODE.match(code):
        raise ValueError(f"東証の銘柄コードではありません: {symbol}")
    return code, False


class JQuantsProvider(MarketDataProvider):
    """J-Quants API（V2）実装。日本株の日足のみ。"""

    name: ClassVar[str] = "jquants"
    intervals: ClassVar[frozenset[Interval]] = frozenset({Interval.D1})

    def __init__(
        self,
        api_key: str,
        *,
        market: Market = Market.JP,
        base_url: str = BASE_URL,
        max_attempts: int = 8,
        timeout: int = 30,
        session: Any | None = None,
        archive: Any | None = None,
        rate_per_minute: float = DEFAULT_RATE_PER_MINUTE,
    ) -> None:
        if market is not Market.JP:
            raise MarketDataError(f"{self.name} は日本株（market = JP）専用です: {market.value}")
        self.market = market
        self.client = JQuantsClient(
            api_key,
            base_url=base_url,
            max_attempts=max_attempts,
            timeout=timeout,
            session=session,
            rate_per_minute=rate_per_minute,
        )
        self.archive = archive

    @classmethod
    def connect(cls, env: Environment, *, market: Market) -> Self:
        """API キーを解決して組み立てる。環境（uat / prod）には依存しない。

        アーカイブ（``WBJP_DATA_DIR/jquants``）を読み書きに使う。
        """
        from wbcore.credentials import load_api_key
        from wbcore.data.jquants_archive import Archive
        from wbcore.settings import AppSettings

        settings = AppSettings()
        return cls(
            load_api_key(API_KEY_VAR) or "",
            market=market,
            archive=Archive(settings.data_dir / "jquants"),
        )

    def fetch_bars(
        self,
        symbols: list[str],
        start: dt.date,
        end: dt.date,
        *,
        interval: Interval = Interval.D1,
    ) -> dict[str, pl.DataFrame]:
        self._require(interval)
        if not symbols:
            return {}
        if start > end:
            raise ValueError(f"start が end より後です: {start} > {end}")

        log.info(
            "足を取得します",
            provider=self.name,
            interval=interval.value,
            symbols=len(symbols),
            start=str(start),
            end=str(end),
        )

        result: dict[str, pl.DataFrame] = {}
        for symbol in symbols:
            try:
                code, is_index = to_jquants_code(symbol)
            except ValueError as exc:
                # 米国の指数（^GSPC 等）が混ざっていても他の銘柄は取る。
                # 取れなかった銘柄はキーごと省く（抽象の約束）
                log.warning("jquants_symbol_unsupported", symbol=symbol, reason=str(exc))
                continue
            frame = self._from_archive(code, is_index, start, end)
            source = "archive"
            if frame is None:
                path = "/indices/bars/daily" if is_index else "/equities/bars/daily"
                rows = self.client.get_all(
                    path, {"code": code, "from": start.isoformat(), "to": end.isoformat()}
                )
                self._to_archive(path, rows)
                frame = _to_frame(rows, is_index=is_index)
                source = "api"
            if frame.height > 0:
                result[symbol] = frame
                log.debug("足を取得", symbol=symbol, code=code, rows=frame.height, source=source)
            else:
                log.warning("銘柄の足が空でした", symbol=symbol, code=code)
        return result

    # -- アーカイブ -----------------------------------------------------------

    def _from_archive(
        self, code: str, is_index: bool, start: dt.date, end: dt.date
    ) -> pl.DataFrame | None:
        """要求範囲がアーカイブに揃っていればそこから作る。足りなければ None。

        「揃っている」は、その銘柄の保存済み最終日が ``end`` までの直近営業日
        以上であること。判定には取引カレンダーが要るので、無ければ使わない。
        """
        if self.archive is None:
            return None
        from wbcore.data.jquants_archive import endpoint

        ep = endpoint("/indices/bars/daily" if is_index else "/equities/bars/daily")
        try:
            stored = self.archive.read(ep, start, end)
        except Exception as exc:  # 壊れたファイル等。API に倒す
            log.warning("archive_read_failed", endpoint=ep.path, error=str(exc))
            return None
        if stored.height == 0 or "Code" not in stored.columns:
            return None
        mine = stored.filter(_code_matches(code))
        if mine.height == 0:
            return None
        last_trading = self._last_trading_day(end)
        if last_trading is None:
            return None
        have_last = mine["Date"].max()
        if have_last is None or have_last < last_trading:
            return None
        return _to_frame(mine.to_dicts(), is_index=is_index)

    def _last_trading_day(self, end: dt.date) -> dt.date | None:
        from wbcore.data.jquants_archive import TRADING_DAY_DIVISIONS, endpoint

        if self.archive is None:
            return None
        cal = self.archive.read(endpoint("/markets/calendar"), end - dt.timedelta(days=14), end)
        if cal.height == 0 or "HolDiv" not in cal.columns:
            return None
        days = cal.filter(pl.col("HolDiv").is_in(list(TRADING_DAY_DIVISIONS)))["Date"].to_list()
        return max(days) if days else None

    def _to_archive(self, path: str, rows: list[dict[str, Any]]) -> None:
        if self.archive is None or not rows:
            return
        from wbcore.data.jquants_archive import endpoint, rows_to_frame

        ep = endpoint(path)
        try:
            self.archive.upsert(ep, rows_to_frame(rows, ep))
        except Exception as exc:  # 蓄積の失敗で足の取得を止めない
            log.warning("archive_write_failed", endpoint=path, error=str(exc))


def _code_matches(code: str) -> pl.Expr:
    """入力の 4 桁コード（``7203``）と保存の 5 桁（``72030``）を突き合わせる。"""
    if len(code) == 5:
        return pl.col("Code") == code
    return pl.col("Code").str.slice(0, 4) == code


#: 応答の項目名 → 正規スキーマ。株式は調整済み（Adj*）を使う。
#: 未調整のままバックテストすると、分割日に巨大な偽の下落が現れる。
_EQUITY_COLUMNS = {
    "AdjO": "open",
    "AdjH": "high",
    "AdjL": "low",
    "AdjC": "close",
    "AdjVo": "volume",
}
#: 指数は調整の概念が無く、出来高も無い。
_INDEX_COLUMNS = {"O": "open", "H": "high", "L": "low", "C": "close"}


def _to_frame(rows: list[dict[str, Any]], *, is_index: bool) -> pl.DataFrame:
    """応答の行を正規スキーマの日足にする。

    取引の無かった日は価格が null で返る。:func:`normalize_bars` が落とす。
    指数は出来高を持たないので 0 を入れる。
    """
    mapping = _INDEX_COLUMNS if is_index else _EQUITY_COLUMNS
    if not rows:
        return pl.DataFrame(schema={"date": pl.Date, **{v: pl.Float64 for v in mapping.values()}})
    frame = pl.DataFrame(
        {
            "date": [_date(row.get("Date")) for row in rows],
            **{
                target: [_float(row.get(source)) for row in rows]
                for source, target in mapping.items()
            },
        },
        schema={"date": pl.Date, **{v: pl.Float64 for v in mapping.values()}},
    )
    if is_index:
        frame = frame.with_columns(pl.lit(0.0).alias("volume"))
    return normalize_bars(frame, Interval.D1)


def _date(value: Any) -> dt.date | None:
    if value is None or value == "":
        return None
    if isinstance(value, dt.date):
        return value
    try:
        return dt.date.fromisoformat(str(value)[:10].replace("/", "-"))
    except ValueError:
        return None


def _float(value: Any) -> float | None:
    """数値は文字列で来ることがある（財務情報の項目など）。空は null。"""
    if value is None or value == "":
        return None
    try:
        return float(value)
    except TypeError, ValueError:
        return None
