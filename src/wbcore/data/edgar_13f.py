"""SEC EDGAR から 13F-HR（機関投資家の四半期保有報告）を取得する。

積立の「バフェット追従」で使う。対象は運用会社の CIK（バークシャーは
``0001067983``）で、四半期ごとの保有一覧を **提出日付き** で返す。

**提出日を持つ理由**

13F は四半期末から最大 45 日遅れて公開される。保有比率を四半期末
（``period``）の日付で使うと、まだ公開されていない情報で買うことになる
（ルックアヘッド）。配分に使ってよいのは ``filed`` の翌営業日以降。

**扱える範囲**

EDGAR の 13F は 2013 年第2四半期（2013-08 提出）から XML 化されている。
それ以前はプレーンテキストで様式も揺れるため対象外にしている。
約 13 年ぶんあれば、下落局面（2018・2020・2022）を含んだ検証はできる。

**CUSIP → ティッカー**

13F は銘柄を CUSIP で書き、ティッカーを持たない。対応表は
``config/cusip.toml`` に置く（:func:`load_cusip_map`）。表に無い CUSIP は
落とし、残りで比率を正規化する。買収・上場廃止で足が取れない銘柄も同様。
これは軽い生存者バイアス（買収は通常プレミアム付きなので、実際より
成績を **低め** に見せる方向）になる。

**アクセス規約**

SEC は User-Agent に連絡先を要求し、毎秒 10 リクエストを上限にしている。
取得したファイルは必ずキャッシュし、同じものを二度取りに行かない。
"""

from __future__ import annotations

import datetime as dt
import json
import re
import time
import tomllib
import urllib.request
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path

import polars as pl

from wbcore.logging import get_logger

log = get_logger(__name__)

#: バークシャー・ハサウェイの CIK。
BERKSHIRE_CIK = "0001067983"

#: XML 化された 13F が揃う最初の四半期末。
XML_SINCE = dt.date(2013, 6, 30)

#: 保存する保有一覧の列。
HOLDING_COLUMNS = ("filed", "period", "cusip", "name", "value")

_SUBMISSIONS = "https://data.sec.gov/submissions/{name}"
_ARCHIVE = "https://www.sec.gov/Archives/edgar/data/{cik}/{accession}.txt"
_INFO_TABLE = re.compile(r"<infoTable>(.*?)</infoTable>", re.S)


def _tag(block: str, name: str) -> str:
    match = re.search(rf"<{name}>(.*?)</{name}>", block, re.S)
    return match.group(1).strip() if match else ""


def parse_information_table(text: str) -> list[tuple[str, str, float]]:
    """13F の提出ファイル本文から ``(cusip, 発行体名, 評価額)`` を取り出す。

    同じ CUSIP が複数行（子会社ごとの保有）に分かれるので合算する。
    評価額の単位は 2023 年に千ドル→ドルへ変わったが、比率にしか使わない
    のでここでは揃えない。
    """
    totals: dict[str, float] = defaultdict(float)
    names: dict[str, str] = {}
    for block in _INFO_TABLE.findall(text):
        cusip = _tag(block, "cusip").upper()
        if not cusip:
            continue
        # 株式以外（債券・ワラント等）は除く。sshPrnamtType が SH のものだけ
        if _tag(block, "sshPrnamtType").upper() not in ("SH", ""):
            continue
        try:
            value = float(_tag(block, "value"))
        except ValueError:
            continue
        totals[cusip] += value
        names.setdefault(cusip, _tag(block, "nameOfIssuer").replace("&amp;", "&"))
    return [(c, names[c], v) for c, v in totals.items()]


@dataclass(frozen=True, slots=True)
class FilingRef:
    """13F-HR 1件の所在。"""

    accession: str
    filed: dt.date
    period: dt.date


class Edgar13F:
    """13F-HR を取得してローカルに保存する。

    Args:
        cik: 運用会社の CIK（10桁ゼロ埋め）。
        cache_dir: 生の提出ファイルと集計結果の保存先。
        user_agent: SEC が要求する連絡先入りの User-Agent。
    """

    def __init__(self, cik: str, cache_dir: Path, user_agent: str) -> None:
        self.cik = cik.zfill(10)
        self.cache_dir = Path(cache_dir) / self.cik
        self.user_agent = user_agent

    # -- 取得 -----------------------------------------------------------

    def _get(self, url: str) -> str:
        request = urllib.request.Request(url, headers={"User-Agent": self.user_agent})
        with urllib.request.urlopen(request, timeout=60) as response:
            return response.read().decode("latin-1")

    def list_filings(self, since: dt.date = XML_SINCE) -> list[FilingRef]:
        """提出済みの 13F-HR を四半期末の昇順で返す（訂正 13F-HR/A は含めない）。"""
        refs: list[FilingRef] = []
        root = json.loads(self._get(_SUBMISSIONS.format(name=f"CIK{self.cik}.json")))
        pages = [root["filings"]["recent"]]
        for extra in root["filings"].get("files", []):
            time.sleep(0.15)
            pages.append(json.loads(self._get(_SUBMISSIONS.format(name=extra["name"]))))
        for page in pages:
            for form, accession, filed, period in zip(
                page["form"],
                page["accessionNumber"],
                page["filingDate"],
                page["reportDate"],
                strict=True,
            ):
                if form != "13F-HR" or not period:
                    continue
                period_date = dt.date.fromisoformat(period)
                if period_date < since:
                    continue
                refs.append(FilingRef(accession, dt.date.fromisoformat(filed), period_date))
        refs.sort(key=lambda r: (r.period, r.filed))
        # 同じ四半期を2度提出している場合（分割提出）は後のものを採る
        latest: dict[dt.date, FilingRef] = {}
        for ref in refs:
            latest[ref.period] = ref
        return [latest[k] for k in sorted(latest)]

    def fetch_text(self, ref: FilingRef) -> str:
        """提出ファイル本文。キャッシュ済みなら取りに行かない。"""
        path = self.cache_dir / "raw" / f"{ref.accession}.txt"
        if path.exists():
            return path.read_text(encoding="latin-1")
        text = self._get(_ARCHIVE.format(cik=int(self.cik), accession=ref.accession))
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="latin-1")
        time.sleep(0.15)
        return text

    # -- 保存 -----------------------------------------------------------

    @property
    def holdings_path(self) -> Path:
        return self.cache_dir / "holdings.parquet"

    def sync(self, since: dt.date = XML_SINCE) -> pl.DataFrame:
        """提出一覧を更新し、未取得の四半期を取りに行って保存する。"""
        refs = self.list_filings(since)
        rows: list[dict[str, object]] = []
        for ref in refs:
            table = parse_information_table(self.fetch_text(ref))
            if not table:
                log.warning("13f_empty", accession=ref.accession, period=str(ref.period))
                continue
            rows.extend(
                {
                    "filed": ref.filed,
                    "period": ref.period,
                    "cusip": cusip,
                    "name": name,
                    "value": value,
                }
                for cusip, name, value in table
            )
        frame = pl.DataFrame(rows, schema_overrides={"value": pl.Float64}).sort(
            "period", "value", descending=[False, True]
        )
        self.holdings_path.parent.mkdir(parents=True, exist_ok=True)
        frame.write_parquet(self.holdings_path)
        return frame

    def load(self) -> pl.DataFrame:
        """保存済みの保有一覧。未取得なら空。"""
        if not self.holdings_path.exists():
            return pl.DataFrame(
                schema={
                    c: pl.Date if c in ("filed", "period") else pl.Utf8 for c in HOLDING_COLUMNS
                }
            ).with_columns(pl.col("value").cast(pl.Float64))
        return pl.read_parquet(self.holdings_path)


# -- CUSIP 対応表 -------------------------------------------------------


def load_cusip_map(config_dir: Path | str = Path("config")) -> dict[str, str]:
    """``config/cusip.toml`` の ``[map]`` を読む。値が空文字の行は「上場廃止・追跡不能」の意。"""
    path = Path(config_dir) / "cusip.toml"
    if not path.is_file():
        raise FileNotFoundError(f"CUSIP 対応表が見つかりません: {path}")
    with path.open("rb") as fh:
        raw = tomllib.load(fh)
    return {str(k).upper(): str(v).strip() for k, v in raw.get("map", {}).items()}


def weight_schedule(
    holdings: pl.DataFrame,
    cusip_map: dict[str, str],
    *,
    top: int = 15,
    min_weight: float = 0.0,
) -> list[tuple[dt.date, dict[str, float]]]:
    """保有一覧を ``(提出日, {ティッカー: 比率}) `` の列に変換する。

    四半期ごとに評価額で上位 ``top`` 銘柄を採り、対応表に無い CUSIP と
    空文字（追跡不能）を落としてから比率を正規化する。同じティッカーに
    複数 CUSIP（GOOGL/GOOG や株式併合前後）が対応する場合は合算する。

    Args:
        top: 採用する銘柄数。バークシャーは上位 10〜15 で 9 割を占める。
        min_weight: これ未満の比率は切り捨てる（端数銘柄の掃除）。
    """
    schedule: list[tuple[dt.date, dict[str, float]]] = []
    unmapped: set[str] = set()
    for (filed,), group in holdings.group_by("filed", maintain_order=True):
        assert isinstance(filed, dt.date)
        ranked = group.sort("value", descending=True).head(top)
        weights: dict[str, float] = defaultdict(float)
        for cusip, name, value in ranked.select("cusip", "name", "value").iter_rows():
            ticker = cusip_map.get(cusip)
            if ticker is None:
                unmapped.add(f"{cusip} {name}")
                continue
            if ticker == "":
                continue
            weights[ticker] += float(value)
        total = sum(weights.values())
        if total <= 0:
            continue
        normalized = {t: v / total for t, v in weights.items() if v / total >= min_weight}
        scale = sum(normalized.values())
        schedule.append((filed, {t: w / scale for t, w in sorted(normalized.items())}))
    if unmapped:
        log.warning("cusip_unmapped", items=sorted(unmapped))
    schedule.sort(key=lambda item: item[0])
    return schedule
