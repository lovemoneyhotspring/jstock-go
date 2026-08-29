"""財務データによる「質」のスクリーニング（バフェット流の母集団選別）。

スイングで最も危険なのは、業績の悪い銘柄が悪材料でギャップダウンし
損切りが効かないこと。収益性・堀・財務健全性で母集団を絞り、
「突然死」の確率を下げるのが目的。

**何を見るか（config で閾値を変えられる）**

- 資本効率: ROE（純利益 ÷ 自己資本）が全期間で下限以上
- 価格決定力: 粗利益率が下限以上で、全期間の最小値が下限を割らない
- 収益性: 営業利益率が下限以上
- 財務健全性: 負債資本倍率（有利子負債 ÷ 自己資本）が上限未満、
  インタレスト・カバレッジ（EBIT ÷ 支払利息）が下限超
- 現金の質: FCF ÷ 純利益 が下限以上、FCF の年率成長率が下限超

**限界（読む前に知っておくこと）**

yfinance の財務諸表は **直近 4 期分** しか取れない。したがって
「過去 5 年安定」は「直近 4 期安定」で代用し、バックテストでは
**今日の財務で選んだ母集団を過去に遡って使う** ことになる。
これはルックアヘッド（生存者）バイアスであり、結果は実際より良く出る。
検証記録にはこの旨を必ず書く。
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from wbcore.clock import today_utc
from wbcore.logging import get_logger

log = get_logger(__name__)

#: 取り出す項目。yfinance の行名 → 内部名。
_INCOME = {
    "Total Revenue": "revenue",
    "Gross Profit": "gross_profit",
    "Operating Income": "operating_income",
    "EBIT": "ebit",
    "Interest Expense": "interest_expense",
    "Net Income": "net_income",
}
_BALANCE = {
    "Stockholders Equity": "equity",
    "Total Debt": "total_debt",
    "Cash And Cash Equivalents": "cash",
}
_CASHFLOW = {
    "Free Cash Flow": "fcf",
}


@dataclass(frozen=True, slots=True)
class QualityThresholds:
    """スクリーニングの閾値。既定はバフェット流の目安。"""

    min_roe: float = 0.18
    min_gross_margin: float = 0.40
    min_operating_margin: float = 0.18
    max_debt_to_equity: float = 1.0
    #: D/E が上限を超えても、純有利子負債 ÷ EBIT がこれ未満なら健全とみなす
    #: （自社株買いで自己資本が縮んだ優良企業を落とさないための代替条件）
    max_net_debt_to_ebit: float = 2.0
    min_interest_coverage: float = 8.0
    min_fcf_to_net_income: float = 1.0
    min_fcf_growth: float = 0.08
    #: 条件を判定する最低の年数。これより短い履歴しか無い銘柄は落とす。
    min_years: int = 3

    @classmethod
    def relaxed(cls) -> QualityThresholds:
        """30〜50 銘柄に収める緩め設定。質の順序は変えず、閾値だけ下げる。"""
        return cls(
            min_roe=0.15,
            min_operating_margin=0.15,
            min_fcf_to_net_income=0.8,
            min_fcf_growth=0.05,
        )


@dataclass(slots=True)
class QualityReport:
    """1銘柄の判定結果。"""

    symbol: str
    years: int
    metrics: dict[str, float] = field(default_factory=dict)
    failed: list[str] = field(default_factory=list)

    @property
    def passed(self) -> bool:
        return not self.failed


def _series(table: Any, mapping: dict[str, str]) -> dict[str, list[float | None]]:
    """pandas の財務諸表（行=項目、列=期）を ``内部名 → 新しい期から順の値`` に。"""
    out: dict[str, list[float | None]] = {}
    if table is None or getattr(table, "empty", True):
        return out
    columns = sorted(table.columns, reverse=True)
    for row, name in mapping.items():
        if row not in table.index:
            continue
        values = []
        for col in columns:
            value = table.at[row, col]
            values.append(
                None
                if value is None or (isinstance(value, float) and math.isnan(value))
                else float(value)
            )
        out[name] = values
    return out


def fetch_statements(symbol: str) -> dict[str, Any]:
    """yfinance から年次の損益・貸借・CF を取り、JSON 化できる形で返す。"""
    import yfinance as yf

    ticker = yf.Ticker(symbol)
    data: dict[str, Any] = {"symbol": symbol, "fetched": today_utc().isoformat()}
    data.update(_series(ticker.financials, _INCOME))
    data.update(_series(ticker.balance_sheet, _BALANCE))
    data.update(_series(ticker.cashflow, _CASHFLOW))
    return data


def _ratio(numer: list[float | None] | None, denom: list[float | None] | None) -> list[float]:
    if not numer or not denom:
        return []
    out = []
    for n, d in zip(numer, denom, strict=False):
        if n is None or d is None or d == 0:
            continue
        out.append(n / d)
    return out


def evaluate(data: dict[str, Any], thresholds: QualityThresholds | None = None) -> QualityReport:
    """財務データを閾値で判定する。純粋関数。"""
    t = thresholds or QualityThresholds()
    symbol = str(data.get("symbol", "?"))
    revenue = data.get("revenue") or []
    years = len([v for v in revenue if v is not None])
    report = QualityReport(symbol=symbol, years=years)
    if years < t.min_years:
        report.failed.append(f"履歴 {years} 年 < {t.min_years}")
        return report

    m = report.metrics
    roe = _ratio(data.get("net_income"), data.get("equity"))
    equity = data.get("equity") or []
    gross = _ratio(data.get("gross_profit"), revenue)
    opm = _ratio(data.get("operating_income"), revenue)
    fcf_ni = _ratio(data.get("fcf"), data.get("net_income"))
    fcf = [v for v in (data.get("fcf") or []) if v is not None]
    debt = data.get("total_debt") or []
    ebit = data.get("ebit") or []
    interest = data.get("interest_expense") or []

    positive_roe = [
        n / e
        for n, e in zip(data.get("net_income") or [], equity, strict=False)
        if n is not None and e is not None and e > 0
    ]
    if positive_roe:
        m["roe_min"] = min(positive_roe)
        if m["roe_min"] < t.min_roe:
            report.failed.append(f"ROE 最小 {m['roe_min']:.0%} < {t.min_roe:.0%}")
    elif not roe:
        report.failed.append("ROE 不明（欠損）")
    # 自己資本がマイナス（自社株買いの結果）は ROE で判定できない。負債の
    # 代替条件（純有利子負債 ÷ EBIT）に委ねる

    if gross:
        m["gross_margin_min"] = min(gross)
        if m["gross_margin_min"] < t.min_gross_margin:
            report.failed.append(
                f"粗利率 最小 {m['gross_margin_min']:.0%} < {t.min_gross_margin:.0%}"
            )
    else:
        report.failed.append("粗利率 不明")

    if opm:
        m["operating_margin"] = opm[0]
        if opm[0] < t.min_operating_margin:
            report.failed.append(f"営業利益率 {opm[0]:.0%} < {t.min_operating_margin:.0%}")
    else:
        report.failed.append("営業利益率 不明")

    cash = data.get("cash") or []
    net_debt_to_ebit = None
    if debt and ebit and debt[0] is not None and ebit[0] and ebit[0] > 0:
        net_debt_to_ebit = (debt[0] - (cash[0] or 0.0 if cash else 0.0)) / ebit[0]
        m["net_debt_to_ebit"] = net_debt_to_ebit
    leverage_ok = net_debt_to_ebit is not None and net_debt_to_ebit < t.max_net_debt_to_ebit
    if debt and equity and debt[0] is not None and equity[0] and equity[0] > 0:
        m["debt_to_equity"] = debt[0] / equity[0]
        if m["debt_to_equity"] >= t.max_debt_to_equity and not leverage_ok:
            report.failed.append(f"D/E {m['debt_to_equity']:.2f} ≥ {t.max_debt_to_equity}")
    elif equity and equity[0] is not None and equity[0] <= 0 and not leverage_ok:
        report.failed.append("自己資本がマイナス")

    if ebit and interest and ebit[0] is not None:
        expense = interest[0]
        if expense is None or expense == 0:
            m["interest_coverage"] = math.inf  # 無借金
        else:
            m["interest_coverage"] = ebit[0] / abs(expense)
            if m["interest_coverage"] <= t.min_interest_coverage:
                report.failed.append(
                    f"利払い余力 {m['interest_coverage']:.1f} ≤ {t.min_interest_coverage}"
                )

    if fcf_ni:
        m["fcf_to_net_income"] = fcf_ni[0]
        if fcf_ni[0] < t.min_fcf_to_net_income:
            report.failed.append(f"FCF/純利益 {fcf_ni[0]:.2f} < {t.min_fcf_to_net_income}")
    else:
        report.failed.append("FCF 不明")

    if len(fcf) >= 2 and fcf[-1] > 0:
        span = len(fcf) - 1
        m["fcf_growth"] = (fcf[0] / fcf[-1]) ** (1 / span) - 1 if fcf[0] > 0 else -1.0
        if m["fcf_growth"] <= t.min_fcf_growth:
            report.failed.append(f"FCF 成長 {m['fcf_growth']:.0%} ≤ {t.min_fcf_growth:.0%}")
    elif fcf:
        report.failed.append("FCF 成長 不明（起点がマイナス）")

    return report


class FundamentalsStore:
    """財務データのキャッシュ。yfinance は連投で弾かれるので必ず保存する。"""

    def __init__(self, root: Path) -> None:
        self.root = Path(root)

    def path_for(self, symbol: str) -> Path:
        return self.root / f"{symbol.replace('/', '_')}.json"

    def load(self, symbol: str) -> dict[str, Any] | None:
        path = self.path_for(symbol)
        if not path.exists():
            return None
        return json.loads(path.read_text(encoding="utf-8"))

    def get(self, symbol: str, *, refresh: bool = False) -> dict[str, Any] | None:
        """キャッシュを返す。無ければ取りに行く。取れなければ None。"""
        if not refresh:
            cached = self.load(symbol)
            if cached is not None:
                return cached
        try:
            data = fetch_statements(symbol)
        except Exception as exc:  # yfinance は例外の型が安定しない
            log.warning("financials_fetch_failed", symbol=symbol, error=str(exc))
            return None
        if not data.get("revenue"):
            log.warning("financials_empty", symbol=symbol)
            return None
        self.root.mkdir(parents=True, exist_ok=True)
        self.path_for(symbol).write_text(json.dumps(data), encoding="utf-8")
        return data
