"""積立の設定（``config/accum/accum.toml``）。戦略と銘柄の対応を持つ。

スイング売買の設定（``config/<dir>/settings.toml``）とは形が違う。
あちらは「銘柄ユニバースに対して全戦略が意見を出し、合成して目標建玉を
決める」。積立は逆で、**銘柄ごとに戦略が1つ決まる**。

**1銘柄に複数の戦略を割り当てるのは既定で禁止**

実運用では二重に買い付けることになり、予算が意図せず倍になる。
比較検証をしたい場合は :func:`load` に ``allow_overlap=True`` を渡す。
"""

from __future__ import annotations

import tomllib
from collections import defaultdict
from decimal import Decimal
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field, field_validator

from accum.basket import DrawdownTilt, WeightSchedule
from accum.registry import create
from accum.tactics import Tactic
from wbcore.domain.models import Market, TaxAccountType

#: 設定ファイル名。
FILENAME = "accum.toml"

#: 既定の設定ディレクトリ。スイング売買の ``config/`` と分けてある。
DEFAULT_CONFIG_DIR = Path("config/accum")

_TACTIC_RESERVED = frozenset(
    {"id", "tactic", "symbols", "enabled", "market", "signal_symbol", "signal_market"}
)


class TacticEntry(BaseModel):
    """1つの戦略と、それを適用する銘柄（``[[tactics]]``）。

    Attributes:
        id: 比較表の行名になる自由なラベル。日本語でよい。
        tactic: 登録簿の鍵（``bear_stack`` など）。機構の名前。
        symbols: この戦略で積み立てる銘柄コード。
        market: 銘柄の市場。足を取りに行くときのティッカー変換と、発注先の
            決定に使う（日本株の ``1305`` は ``1305.T``、米国株の ``VOO`` はそのまま）。
            ``^`` で始まる指数は市場に関係なくそのまま扱われる（発注はできない）。
        signal_symbol: 倍率の判定に使う別の銘柄。省略すれば買う銘柄自身の足で
            判定する。東証の S&P500 連動 ETF を ``^GSPC`` で判定する、といった使い方。
            判定にしか使わないので指数でもよい。
        signal_market: 判定用銘柄の市場。省略すれば ``market`` と同じ。
    """

    model_config = {"extra": "allow"}  # 戦略固有パラメータを受け取る

    id: str
    tactic: str
    symbols: list[str] = Field(min_length=1)
    enabled: bool = True
    market: Market = Market.JP
    signal_symbol: str | None = None
    signal_market: Market | None = None

    @property
    def signal_market_resolved(self) -> Market:
        return self.signal_market or self.market

    @property
    def signal_lags(self) -> bool:
        """判定用の足を 1 日遅らせるか（判定用の市場の引けが買う市場より後のとき）。"""
        from wbcore.domain.session import closes_after

        return self.signal_symbol is not None and closes_after(
            self.signal_market_resolved, self.market
        )

    @field_validator("symbols")
    @classmethod
    def _clean(cls, value: list[str]) -> list[str]:
        cleaned = [s.strip() for s in value if s.strip()]
        if not cleaned:
            raise ValueError("symbols が空です")
        duplicates = {s for s in cleaned if cleaned.count(s) > 1}
        if duplicates:
            raise ValueError(f"同じ銘柄が重複しています: {sorted(duplicates)}")
        return cleaned

    @property
    def params(self) -> dict[str, Any]:
        """戦略コンストラクタに渡す固有パラメータ。"""
        extra = self.__pydantic_extra__ or {}
        return {k: v for k, v in extra.items() if k not in _TACTIC_RESERVED}

    def build(self) -> Tactic:
        """戦略インスタンスを組み立てる。"""
        try:
            return create(self.tactic, self.params)
        except ValueError as exc:
            raise ValueError(f"[{self.id}] {exc}") from None


class BasketEntry(BaseModel):
    """複数銘柄への配分（``[[baskets]]``、:mod:`accum.basket`）。

    Attributes:
        id: 表の行名。
        source: ``"static"`` は ``weights`` をそのまま使う。``"13f"`` は
            EDGAR の 13F（``cik`` の運用会社）から四半期ごとの比率を作る。
        weights: ``source = "static"`` の配分。``13f`` のときはコア（固定部分）
            として使い、``satellite_share`` の残りをこれに振る。
        cik: 13F を取る運用会社。既定はバークシャー。
        top: 13F の上位何銘柄を採るか。
        satellite_share: 13F 部分の比率。``weights`` が空なら 1。
        benchmark: 同じ資金の流れを投じて比較する銘柄。
        monthly_budget: バスケット全体の毎月の予算。省略時は共通設定。
        tactic: 各銘柄に掛ける倍率戦略と、その固有パラメータ。
        tilt_strength: 高値からの下落率に応じて配分を寄せる強さ。0 なら無効。
        tilt_lookback: 下落率を測る高値の期間（足の本数）。
    """

    model_config = {"extra": "allow"}

    id: str
    source: str = "static"
    weights: dict[str, float] = Field(default_factory=dict)
    cik: str = "0001067983"
    top: int = Field(default=15, ge=1)
    satellite_share: float = Field(default=1.0, gt=0, le=1)
    benchmark: str | None = "VOO"
    monthly_budget: Decimal | None = None
    tactic: str = "constant"
    tilt_strength: float = Field(default=0.0, ge=0)
    tilt_lookback: int = Field(default=252, ge=2)
    enabled: bool = True
    market: Market = Market.US

    def build_tilt(self) -> DrawdownTilt | None:
        if self.tilt_strength <= 0:
            return None
        return DrawdownTilt(self.tilt_strength, self.tilt_lookback)

    @field_validator("source")
    @classmethod
    def _source(cls, value: str) -> str:
        if value not in ("static", "13f"):
            raise ValueError(f"source は static か 13f: {value!r}")
        return value

    @field_validator("weights")
    @classmethod
    def _weights(cls, value: dict[str, float]) -> dict[str, float]:
        bad = {k: v for k, v in value.items() if v <= 0}
        if bad:
            raise ValueError(f"比率は正の値: {bad}")
        return value

    @property
    def params(self) -> dict[str, Any]:
        extra = self.__pydantic_extra__ or {}
        return {k: v for k, v in extra.items() if k not in _BASKET_RESERVED}

    def build_tactic(self) -> Tactic:
        try:
            return create(self.tactic, self.params)
        except ValueError as exc:
            raise ValueError(f"[{self.id}] {exc}") from None

    def build_schedule(
        self, schedule_13f: list[tuple[Any, dict[str, float]]] | None = None
    ) -> WeightSchedule:
        """配分表を組み立てる。``13f`` のときは取得済みの比率列を渡す。"""
        if self.source == "static":
            if not self.weights:
                raise ValueError(f"[{self.id}] static には weights が必要です")
            return WeightSchedule.static(self.weights)
        if not schedule_13f:
            raise ValueError(f"[{self.id}] 13F の保有一覧がありません（`accum sync-13f` を実行）")
        schedule = WeightSchedule.from_pairs(schedule_13f)
        if self.weights:
            return schedule.blend(self.weights, self.satellite_share)
        return schedule


_BASKET_RESERVED = frozenset(BasketEntry.model_fields)


class ExecutionConfig(BaseModel):
    """発注の振る舞い（``[execution]``）。

    積立は当日の投下額を成行の買いにする。指値にすると約定しない日が出て、
    「毎月同じ日に確実に買う」という積立の前提が崩れるため。
    """

    model_config = {"extra": "forbid"}

    #: 発注先。:data:`wbcore.broker.registry.BROKERS` の名前（webull / paper / …）。
    broker: str = "webull"
    #: GENERAL（一般） / SPECIFIC（特定） / NISA
    tax_account_type: TaxAccountType = TaxAccountType.SPECIFIC
    #: 米国株の時間外取引を許すか。日本株では無視される。
    extended_hours: bool = False
    #: 売買単位が既定と異なる銘柄の例外 {設定に書いた銘柄コード: 単元株数}。
    #: ETF には 1 株や 10 株単位のものがある。既定の 100 株だと月の予算が
    #: 1単元に届かず、発注が丸ごと見送りになる。
    lot_size_overrides: dict[str, int] = Field(default_factory=dict)


class AccumConfig(BaseModel):
    """``config/accum/accum.toml`` の内容。"""

    model_config = {"extra": "forbid"}

    monthly_budget: Decimal = Decimal(25_000)
    """1銘柄あたりの毎月の基本予算（口座通貨）。比較の前提を揃えるため全戦略で共通。"""

    #: true で全発注を即停止する。スイング売買の ``risk.kill_switch`` と同じ役割。
    kill_switch: bool = False

    #: 足データの取得元。:data:`wbcore.data.registry.PROVIDERS` の名前。
    #: 積立は市場をまたぐので、両市場に対応する取得元（yfinance）が既定。
    data_provider: str = "yfinance"

    execution: ExecutionConfig = Field(default_factory=ExecutionConfig)

    tactics: list[TacticEntry] = Field(default_factory=list)
    baskets: list[BasketEntry] = Field(default_factory=list)
    """複数銘柄への配分。戦略と違い1銘柄が複数のバスケットに現れてもよい
    （バスケットは比較検証が主用途で、実発注は id を指定して行う）。"""

    @property
    def active(self) -> list[TacticEntry]:
        return [t for t in self.tactics if t.enabled]

    @property
    def active_baskets(self) -> list[BasketEntry]:
        return [b for b in self.baskets if b.enabled]

    def validate_assignment(self, *, allow_overlap: bool = False) -> None:
        """id の重複と、1銘柄への複数割り当てを検出する。

        Raises:
            ValueError: id が重複、または ``allow_overlap`` が False のときに
                同じ銘柄が複数の戦略に現れた場合。
        """
        ids = [t.id for t in self.tactics] + [b.id for b in self.baskets]
        dup_ids = sorted({i for i in ids if ids.count(i) > 1})
        if dup_ids:
            raise ValueError(f"id が重複しています: {dup_ids}")

        if allow_overlap:
            return
        owners: dict[str, list[str]] = defaultdict(list)
        for entry in self.active:
            for symbol in entry.symbols:
                owners[symbol].append(entry.id)
        conflicts = {s: v for s, v in owners.items() if len(v) > 1}
        if conflicts:
            detail = "、".join(f"{s} → {v}" for s, v in sorted(conflicts.items()))
            raise ValueError(
                f"1銘柄に複数の戦略が割り当てられています（二重買付になります）: {detail}"
            )

    def build(self) -> dict[str, Tactic]:
        """``銘柄 → 戦略`` に展開する。設定の主用途はこれ。"""
        return {s: entry.build() for entry in self.active for s in entry.symbols}

    def tactic_for(self, symbol: str) -> Tactic | None:
        """銘柄に割り当てられた戦略。無ければ None。"""
        return self.build().get(symbol)

    def market_of(self, symbol: str) -> Market | None:
        """銘柄の市場。有効な戦略に割り当てられていなければ None。"""
        for entry in self.active:
            if symbol in entry.symbols:
                return entry.market
        return None

    def symbols_by_market(self) -> dict[Market, list[str]]:
        """市場 → 銘柄（判定用の銘柄も含む）。足の取得はティッカー変換が市場ごとに違うので分ける。

        買う銘柄が複数の戦略に現れないことは :meth:`validate_assignment` が
        保証している。判定用の銘柄は複数の戦略で共有してよい。
        """
        grouped: dict[Market, list[str]] = defaultdict(list)
        for entry in self.active:
            grouped[entry.market].extend(entry.symbols)
            if entry.signal_symbol:
                grouped[entry.signal_market_resolved].append(entry.signal_symbol)
        return {market: sorted(set(symbols)) for market, symbols in grouped.items()}

    @property
    def symbols(self) -> list[str]:
        """有効な戦略で**買う**銘柄の一覧。判定用は含まない。"""
        return sorted({s for entry in self.active for s in entry.symbols})

    @property
    def all_symbols(self) -> list[str]:
        """足が要る銘柄すべて（買う銘柄 ＋ 判定用の銘柄）。"""
        signals = {e.signal_symbol for e in self.active if e.signal_symbol}
        return sorted(set(self.symbols) | signals)


def load(
    config_dir: Path | str = DEFAULT_CONFIG_DIR, *, allow_overlap: bool = False
) -> AccumConfig:
    """``accum.toml`` を読む。

    Args:
        config_dir: 設定ディレクトリ。
        allow_overlap: 1銘柄に複数の戦略を許すか。比較検証のときだけ True。

    Raises:
        FileNotFoundError: 設定ファイルが無いとき。
        ValueError: 内容が不正なとき。
    """
    path = Path(config_dir) / FILENAME
    if not path.is_file():
        raise FileNotFoundError(f"積立の設定が見つかりません: {path}")
    with path.open("rb") as fh:
        raw = tomllib.load(fh)
    config = AccumConfig.model_validate(raw)
    config.validate_assignment(allow_overlap=allow_overlap)
    return config
