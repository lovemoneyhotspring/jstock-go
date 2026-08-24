"""設定と秘匿情報の管理。

方針:
    - **秘密はリポジトリに置かない。** ローカル開発では OS のキーチェーンに
      保管する。キーチェーンの無いサーバー（ヘッドレスな Linux など）では
      環境変数か、パーミッションを絞った ``.env`` を使う。
    - 環境（uat / prod）ごとに認証情報を完全に分ける。取り違えて本番に
      発注する事故を、名前空間の分離で防ぐ。
    - **実発注には ``WBJP_ENV=prod`` と ``--live`` の両方が要る。**
      片方だけでは必ず dry-run になる。
"""

from __future__ import annotations

import datetime as dt
import os
import stat
import tomllib
from collections.abc import Mapping
from dataclasses import dataclass
from decimal import Decimal
from enum import StrEnum
from pathlib import Path
from typing import Any, Self

import structlog
from dotenv import dotenv_values
from pydantic import BaseModel, Field, field_validator, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

from wbjp.domain.models import TaxAccountType

log = structlog.get_logger(__name__)

#: キーチェーンのサービス名の接頭辞。環境ごとに別エントリになる。
KEYRING_SERVICE_PREFIX = "wbjp"

#: 既定の ``.env`` の場所（プロセスのカレントディレクトリ）。
DEFAULT_ENV_FILE = Path(".env")

#: ``.env`` の場所を絶対パスで上書きする環境変数。
ENV_FILE_OVERRIDE_VAR = "WBJP_ENV_FILE"

#: 認証情報を構成する項目。
_CREDENTIAL_FIELDS = ("app_key", "app_secret", "account_id")

#: Webull の API キーは既定でこの日数で失効する。
API_KEY_VALIDITY_DAYS = 45

#: 失効まで何日を切ったら警告するか。
API_KEY_EXPIRY_WARN_DAYS = 7


class Environment(StrEnum):
    UAT = "uat"
    PROD = "prod"

    @property
    def is_production(self) -> bool:
        return self is Environment.PROD


@dataclass(frozen=True, slots=True)
class Endpoints:
    """環境ごとの接続先。"""

    trade: str
    events: str
    market_data: str


#: 出典: https://developer.webull.co.jp/apis/docs/sdk.md
ENDPOINTS: dict[Environment, Endpoints] = {
    Environment.UAT: Endpoints(
        trade="jp-openapi-alb.uat.webullbroker.com",
        events="jp-openapi-events.uat.webullbroker.com",
        market_data="data-api.uat.webullbroker.com",
    ),
    Environment.PROD: Endpoints(
        trade="api.webull.co.jp",
        events="events-api.webull.co.jp",
        market_data="data-api.webull.co.jp",
    ),
}

#: Webull が公式ドキュメントで公開している UAT 用の共有テスト口座。
#: 誰でも使えるため残高・建玉は常に他人によって変動する。疎通確認専用。
#: 本番環境では絶対に使われない（:func:`load_credentials` で分岐）。
PUBLIC_UAT_CREDENTIALS = {
    "app_key": "209fffb82d4e62b60d167b7b9c55e163",
    "app_secret": "af02275fc2e9cfccd3745c85f48b40cd",
    "account_id": "1241489592734023680",
}


class MissingCredentialsError(RuntimeError):
    """認証情報が見つからない。"""


@dataclass(frozen=True, slots=True)
class Credentials:
    """Webull API の認証情報。

    ``__repr__`` を潰してあるので、うっかりログや例外に出しても
    秘密が漏れない。
    """

    app_key: str
    app_secret: str
    account_id: str
    created_on: dt.date | None = None
    is_public_test_account: bool = False

    def __repr__(self) -> str:
        return f"Credentials(app_key='***{self.app_key[-4:]}', account_id='***')"

    __str__ = __repr__

    @property
    def expires_on(self) -> dt.date | None:
        """キーの失効予定日。生成日が未設定なら None。"""
        if self.created_on is None:
            return None
        return self.created_on + dt.timedelta(days=API_KEY_VALIDITY_DAYS)

    def days_until_expiry(self, today: dt.date | None = None) -> int | None:
        expires = self.expires_on
        if expires is None:
            return None
        return (expires - (today or dt.date.today())).days


# --------------------------------------------------------------------------
# 認証情報の解決
# --------------------------------------------------------------------------


def _keyring_service(env: Environment) -> str:
    return f"{KEYRING_SERVICE_PREFIX}/{env.value}"


def _from_keyring(env: Environment) -> dict[str, str | None]:
    try:
        import keyring
    except ImportError:  # pragma: no cover - keyring は必須依存
        return {}

    service = _keyring_service(env)
    try:
        return {name: keyring.get_password(service, name) for name in _CREDENTIAL_FIELDS}
    except Exception as exc:
        # ヘッドレスな Linux サーバーには SecretService も D-Bus も無く、
        # keyring は NoKeyringError を投げる（バックエンド由来の例外が
        # そのまま漏れてくることもある）。ここで落とすと、設定方法を案内する
        # MissingCredentialsError に到達できない。「見つからなかった」として
        # 扱い、他のソースに任せる。
        log.debug("keyring は利用できません", error=str(exc), service=service)
        return {}


def _scoped_lookup(source: Mapping[str, str | None], env: Environment) -> dict[str, str | None]:
    """``WBJP_UAT_APP_KEY`` 形式を優先し、``WBJP_APP_KEY`` を後方互換とする。"""
    prefix = f"WBJP_{env.value.upper()}_"
    return {
        name: source.get(f"{prefix}{name.upper()}") or source.get(f"WBJP_{name.upper()}")
        for name in _CREDENTIAL_FIELDS
    }


def _from_env_vars(env: Environment) -> dict[str, str | None]:
    """環境変数から読む。systemd の ``EnvironmentFile=`` もここに来る。"""
    return _scoped_lookup(os.environ, env)


def _resolve_env_file(env_file: Path | None) -> Path:
    """使う ``.env`` を決める。

    cron は ``$HOME`` で起動するため、相対パスの ``.env`` は見つからない
    （しかも黙って見つからない）。``WBJP_ENV_FILE`` に絶対パスを渡せば
    カレントディレクトリに依存しなくなる。
    """
    if env_file is not None:
        return env_file
    override = os.environ.get(ENV_FILE_OVERRIDE_VAR)
    return Path(override) if override else DEFAULT_ENV_FILE


def _read_dotenv(env_file: Path) -> dict[str, str | None]:
    """``.env`` を読む。**``os.environ`` は汚さない。**

    ``load_dotenv()`` を使わないのは意図的で、あれは値をプロセス環境に
    書き込むため、秘密が子プロセスと ``/proc/<pid>/environ`` に漏れる。
    ``dotenv_values()`` なら呼び出し元の dict に留まる。
    """
    if not env_file.is_file():
        return {}
    _warn_if_readable_by_others(env_file)
    return dict(dotenv_values(env_file))


def _warn_if_readable_by_others(env_file: Path) -> None:
    """``.env`` が自分以外から読めるなら警告する。"""
    try:
        mode = env_file.stat().st_mode
    except OSError:  # pragma: no cover - stat が失敗するなら読み込みも失敗する
        return
    if mode & (stat.S_IRWXG | stat.S_IRWXO):
        log.warning(
            "`.env` が他ユーザーから読める状態です。`chmod 600` を推奨します",
            path=str(env_file),
            mode=oct(stat.S_IMODE(mode)),
        )


def load_credentials(
    env: Environment,
    *,
    allow_public_test_account: bool = True,
    env_file: Path | None = None,
) -> Credentials:
    """認証情報を解決する。

    優先順位:
        1. 環境変数（CI・systemd の ``EnvironmentFile=`` 向け）
        2. ``.env``（キーチェーンの無いサーバー向け。0600 にすること）
        3. OS キーチェーン（ローカル開発の既定）
        4. UAT のみ: Webull 公開のテスト口座

    Raises:
        MissingCredentialsError: どこにも見つからないとき。
    """
    dotenv = _read_dotenv(_resolve_env_file(env_file))

    sources = (_from_env_vars(env), _scoped_lookup(dotenv, env), _from_keyring(env))
    resolved: dict[str, str | None] = dict.fromkeys(_CREDENTIAL_FIELDS)
    for source in sources:
        for key, value in source.items():
            if resolved[key] is None and value:
                resolved[key] = value

    created_key = f"WBJP_{env.value.upper()}_KEY_CREATED_ON"
    created_on = _parse_date(os.environ.get(created_key) or dotenv.get(created_key))

    if all(resolved.values()):
        return Credentials(
            app_key=str(resolved["app_key"]),
            app_secret=str(resolved["app_secret"]),
            account_id=str(resolved["account_id"]),
            created_on=created_on,
        )

    # 本番で公開テスト口座にフォールバックすることは絶対にない。
    if env is Environment.UAT and allow_public_test_account:
        return Credentials(
            app_key=PUBLIC_UAT_CREDENTIALS["app_key"],
            app_secret=PUBLIC_UAT_CREDENTIALS["app_secret"],
            account_id=PUBLIC_UAT_CREDENTIALS["account_id"],
            is_public_test_account=True,
        )

    missing = [k for k, v in resolved.items() if not v]
    upper = env.value.upper()
    raise MissingCredentialsError(
        f"{env.value} 環境の認証情報が不足しています: {', '.join(missing)}\n"
        f"  キーチェーンに登録: uv run wbjp credentials set --env {env.value}\n"
        f"  または環境変数:     WBJP_{upper}_APP_KEY 等を設定\n"
        f"  または .env に記載: WBJP_{upper}_APP_KEY=... （chmod 600 のこと）"
    )


def credential_source(env: Environment, *, env_file: Path | None = None) -> str:
    """``app_key`` がどのソースから来たかを人間向けに説明する。

    どこから読まれているか分からないまま本番に発注する事故を防ぐための、
    ``credentials check`` 用の診断。秘密そのものは返さない。
    """
    path = _resolve_env_file(env_file)
    candidates = (
        ("環境変数", _from_env_vars(env)),
        (f"{path}", _scoped_lookup(_read_dotenv(path), env)),
        ("OS キーチェーン", _from_keyring(env)),
    )
    for label, source in candidates:
        if source.get("app_key"):
            return label
    return "公開テスト口座（UAT 共有）"


def store_credentials(env: Environment, creds: Credentials) -> None:
    """認証情報を OS キーチェーンに保存する。"""
    import keyring

    service = _keyring_service(env)
    keyring.set_password(service, "app_key", creds.app_key)
    keyring.set_password(service, "app_secret", creds.app_secret)
    keyring.set_password(service, "account_id", creds.account_id)


def _parse_date(value: str | None) -> dt.date | None:
    if not value:
        return None
    try:
        return dt.date.fromisoformat(value)
    except ValueError:
        return None


# --------------------------------------------------------------------------
# ファイル設定（config/*.toml）
# --------------------------------------------------------------------------


class RiskConfig(BaseModel):
    """リスク上限。発注前に全項目をチェックする。"""

    model_config = {"extra": "forbid"}

    #: true で全発注を即停止する。ファイル1行で止められる緊急停止装置。
    kill_switch: bool = False
    #: 1注文あたりの最大約定代金（円）
    max_order_value_jpy: Decimal = Decimal(500_000)
    #: 1日あたりの最大発注件数
    max_orders_per_day: int = 20
    #: 1日あたりの最大損失額（円）。超過で当日の新規発注を停止
    max_daily_loss_jpy: Decimal = Decimal(100_000)
    #: 1銘柄あたりのポートフォリオ比率の上限
    max_position_weight: Decimal = Decimal("0.25")
    #: 建玉合計が総資産に占める比率の上限
    max_gross_exposure: Decimal = Decimal("0.90")
    #: preview_order の見積りと自前計算の許容乖離率
    max_preview_deviation: Decimal = Decimal("0.02")

    @field_validator("max_position_weight", "max_gross_exposure", "max_preview_deviation")
    @classmethod
    def _ratio_range(cls, v: Decimal) -> Decimal:
        if not 0 < v <= 1:
            raise ValueError("比率は 0 より大きく 1 以下")
        return v


class SizingConfig(BaseModel):
    """ポジションサイジング。"""

    model_config = {"extra": "forbid"}

    #: "equal_weight" | "fixed_notional" | "atr_risk"
    method: str = "atr_risk"
    #: fixed_notional のときの1銘柄あたり投入額
    fixed_notional_jpy: Decimal = Decimal(300_000)
    #: atr_risk のときの1トレードあたり許容損失（総資産比）
    risk_per_trade: Decimal = Decimal("0.01")
    #: 損切り幅を ATR の何倍に置くか
    atr_stop_multiple: Decimal = Decimal("2.0")
    #: 同時に保有する最大銘柄数
    max_positions: int = 5

    @field_validator("method")
    @classmethod
    def _known_method(cls, v: str) -> str:
        allowed = {"equal_weight", "fixed_notional", "atr_risk"}
        if v not in allowed:
            raise ValueError(f"method は {sorted(allowed)} のいずれか: {v}")
        return v


class UniverseConfig(BaseModel):
    """売買対象。

    ``symbols`` は allowlist としても機能する。ここに無い銘柄には
    どんな経路でも発注されない。
    """

    model_config = {"extra": "forbid"}

    symbols: list[str] = Field(default_factory=list)
    #: TOPIX500 構成銘柄（呼値が細かくなる）
    topix500_symbols: list[str] = Field(default_factory=list)
    #: 売買単位が 100 株でない銘柄の例外 {銘柄コード: 単元株数}
    lot_size_overrides: dict[str, int] = Field(default_factory=dict)

    @field_validator("symbols", "topix500_symbols")
    @classmethod
    def _normalize(cls, v: list[str]) -> list[str]:
        return [s.strip() for s in v if s.strip()]

    @model_validator(mode="after")
    def _topix500_subset(self) -> Self:
        unknown = set(self.topix500_symbols) - set(self.symbols)
        if unknown:
            raise ValueError(f"topix500_symbols が symbols に含まれていません: {sorted(unknown)}")
        return self


class StrategyEntry(BaseModel):
    """1つの戦略の有効化と重み。"""

    model_config = {"extra": "allow"}  # 戦略固有パラメータを受け取る

    name: str
    enabled: bool = True
    weight: float = 1.0

    @property
    def params(self) -> dict[str, Any]:
        """戦略コンストラクタに渡す固有パラメータ。"""
        reserved = {"name", "enabled", "weight"}
        extra = self.__pydantic_extra__ or {}
        return {k: v for k, v in extra.items() if k not in reserved}


class StrategiesConfig(BaseModel):
    model_config = {"extra": "forbid"}

    #: "weighted_vote" | "majority" | "veto" | "priority"
    combiner: str = "weighted_vote"
    #: 合成後の |direction| がこれ未満なら中立とみなす
    entry_threshold: float = 0.3
    #: 保有中の銘柄がこれを下回ったら手仕舞う（ヒステリシス）
    exit_threshold: float = 0.1
    strategies: list[StrategyEntry] = Field(default_factory=list)

    @model_validator(mode="after")
    def _thresholds_ordered(self) -> Self:
        if self.exit_threshold > self.entry_threshold:
            raise ValueError("exit_threshold は entry_threshold 以下にしてください")
        return self

    @property
    def enabled(self) -> list[StrategyEntry]:
        return [s for s in self.strategies if s.enabled]


class ExecutionConfig(BaseModel):
    """発注の振る舞い。"""

    model_config = {"extra": "forbid"}

    tax_account_type: TaxAccountType = TaxAccountType.SPECIFIC
    #: "market" | "limit"
    order_type: str = "limit"
    #: 指値を直近終値から何%ずらすか（買いは上、売りは下＝約定しやすい方向）
    limit_offset: Decimal = Decimal("0.005")

    @field_validator("order_type")
    @classmethod
    def _known_order_type(cls, v: str) -> str:
        if v not in {"market", "limit"}:
            raise ValueError(f"order_type は market か limit: {v}")
        return v


class FileConfig(BaseModel):
    """config/*.toml の内容全体。"""

    model_config = {"extra": "forbid"}

    universe: UniverseConfig = Field(default_factory=UniverseConfig)
    risk: RiskConfig = Field(default_factory=RiskConfig)
    sizing: SizingConfig = Field(default_factory=SizingConfig)
    execution: ExecutionConfig = Field(default_factory=ExecutionConfig)
    strategies: StrategiesConfig = Field(default_factory=StrategiesConfig)


# --------------------------------------------------------------------------
# 環境変数由来の設定
# --------------------------------------------------------------------------


class AppSettings(BaseSettings):
    """環境変数と .env から読む設定。ここに秘密は入れない。"""

    model_config = SettingsConfigDict(
        env_prefix="WBJP_",
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    env: Environment = Environment.UAT
    config_dir: Path = Path("config")
    data_dir: Path = Path("data")
    log_level: str = "INFO"
    #: 構造化ログを JSON で出す（本番運用向け）
    log_json: bool = False

    @property
    def endpoints(self) -> Endpoints:
        return ENDPOINTS[self.env]

    @property
    def db_path(self) -> Path:
        return self.data_dir / f"wbjp-{self.env.value}.db"

    @property
    def bars_dir(self) -> Path:
        return self.data_dir / "bars"


@dataclass(frozen=True, slots=True)
class Config:
    """アプリ全体の設定。"""

    settings: AppSettings
    file: FileConfig

    @property
    def env(self) -> Environment:
        return self.settings.env

    def allows_live_orders(self, live_flag: bool) -> tuple[bool, str]:
        """実発注してよいかを判定する。

        本番の実発注には ``WBJP_ENV=prod`` と ``--live`` の**両方**が要る。
        UAT は実弾ではないので ``--live`` だけで足りる。

        Returns:
            (発注してよいか, 理由)
        """
        if self.file.risk.kill_switch:
            return False, "キルスイッチが有効（config の risk.kill_switch = true）"
        if not live_flag:
            return False, "--live が指定されていない（dry-run）"
        if self.env.is_production:
            return True, "本番環境・--live 指定あり"
        return True, f"{self.env.value} 環境・--live 指定あり（実弾ではない）"


def load_file_config(config_dir: Path) -> FileConfig:
    """``settings.toml`` と ``strategies.toml`` を読み込む。

    どちらも無ければ既定値で動く。
    """
    merged: dict[str, Any] = {}

    settings_path = config_dir / "settings.toml"
    if settings_path.exists():
        merged.update(_read_toml(settings_path))

    strategies_path = config_dir / "strategies.toml"
    if strategies_path.exists():
        merged["strategies"] = _read_toml(strategies_path)

    return FileConfig.model_validate(merged)


def _read_toml(path: Path) -> dict[str, Any]:
    with path.open("rb") as fh:
        return tomllib.load(fh)


def load_config(config_dir: Path | None = None) -> Config:
    """設定一式を読み込む。"""
    settings = AppSettings()
    if config_dir is not None:
        settings = settings.model_copy(update={"config_dir": config_dir})
    return Config(settings=settings, file=load_file_config(settings.config_dir))
