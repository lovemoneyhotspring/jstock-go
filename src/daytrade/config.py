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
    #: N 銘柄への配分。"equal"（等金額）か "inverse_vol"（20 日ボラの逆数で按分。荒い銘柄を少なく、
    #: 穏やかな銘柄を多く持つ。研究では利益 +8%・Sharpe 1.81→2.02 で DD は同じ）。
    weighting: str = "inverse_vol"
    #: 資金が増えたら「順位順に 1 銘柄の上限まで詰め込み、余りを次点へ」（稼働 96%、利益 +9%）を
    #: 再検討する。DD が資金を減らす局面では等分の方が安全なので、今は固定 N。

    @field_validator("weighting")
    @classmethod
    def _weighting(cls, v: str) -> str:
        if v not in {"equal", "inverse_vol"}:
            raise ValueError(f"weighting は equal か inverse_vol: {v}")
        return v

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


class MarginConfig(BaseModel):
    """信用売り（ショート）の資金と条件（``[margin]``）。``jp_gap_fade_margin`` 専用。

    ``[capital]`` のロング側と対になる、ショート側の資金枠。銘柄選定はロングと
    対称（ギャップの符号を反転し、貸借銘柄に限定）。資金配分はロング側の
    資産曲線ゲート（``regime.equity_curve_days`` / ``equity_curve_scale``）に
    連動させる：ロング側が縮小した日（地合いが弱い日）ほどショート側を
    ``long_weak_multiplier`` 倍に増強する「シーソー」。
    """

    model_config = {"extra": "forbid"}

    #: ショートのオン／オフ。false なら ``jp_gap_fade`` と同じ動き（ロングのみ）。
    enabled: bool = False
    #: 保証金として差し入れる現金（円）。信用取引では建玉（``capital.max_capital`` +
    #: ``margin.max_capital``）が現金より大きくなるため、年率や DD はこれに対して見る。
    #: 0 なら ``capital.max_capital`` を現金とみなす。
    cash: Decimal = Decimal(0)
    #: 1 日に使う資金の上限（円）。``[capital]`` と同じ意味だがショート専用の枠。
    max_capital: Decimal = Decimal(0)
    order_budget: Decimal = Decimal(670_000)
    max_positions: int = 10
    weighting: str = "inverse_vol"
    # --- ショート専用の母集団（``[universe]`` はロング用。研究ノート 2026-09-jp-gap-up-short） ---
    #: 市場区分。プライムは張り付き（引けストップ高で返済できない）率 2.1%、グロース 5.3%・
    #: スタンダード 5.8%。全区分の方が利益は大きいが持ち越しの損失で OOS が崩れる。
    segments: list[str] = Field(default_factory=lambda: ["prime"])
    #: 売買代金の ``universe.turnover_days`` 日中央値がこれ以上。上げるほど悪くなる（ロングと同じ）。
    min_turnover: Decimal = Decimal(100_000_000)
    #: 時価総額の下位からいくつ外すか。ショートは小型に効きが厚いので既定は外さない。
    exclude_cap_terciles: int = 0
    #: 決算翌日のギャップアップは順張り（−30 bp/取引）なので必ず外す。
    exclude_earnings_prev: bool = True
    exclude_earnings_today: bool = True
    #: 日々公表・注意喚起・増担保の銘柄を外すか。建てられる規制銘柄に効きの差は無いので既定は外さない。
    exclude_margin_alert: bool = False
    #: 日証金の申込停止（売り禁）銘柄を外す。新規売りが出せないので発注しても拒否される。
    exclude_jsf_stop: bool = True
    #: ギャップ（寄付 ÷ 前日終値 − 1）がこれ**以上**の銘柄だけ（ロングの逆）。
    #: 5〜7% は +29 bp/取引で薄く、7〜10% +79、10〜15% +112、15% 以上 +304。大きい順に取る。
    min_gap: Decimal = Decimal("0.05")
    #: ギャップの上限（これ**未満**）。1 なら実質上限なし。
    max_gap: Decimal = Decimal(1)
    #: 9:00 の気配がストップ高の銘柄は売らない（踏み上げの初動を売る危険を避ける。
    #: ``signal.skip_limit_down`` の逆）。
    skip_limit_up: bool = True
    #: ショート側の資金の倍率（シーソー）。``multiplier_normal`` はロング側が通常運転の日、
    #: ``multiplier_long_weak`` はロング側が資産曲線で縮小された日（地合いが弱い日）。
    #: 既定は常時 1.0（弱い日限定は Sharpe が高いが稼働が 1/3 に減る）。
    multiplier_normal: Decimal = Decimal(1)
    multiplier_long_weak: Decimal = Decimal(1)
    #: 検証のみ: 引けがストップ高（返済買いが約定しない）の取引を「翌営業日の寄付で返済」として
    #: 計上する係数。1 で全額（張り付きは全て約定しない）、0 で無視。実態は 0〜1 の間で、
    #: 運用で約定率を測って決める。研究では張り付き 5.4%・翌朝までさらに −425 bp。
    carry_penalty: Decimal = Decimal(1)
    #: ロング側の資産曲線による縮小（``regime.equity_curve_scale``）を効かせるか。
    #: false にすると、合図（直近 N 日の損益 ≤ 0）はショートのシーソーにだけ使い、
    #: ロングは縮めない——ドローダウンをショートで受け止め、買いは利益を追う設計。
    long_shrink: bool = True
    #: ショートの往復コスト（bp、約定代金に対して）。立花証券は信用手数料 0 円で、
    #: 貸株料 年 1.15% の日計り 1 日分 ≈ 0.3 bp。残りは滑り（板の厚さ）の見込み。
    extra_cost_bp: Decimal = Decimal(5)
    #: ロング側も信用買い（日計り）で建てる。手数料 0 円になり、代わりに金利
    #: （年 2.50% の 1 日分 ≈ 0.7 bp）を ``long_extra_cost_bp`` で見る。
    long_via_margin: bool = False
    long_extra_cost_bp: Decimal = Decimal(5)

    @field_validator("weighting")
    @classmethod
    def _weighting(cls, v: str) -> str:
        if v not in {"equal", "inverse_vol"}:
            raise ValueError(f"weighting は equal か inverse_vol: {v}")
        return v

    @field_validator(
        "cash",
        "max_capital",
        "extra_cost_bp",
        "long_extra_cost_bp",
        "multiplier_normal",
        "multiplier_long_weak",
        "min_turnover",
    )
    @classmethod
    def _non_negative(cls, v: Decimal) -> Decimal:
        if v < 0:
            raise ValueError("0 以上")
        return v

    @field_validator("carry_penalty")
    @classmethod
    def _penalty(cls, v: Decimal) -> Decimal:
        if not Decimal(0) <= v <= Decimal(1):
            raise ValueError("carry_penalty は 0〜1")
        return v

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

    @field_validator("order_budget")
    @classmethod
    def _positive(cls, v: Decimal) -> Decimal:
        if v <= 0:
            raise ValueError("正の値")
        return v

    @field_validator("min_gap", "max_gap")
    @classmethod
    def _range(cls, v: Decimal) -> Decimal:
        if not Decimal(-1) <= v <= Decimal(1):
            raise ValueError("ギャップは −1〜1 の比率で書く")
        return v

    @property
    def positions(self) -> int:
        """この資金で持つ銘柄数 N。無効か資金 0 なら 0。"""
        if not self.enabled or self.max_capital == 0:
            return 0
        return positions_for(self.max_capital, self.order_budget, self.max_positions)

    @property
    def budget_per_order(self) -> Decimal:
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
    #: 9:00 の気配が制限値幅の下限（ストップ安）にある銘柄は買わない。売り殺到の板では
    #: 引けの売りが約定せず持ち越しになる（研究: ストップ安に触れた日は勝率 9%）。
    skip_limit_down: bool = True

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
    #: 戦略自身の直近 N 日の損益が 0 以下なら、資金を ``equity_curve_scale`` 倍に縮める。0 で無効。
    equity_curve_days: int = 0
    #: 縮めた後の倍率。0 なら休む。0.5 で MaxDD −50→−30 万、利益 −2%（研究）。
    equity_curve_scale: Decimal = Decimal("0.5")
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

    @field_validator("equity_curve_scale")
    @classmethod
    def _scale(cls, v: Decimal) -> Decimal:
        if not Decimal(0) <= v <= Decimal(1):
            raise ValueError("equity_curve_scale は 0〜1")
        return v

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

    broker: str = "tachibana"
    tax_account_type: TaxAccountType = TaxAccountType.SPECIFIC
    #: 9:00 の気配の取得元。tachibana / csv（:mod:`daytrade.quotes`）。
    quote_source: str = "tachibana"
    #: csv のときの置き場（``symbol,price`` の CSV）。
    quote_file: Path | None = None
    #: 寄付買いを出してよい時間帯（JST）。外なら何もしない。
    entry_window: tuple[str, str] = ("09:00", "09:15")
    #: 手仕舞いの成行売りを出してよい時間帯（JST）。15:25 以降の注文はクロージング・
    #: オークションに回り引け値で約定する。それより前の成行はその場の気配で即約定する
    #: （引け値より少し早い値になるが、確実に手仕舞える方を取る）。
    exit_window: tuple[str, str] = ("15:20", "15:30")
    kill_switch: bool = False
    #: 気配のタイムスタンプがこれより古ければ使わない（秒）。
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
    margin: MarginConfig = Field(default_factory=MarginConfig)


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
        _ = config.margin.positions  # 同上（ショート側。無効なら 0 のまま素通り）
    except ValueError as exc:
        raise ValueError(f"{path}: {exc}") from None
    return config
