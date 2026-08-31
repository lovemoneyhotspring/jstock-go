"""デイトレの設定（``config/daytrade/daytrade.toml``）。

資金の上限から銘柄数 N を決めるのが要（:func:`daytrade.fees.positions_for`）。
「N をいくつにするか」を人が書くと、資金を変えたときに手数料段階と合わなくなる。
"""

from __future__ import annotations

import datetime as dt
import tomllib
from decimal import ROUND_DOWN, Decimal
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field, field_validator

from daytrade.fees import positions_for
from wbcore.domain.models import TaxAccountType
from wbcore.domain.session import parse_time

#: 設定ファイル名。
FILENAME = "daytrade.toml"

#: 既定の設定ディレクトリ。
DEFAULT_CONFIG_DIR = Path("config/daytrade")

#: 市場区分の呼び方（``universe.segments`` に書く値）。
SEGMENTS = ("prime", "standard", "growth")


class CapitalConfig(BaseModel):
    """資金（``[capital]``、円）と戦略のスイッチ。"""

    model_config = {"extra": "forbid"}

    #: 戦略のオン／オフ。false なら ``plan`` / ``open`` は何もしない（``close`` は台帳に
    #: 当日の買いが残っていれば売る——止めた日に建玉を持ち越さないため）。
    enabled: bool = True
    #: 1 日に使う資金の上限。この額で N 銘柄を等金額に分ける。
    #: **0 なら N = 0**: スクリーニングと候補の表示はするが買わない（様子見モード）。
    max_capital: Decimal = Decimal(2_000_000)
    #: 1 注文の目安。N = max_capital ÷ order_budget（切り捨て）。
    #: 67 万円は「20〜100 万円は一律 275 円」の手数料段階と分散のバランス（研究の結論）。
    order_budget: Decimal = Decimal(670_000)
    #: N の上限。研究では N10 を超えると Sharpe が下がった。
    max_positions: int = 10

    @field_validator("order_budget")
    @classmethod
    def _positive(cls, v: Decimal) -> Decimal:
        if v <= 0:
            raise ValueError("正の値")
        return v

    @field_validator("max_capital")
    @classmethod
    def _non_negative(cls, v: Decimal) -> Decimal:
        if v < 0:
            raise ValueError("0 以上（0 は「買わない」）")
        return v

    @property
    def positions(self) -> int:
        """この資金で持つ銘柄数 N。資金 0 なら 0。"""
        if self.max_capital == 0:
            return 0
        return positions_for(self.max_capital, self.order_budget, self.max_positions)

    @property
    def budget_per_order(self) -> Decimal:
        """1 注文の予算（``max_capital ÷ N``、円未満切り捨て）。N = 0 なら 0。"""
        n = self.positions
        if n == 0:
            return Decimal(0)
        return (self.max_capital / n).quantize(Decimal(1), rounding=ROUND_DOWN)


class UniverseConfig(BaseModel):
    """母集団（``[universe]``）。前夜に確定する条件だけを置く。"""

    model_config = {"extra": "forbid"}

    #: 市場区分。prime / standard / growth。2022-04 以前の一部・二部・マザーズも同じ呼び方で扱う。
    segments: list[str] = Field(default_factory=lambda: ["prime"])
    #: 売買代金（円）の ``turnover_days`` 日中央値がこれ以上。
    min_turnover: Decimal = Decimal(100_000_000)
    turnover_days: int = 20
    #: 時価総額を 3 分位に分け、下位からいくつ外すか（0=外さない、1=下位 1/3 を外す、2=上位 1/3 のみ）。
    #: 分位は市場区分で絞る前の全銘柄で切る（研究と同じ）。
    exclude_cap_terciles: int = 1
    #: 前日引け後に決算短信を開示した銘柄を外す（決算翌日はギャップの符号が反転する）。
    exclude_earnings_prev: bool = True
    #: 当日に決算発表の予定がある銘柄を外す（場中開示は宝くじ）。
    exclude_earnings_today: bool = True
    #: 前日に日々公表信用残の対象だった銘柄を外す（全条件で負ける）。
    exclude_margin_alert: bool = True

    @field_validator("segments")
    @classmethod
    def _known_segments(cls, v: list[str]) -> list[str]:
        unknown = sorted(set(v) - set(SEGMENTS))
        if unknown:
            raise ValueError(f"segments に未知の値: {unknown}（使えるのは {list(SEGMENTS)}）")
        if not v:
            raise ValueError("segments が空です")
        return v

    @field_validator("exclude_cap_terciles")
    @classmethod
    def _tercile_range(cls, v: int) -> int:
        if not 0 <= v <= 2:
            raise ValueError("exclude_cap_terciles は 0〜2")
        return v


class SignalConfig(BaseModel):
    """9:00 に決める条件（``[signal]``）。"""

    model_config = {"extra": "forbid"}

    #: ギャップ（寄付 ÷ 前日終値 − 1）がこれ**未満**の銘柄だけ。0 なら「ギャップダウン」。
    max_gap: Decimal = Decimal(0)
    #: ギャップの下限（これ**以上**）。−1 なら制限なし。研究では下限を切るほど悪化した。
    min_gap: Decimal = Decimal(-1)

    @field_validator("max_gap", "min_gap")
    @classmethod
    def _range(cls, v: Decimal) -> Decimal:
        if not Decimal(-1) <= v <= Decimal(1):
            raise ValueError("ギャップは −1〜1 の比率で書く")
        return v


class RegimeConfig(BaseModel):
    """危険信号（``[regime]``）。詳細と検証は :mod:`daytrade.regime` と研究ノート。"""

    model_config = {"extra": "forbid"}

    #: 日経 225 オプションの IV（前日の ``BaseVol`` 中央値）がこれを超える日だけ取引する。
    #: 0 なら常時。18 で CAGR をほぼ保ったまま最大 DD が半分になった（研究）。
    iv_gate: Decimal = Decimal(0)
    #: 取引しない月（1〜12）。12 月は 9 年中 7 年がマイナスで、IS/OOS ともに外す方が良かった。
    skip_months: list[int] = Field(default_factory=list)
    #: 市場の日中ドリフト（TOPIX 寄り→引けの平均）を取る日数。
    drift_days: int = 20
    #: ドリフトがこれ以下（比率。−0.0003 = −3 bp）なら取引しない。None で無効。
    #: 2018・2021 年を黒字にするが 2022 年以降の利益を 3 割削る（IS 限定の効き）。既定は無効
    drift_gate: Decimal | None = None
    #: 市場ギャップ（候補の中央値ギャップ）の絶対値がこれを超える日はドリフトのゲートを無視する。
    drift_gap_override: Decimal = Decimal("0.01")
    #: 戦略自身の直近 N 日の損益が 0 以下なら取引しない。0 で無効。
    equity_curve_days: int = 0
    #: 前夜の S&P500 の終値リターンが ``[us_skip_low, us_skip_high)`` にあれば休む。
    #: ``us_skip_high`` が None で無効。研究の既定は 0〜+1%。
    us_skip_low: Decimal = Decimal(0)
    us_skip_high: Decimal | None = None
    #: VIX がこれを超えていれば米国のゲートを無視する（高ボラ局面は取引する）。
    us_vix_override: Decimal = Decimal(24)

    @field_validator("skip_months")
    @classmethod
    def _months(cls, v: list[int]) -> list[int]:
        bad = [m for m in v if not 1 <= m <= 12]
        if bad:
            raise ValueError(f"skip_months は 1〜12: {bad}")
        return sorted(set(v))

    @field_validator("us_skip_high")
    @classmethod
    def _band(cls, v: Decimal | None, info: Any) -> Decimal | None:
        low = info.data.get("us_skip_low", Decimal(0))
        if v is not None and v <= low:
            raise ValueError("us_skip_high は us_skip_low より大きい値")
        return v

    @field_validator("drift_days", "equity_curve_days")
    @classmethod
    def _non_negative(cls, v: int) -> int:
        if v < 0:
            raise ValueError("0 以上")
        return v


class ExecutionConfig(BaseModel):
    """発注の振る舞い（``[execution]``）。"""

    model_config = {"extra": "forbid"}

    broker: str = "webull"
    tax_account_type: TaxAccountType = TaxAccountType.SPECIFIC
    #: 9:00 の気配の取得元。webull / yfinance / csv（:mod:`daytrade.quotes`）。
    quote_source: str = "webull"
    #: csv のときの置き場（``symbol,price`` の CSV）。
    quote_file: Path | None = None
    #: 寄付買いを出してよい時間帯（JST）。外なら何もしない。
    entry_window: tuple[str, str] = ("09:00", "09:15")
    #: 手仕舞いの成行売りを出してよい時間帯（JST）。15:25 以降の注文はクロージング・
    #: オークションに回り引け値で約定する。それより前の成行はその場の気配で即約定する
    #: （引け値より少し早い値になるが、確実に手仕舞える方を取る）。
    exit_window: tuple[str, str] = ("15:20", "15:30")
    kill_switch: bool = False
    #: 気配のタイムスタンプがこれより古ければ使わない（秒）。yfinance は 20 分遅れなので通らない。
    max_quote_age: int = 90

    @field_validator("entry_window", "exit_window")
    @classmethod
    def _window(cls, v: tuple[str, str]) -> tuple[str, str]:
        start, end = parse_time(v[0]), parse_time(v[1])
        if start >= end:
            raise ValueError(f"時間帯の開始が終了より後です: {v}")
        return v

    def window(self, name: str) -> tuple[dt.time, dt.time]:
        raw = self.entry_window if name == "entry" else self.exit_window
        return parse_time(raw[0]), parse_time(raw[1])


class DaytradeConfig(BaseModel):
    model_config = {"extra": "forbid"}

    capital: CapitalConfig = Field(default_factory=CapitalConfig)
    universe: UniverseConfig = Field(default_factory=UniverseConfig)
    signal: SignalConfig = Field(default_factory=SignalConfig)
    regime: RegimeConfig = Field(default_factory=RegimeConfig)
    execution: ExecutionConfig = Field(default_factory=ExecutionConfig)


def load(config_dir: Path) -> DaytradeConfig:
    """設定を読む。

    Raises:
        FileNotFoundError: ファイルが無い。
        ValueError: 内容が不正（未知の項目・範囲外）。
    """
    path = Path(config_dir) / FILENAME
    if not path.is_file():
        raise FileNotFoundError(f"デイトレの設定が見つかりません: {path}")
    with path.open("rb") as handle:
        raw: dict[str, Any] = tomllib.load(handle)
    try:
        config = DaytradeConfig.model_validate(raw)
        _ = config.capital.positions  # 資金と目安の整合をここで確かめる
    except ValueError as exc:
        raise ValueError(f"{path}: {exc}") from None
    return config
