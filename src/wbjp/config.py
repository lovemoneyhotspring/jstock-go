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
from collections import defaultdict
from collections.abc import Mapping
from dataclasses import dataclass
from decimal import Decimal
from enum import StrEnum
from pathlib import Path
from typing import TYPE_CHECKING, Any, Self

import structlog
from dotenv import dotenv_values
from pydantic import BaseModel, Field, field_validator, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

from wbjp.domain.models import Market, TaxAccountType

if TYPE_CHECKING:
    from wbjp.accumulate.basket import DrawdownTilt, WeightSchedule
    from wbjp.accumulate.tactics import Tactic

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


def _resolve_fields(
    env: Environment,
    dotenv: dict[str, str | None],
    env_file: Path,
) -> tuple[dict[str, str | None], dict[str, str]]:
    """項目ごとに値と、その取得元のラベルを返す。

    ``load_credentials`` と ``credential_source`` が**同じ**解決を見るための
    土台。別々に判定すると、実際に使われた認証情報と診断表示がずれる。
    """
    candidates = (
        ("環境変数", _from_env_vars(env)),
        (str(env_file), _scoped_lookup(dotenv, env)),
        ("OS キーチェーン", _from_keyring(env)),
    )
    resolved: dict[str, str | None] = dict.fromkeys(_CREDENTIAL_FIELDS)
    origins: dict[str, str] = {}
    for label, source in candidates:
        for key, value in source.items():
            if resolved[key] is None and value:
                resolved[key] = value
                origins[key] = label
    return resolved, origins


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
    path = _resolve_env_file(env_file)
    dotenv = _read_dotenv(path)
    resolved, _ = _resolve_fields(env, dotenv, path)

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
    """認証情報がどこから来たかを人間向けに説明する。

    どこから読まれているか分からないまま本番に発注する事故を防ぐための、
    ``credentials check`` 用の診断。秘密そのものは返さない。

    **``load_credentials`` が実際に採用したものを説明すること。** 1項目でも
    欠けていれば全体が公開テスト口座にフォールバックするので、``app_key``
    だけを見て「.env から読んだ」と表示すると嘘になる。
    """
    path = _resolve_env_file(env_file)
    resolved, origins = _resolve_fields(env, _read_dotenv(path), path)

    missing = [k for k, v in resolved.items() if not v]
    if missing:
        detail = f"{', '.join(missing)} が未設定"
        if env is Environment.UAT:
            return f"公開テスト口座（UAT 共有）— {detail}のため"
        return f"解決できません（{detail}）"

    labels = set(origins.values())
    if len(labels) == 1:
        return labels.pop()
    # 項目ごとにソースが違うのは事故のもと。どれがどこから来たかを出す。
    return ", ".join(f"{k}={origins[k]}" for k in _CREDENTIAL_FIELDS)


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


def _rename_legacy_keys(data: Any, mapping: dict[str, str]) -> Any:
    """旧フィールド名（``*_jpy``）を新名に読み替える。

    金額の項目は米国株対応で通貨を含まない名前になった。既存の設定
    ファイルを壊さないため、旧名も受け付ける。両方書かれていれば
    設定ミスなので落とす。
    """
    if not isinstance(data, dict):
        return data
    renamed = dict(data)
    for old, new in mapping.items():
        if old in renamed:
            if new in renamed:
                raise ValueError(f"{old} と {new} は同時に指定できません")
            renamed[new] = renamed.pop(old)
    return renamed


class RiskConfig(BaseModel):
    """リスク上限。発注前に全項目をチェックする。

    金額の項目は**口座通貨建て**（日本株なら円、米国株ならドル）。
    ``max_order_value_jpy`` など旧名も読めるが、新しい設定では
    通貨を含まない名前を使う。
    """

    model_config = {"extra": "forbid"}

    #: true で全発注を即停止する。ファイル1行で止められる緊急停止装置。
    kill_switch: bool = False
    #: 1注文あたりの最大約定代金（口座通貨）
    max_order_value: Decimal = Decimal(500_000)
    #: 1日あたりの最大発注件数
    max_orders_per_day: int = 20
    #: 1日あたりの最大損失額（口座通貨）。超過で当日の新規発注を停止
    max_daily_loss: Decimal = Decimal(100_000)

    @model_validator(mode="before")
    @classmethod
    def _accept_legacy_names(cls, data: Any) -> Any:
        return _rename_legacy_keys(
            data, {"max_order_value_jpy": "max_order_value", "max_daily_loss_jpy": "max_daily_loss"}
        )

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
    #: fixed_notional のときの1銘柄あたり投入額（口座通貨）
    fixed_notional: Decimal = Decimal(300_000)

    @model_validator(mode="before")
    @classmethod
    def _accept_legacy_names(cls, data: Any) -> Any:
        return _rename_legacy_keys(data, {"fixed_notional_jpy": "fixed_notional"})

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

    #: 取引市場。1つの設定ディレクトリは1つの市場だけを扱う。
    #: 日本株と米国株を両方回すなら、設定ディレクトリを分けて別プロセスで動かす。
    market: Market = Market.JP
    #: 足データの取得元。"yfinance"（両市場）| "webull"（米国株のみ）
    data_provider: str = "yfinance"
    symbols: list[str] = Field(default_factory=list)
    #: 銘柄リストのファイル（1行1銘柄、# はコメント）。設定ディレクトリからの相対パス。
    #: 読み込んだ銘柄は ``symbols`` に合流し、allowlist にもなる。
    symbols_file: str | None = None
    #: TOPIX500 構成銘柄（呼値が細かくなる）。日本株のみ意味を持つ。
    topix500_symbols: list[str] = Field(default_factory=list)
    #: 売買単位が既定と異なる銘柄の例外 {銘柄コード: 単元株数}
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
        if self.market is not Market.JP and self.topix500_symbols:
            raise ValueError('topix500_symbols は market = "JP" のときだけ指定できます')
        if self.data_provider not in {"yfinance", "webull"}:
            raise ValueError(f"data_provider は yfinance か webull: {self.data_provider}")
        if self.data_provider == "webull" and self.market is not Market.US:
            raise ValueError('data_provider = "webull" は米国株（market = "US"）専用です')
        return self

    @property
    def currency(self) -> str:
        return self.market.currency


class StopsConfig(BaseModel):
    """損切り・利確の管理。

    ストップ価格は :mod:`wbjp.risk.stops` が持つ。ここはその動かし方。
    初期ストップ幅は ``sizing.atr_stop_multiple`` で決まる（= 1R）。
    """

    model_config = {"extra": "forbid"}

    #: ATR トレーリングストップを使うか（上げるだけ、下げない）
    trailing: bool = False
    #: 含み益が初期リスクの何倍に達したらストップを建値に上げるか。None で無効
    breakeven_after_r: Decimal | None = None
    #: 建ててから何営業日で含み益ゼロ以下なら手仕舞うか。None で無効
    stale_exit_days: int | None = None
    #: 最大保有営業日数。None で無効
    max_hold_days: int | None = None
    #: 初期ストップ幅を建値からの比率で固定する（例: 0.04 = -4%）。
    #: None なら ``sizing.atr_stop_multiple`` による ATR ベースのまま。
    initial_stop_pct: Decimal | None = None
    #: 含み益がこの R 倍に達したら、建玉の一部を利確する（2段階利確の1段目）。
    #: None で無効。
    take_profit_r: Decimal | None = None
    #: 1段目の利確で手仕舞う比率（既定 50%）。残りは ``trend_exit_sma`` に委ねる。
    take_profit_fraction: Decimal = Decimal("0.5")
    #: 1段目の利確後、残りの建玉を手仕舞う移動平均の期間（例: 20 = 20日MA）。
    #: 終値がこの移動平均を割り込んだら残り全部を手仕舞う。None で無効。
    trend_exit_sma: int | None = None
    #: True なら ``trend_exit_sma`` 割れを利確前の建玉にも適用する（移動平均そのものを
    #: 利確・損切りの合図にする）。False（既定）は 1 段目の利確後だけ。
    trend_exit_always: bool = False
    #: トレーリングストップの幅（ATR 倍率）。None なら初期ストップと同じ
    #: ``sizing.atr_stop_multiple``。初期は狭く（1.5）、追従は広く（2.5 =
    #: Chandelier Exit）としたいときに使う。
    trailing_atr_multiple: Decimal | None = None
    #: %トレーリング（例: 0.08 = 最高終値から −8%）。設定すると ATR 追従の代わりに使う
    trailing_pct: Decimal | None = None
    #: ``trend_exit_sma`` の線の種類: sma / ema / donchian（N 日安値割れ＝タートル型）
    trend_exit_kind: str = "sma"

    @field_validator("trend_exit_kind")
    @classmethod
    def _known_kind(cls, v: str) -> str:
        if v not in ("sma", "ema", "donchian"):
            raise ValueError(f"trend_exit_kind は sma / ema / donchian: {v}")
        return v

    @field_validator("breakeven_after_r", "take_profit_r")
    @classmethod
    def _positive_r(cls, v: Decimal | None) -> Decimal | None:
        if v is not None and v <= 0:
            raise ValueError("R 倍率は正の数")
        return v

    @field_validator("stale_exit_days", "max_hold_days", "trend_exit_sma")
    @classmethod
    def _positive_days(cls, v: int | None) -> int | None:
        if v is not None and v <= 0:
            raise ValueError("日数は正の整数")
        return v

    @field_validator("initial_stop_pct", "take_profit_fraction", "trailing_pct")
    @classmethod
    def _ratio(cls, v: Decimal | None) -> Decimal | None:
        if v is not None and not 0 < v < 1:
            raise ValueError("比率は 0 より大きく 1 未満")
        return v

    @model_validator(mode="after")
    def _stale_before_max(self) -> Self:
        if (
            self.stale_exit_days is not None
            and self.max_hold_days is not None
            and self.stale_exit_days > self.max_hold_days
        ):
            raise ValueError("stale_exit_days は max_hold_days 以下にしてください")
        return self


class RegimeConfig(BaseModel):
    """相場レジーム（指数の環境認識）による露出の制御。

    S&P500 の買い持ちの弱点は暴落時に全額被弾すること。指数の位置で
    3 段階に分け、弱気では全建玉を手仕舞って現金（短期国債の利息）に退避する。

    - 強気:   終値 > SMA長期 かつ SMA長期が上向き        → exposure_bull
    - 警戒:   終値 > SMA長期 だが 終値 < SMA中期（または長期線が下向き） → exposure_caution
    - 弱気:   終値 < SMA長期                              → exposure_bear（0 なら全手仕舞い）

    露出は「サイジングに渡す総資産」を比率で縮めることで実現する。
    新規建ては、建玉比率が露出上限に達していれば見送る。
    """

    model_config = {"extra": "forbid"}

    enabled: bool = False
    #: 環境認識に使う指数（universe.symbols に含めること）
    benchmark: str = "SPY"
    sma_long: int = 200
    sma_mid: int = 50
    #: 長期線の傾きを測る日数
    slope_lookback: int = 20
    exposure_bull: Decimal = Decimal("1.0")
    exposure_caution: Decimal = Decimal("0.5")
    exposure_bear: Decimal = Decimal("0")
    #: 待機資金の利回りに使う系列（^IRX = 13 週 T-Bill 利回り、%）。None で無利息
    cash_yield_symbol: str | None = None

    @field_validator("exposure_bull", "exposure_caution", "exposure_bear")
    @classmethod
    def _exposure_range(cls, v: Decimal) -> Decimal:
        if not 0 <= v <= 1:
            raise ValueError("露出は 0〜1")
        return v

    @model_validator(mode="after")
    def _ordered(self) -> Self:
        if not self.exposure_bear <= self.exposure_caution <= self.exposure_bull:
            raise ValueError("露出は 弱気 ≤ 警戒 ≤ 強気 の順")
        if self.sma_mid >= self.sma_long:
            raise ValueError("sma_mid は sma_long より短く")
        return self


# --------------------------------------------------------------------------
# 戦略（strategies.toml）
#
# 売買型（[[strategies]]）と積立型（[[tactics]] / [[baskets]]）を1つの
# ファイル・1つのモデルで扱う。どちらも取引に使う戦略なので、置き場所を
# 分けると片方の存在を忘れる。書き方の違いは節の名前だけ:
#
#   [[strategies]] … name で機構を指す。全銘柄に意見を出し、合成される
#   [[tactics]]    … id がラベル、tactic が機構。銘柄ごとに1つだけ割り当てる
#   [[baskets]]    … 複数銘柄への配分。比較検証が主用途
# --------------------------------------------------------------------------


class StrategyEntry(BaseModel):
    """1つの売買型戦略の有効化と重み（``[[strategies]]``）。"""

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


_TACTIC_RESERVED = frozenset({"id", "tactic", "symbols", "enabled", "market"})


class TacticEntry(BaseModel):
    """1つの積立型戦略と、それを適用する銘柄（``[[tactics]]``）。

    Attributes:
        id: 比較表の行名になる自由なラベル。日本語でよい。
        tactic: 登録簿の鍵（``bear_stack`` など）。機構の名前。
        symbols: この戦略で積み立てる銘柄コード。
        market: 銘柄の市場。足を取りに行くときのティッカー変換に使う
            （日本株の ``1305`` は ``1305.T``、米国株の ``VOO`` はそのまま）。
            ``^`` で始まる指数は市場に関係なくそのまま扱われる。
    """

    model_config = {"extra": "allow"}  # 戦略固有パラメータを受け取る

    id: str
    tactic: str
    symbols: list[str] = Field(min_length=1)
    enabled: bool = True
    market: Market = Market.JP

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
        """積立型戦略のインスタンスを組み立てる。"""
        from wbjp.strategy.registry import create_tactic

        try:
            return create_tactic(self.tactic, self.params)
        except ValueError as exc:
            raise ValueError(f"[{self.id}] {exc}") from None


class BasketEntry(BaseModel):
    """複数銘柄への配分（``[[baskets]]``、:mod:`wbjp.accumulate.basket`）。

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
        from wbjp.accumulate.basket import DrawdownTilt

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
        from wbjp.strategy.registry import create_tactic

        try:
            return create_tactic(self.tactic, self.params)
        except ValueError as exc:
            raise ValueError(f"[{self.id}] {exc}") from None

    def build_schedule(
        self, schedule_13f: list[tuple[Any, dict[str, float]]] | None = None
    ) -> WeightSchedule:
        """配分表を組み立てる。``13f`` のときは取得済みの比率列を渡す。"""
        from wbjp.accumulate.basket import WeightSchedule

        if self.source == "static":
            if not self.weights:
                raise ValueError(f"[{self.id}] static には weights が必要です")
            return WeightSchedule.static(self.weights)
        if not schedule_13f:
            raise ValueError(
                f"[{self.id}] 13F の保有一覧がありません（`wbjp accumulate sync-13f` を実行）"
            )
        schedule = WeightSchedule.from_pairs(schedule_13f)
        if self.weights:
            return schedule.blend(self.weights, self.satellite_share)
        return schedule


_BASKET_RESERVED = frozenset(BasketEntry.model_fields)


class StrategiesConfig(BaseModel):
    """``strategies.toml`` の内容。売買型と積立型の両方を持つ。"""

    model_config = {"extra": "forbid"}

    #: "weighted_vote" | "majority" | "veto" | "priority"
    combiner: str = "weighted_vote"
    #: 合成後の |direction| がこれ未満なら中立とみなす
    entry_threshold: float = 0.3
    #: 保有中の銘柄がこれを下回ったら手仕舞う（ヒステリシス）
    exit_threshold: float = 0.1
    strategies: list[StrategyEntry] = Field(default_factory=list)

    monthly_budget: Decimal = Decimal(25_000)
    """積立の1銘柄あたりの毎月の基本予算。比較の前提を揃えるため全戦略で共通。"""

    tactics: list[TacticEntry] = Field(default_factory=list)
    baskets: list[BasketEntry] = Field(default_factory=list)
    """複数銘柄への配分。積立型戦略と違い1銘柄が複数のバスケットに現れてもよい
    （バスケットは比較検証が主用途で、実発注は id を指定して行う）。"""

    @model_validator(mode="after")
    def _thresholds_ordered(self) -> Self:
        if self.exit_threshold > self.entry_threshold:
            raise ValueError("exit_threshold は entry_threshold 以下にしてください")
        return self

    # -- 売買型 ---------------------------------------------------------

    @property
    def enabled(self) -> list[StrategyEntry]:
        """有効な売買型戦略。"""
        return [s for s in self.strategies if s.enabled]

    # -- 積立型 ---------------------------------------------------------

    @property
    def active(self) -> list[TacticEntry]:
        """有効な積立型戦略。"""
        return [t for t in self.tactics if t.enabled]

    @property
    def active_baskets(self) -> list[BasketEntry]:
        return [b for b in self.baskets if b.enabled]

    def validate_assignment(self, *, allow_overlap: bool = False) -> None:
        """id の重複と、1銘柄への複数割り当てを検出する。

        実運用で1銘柄に複数の積立型戦略を割り当てると二重に買い付けることに
        なり、予算が意図せず倍になる。比較検証をしたい場合だけ
        ``allow_overlap=True`` を渡す。

        Raises:
            ValueError: id が重複、または ``allow_overlap`` が False のときに
                同じ銘柄が複数の積立型戦略に現れた場合。
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
                f"1銘柄に複数の積立型戦略が割り当てられています（二重買付になります）: {detail}"
            )

    def build(self) -> dict[str, Tactic]:
        """``銘柄 → 積立型戦略`` に展開する。積立の主用途はこれ。"""
        return {s: entry.build() for entry in self.active for s in entry.symbols}

    def tactic_for(self, symbol: str) -> Tactic | None:
        """銘柄に割り当てられた積立型戦略。無ければ None。"""
        return self.build().get(symbol)

    def symbols_by_market(self) -> dict[Market, list[str]]:
        """市場 → 銘柄。足の取得はティッカー変換が市場ごとに違うので分ける。

        銘柄が複数の戦略に現れないことは :meth:`validate_assignment` が
        保証しているので、ここで市場が衝突することはない。
        """
        grouped: dict[Market, list[str]] = defaultdict(list)
        for entry in self.active:
            grouped[entry.market].extend(entry.symbols)
        return {market: sorted(set(symbols)) for market, symbols in grouped.items()}

    @property
    def symbols(self) -> list[str]:
        """有効な積立型戦略が割り当てられた銘柄の一覧。"""
        return sorted({s for entry in self.active for s in entry.symbols})


class ExecutionConfig(BaseModel):
    """発注の振る舞い。"""

    model_config = {"extra": "forbid"}

    tax_account_type: TaxAccountType = TaxAccountType.SPECIFIC
    #: "market" | "limit"
    order_type: str = "limit"
    #: 指値を直近終値から何%ずらすか（買いは上、売りは下＝約定しやすい方向）
    limit_offset: Decimal = Decimal("0.005")
    #: 損切りの置き場所。
    #:   "auto"   … 市場が逆指値に対応していればブローカーに置き、無ければエンジン合成
    #:   "engine" … 常にエンジン側で日足判定する（米国株でも）
    #:   "broker" … 常にブローカーに置く。対応しない市場では設定エラー
    stop_mode: str = "auto"
    #: 米国株の時間外（プレ・アフター）取引を許すか。日本株では無視される。
    #: 時間外は板が薄く、指値でも想定外の価格で約定しやすいので既定は無効。
    extended_hours: bool = False

    @field_validator("order_type")
    @classmethod
    def _known_order_type(cls, v: str) -> str:
        if v not in {"market", "limit"}:
            raise ValueError(f"order_type は market か limit: {v}")
        return v

    @field_validator("stop_mode")
    @classmethod
    def _known_stop_mode(cls, v: str) -> str:
        if v not in {"auto", "engine", "broker"}:
            raise ValueError(f"stop_mode は auto / engine / broker のいずれか: {v}")
        return v


class FileConfig(BaseModel):
    """config/*.toml の内容全体。"""

    model_config = {"extra": "forbid"}

    universe: UniverseConfig = Field(default_factory=UniverseConfig)
    risk: RiskConfig = Field(default_factory=RiskConfig)
    sizing: SizingConfig = Field(default_factory=SizingConfig)
    execution: ExecutionConfig = Field(default_factory=ExecutionConfig)
    stops: StopsConfig = Field(default_factory=StopsConfig)
    strategies: StrategiesConfig = Field(default_factory=StrategiesConfig)
    regime: RegimeConfig = Field(default_factory=RegimeConfig)

    @model_validator(mode="after")
    def _stop_mode_is_possible(self) -> Self:
        from wbjp.domain.market_rules import rules_for

        rules = rules_for(self.universe.market)
        if self.execution.stop_mode == "broker" and not rules.supports_broker_stops:
            raise ValueError(
                f'execution.stop_mode = "broker" は {self.universe.market.value} 市場では'
                "使えません（逆指値 API 非対応）。auto か engine にしてください"
            )
        return self

    @property
    def market(self) -> Market:
        return self.universe.market

    @property
    def uses_broker_stops(self) -> bool:
        """損切りをブローカーの逆指値として置くか。"""
        from wbjp.domain.market_rules import rules_for

        match self.execution.stop_mode:
            case "engine":
                return False
            case "broker":
                return True
            case _:
                return rules_for(self.universe.market).supports_broker_stops


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


#: 戦略設定のファイル名。売買型・積立型ともここに書く。
STRATEGIES_FILENAME = "strategies.toml"

#: 積立を別ファイルに分けていた頃の名前。見つかったら取り込んで移行を促す。
LEGACY_ACCUMULATE_FILENAME = "accumulate.toml"


def _read_strategies_toml(config_dir: Path) -> dict[str, Any]:
    """``strategies.toml`` を読む。旧 ``accumulate.toml`` があれば取り込む。

    かつて積立は別ファイルだった。統合後も古いファイルを黙って無視すると
    「積立が丸ごと消えた」という分かりにくい状態になるので、読み込んだ上で
    移行を促す。同じ節が両方にあるときは新しい ``strategies.toml`` を採る。
    """
    raw: dict[str, Any] = {}
    path = config_dir / STRATEGIES_FILENAME
    if path.exists():
        raw = _read_toml(path)

    legacy_path = config_dir / LEGACY_ACCUMULATE_FILENAME
    if not legacy_path.exists():
        return raw

    legacy = _read_toml(legacy_path)
    log.warning(
        "旧 accumulate.toml を読み込みました。内容を strategies.toml へ移して削除してください",
        legacy=str(legacy_path),
        target=str(path),
    )
    return {**legacy, **raw}


def load_strategies(config_dir: Path | str = Path("config"), *, allow_overlap: bool = False):  # type: ignore[no-untyped-def]
    """``strategies.toml`` だけを読む。積立系コマンドの入り口。

    売買には ``universe`` などの検証が要るが、積立には要らない。そのぶん
    軽く読めるように :func:`load_file_config` とは別の入り口にしてある。

    Args:
        config_dir: 設定ディレクトリ。
        allow_overlap: 1銘柄に複数の積立型戦略を許すか。比較検証のときだけ True。

    Raises:
        FileNotFoundError: 設定ファイルが無いとき。
        ValueError: 内容が不正なとき。
    """
    directory = Path(config_dir)
    if (
        not (directory / STRATEGIES_FILENAME).is_file()
        and not (directory / LEGACY_ACCUMULATE_FILENAME).is_file()
    ):
        raise FileNotFoundError(f"戦略の設定が見つかりません: {directory / STRATEGIES_FILENAME}")
    config = StrategiesConfig.model_validate(_read_strategies_toml(directory))
    config.validate_assignment(allow_overlap=allow_overlap)
    return config


def load_file_config(config_dir: Path) -> FileConfig:
    """``settings.toml`` と ``strategies.toml`` を読み込む。

    どちらも無ければ既定値で動く。
    """
    merged: dict[str, Any] = {}

    settings_path = config_dir / "settings.toml"
    if settings_path.exists():
        merged.update(_read_toml(settings_path))

    strategies = _read_strategies_toml(config_dir)
    if strategies:
        merged["strategies"] = strategies

    universe = merged.get("universe")
    if isinstance(universe, dict) and universe.get("symbols_file"):
        listed = read_symbols_file(config_dir / str(universe["symbols_file"]))
        existing = [str(s) for s in universe.get("symbols", [])]
        universe["symbols"] = existing + [s for s in listed if s not in existing]

    return FileConfig.model_validate(merged)


def read_symbols_file(path: Path) -> list[str]:
    """銘柄リストを読む。1行1銘柄、``#`` 以降はコメント、重複は最初の1つ。

    Raises:
        FileNotFoundError: ファイルが無いとき。黙って空にすると
            「対象銘柄ゼロ」で静かに何も起きなくなる。
    """
    if not path.is_file():
        raise FileNotFoundError(f"銘柄リストが見つかりません: {path}")
    symbols: list[str] = []
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.split("#", 1)[0].strip()
        if line and line not in symbols:
            symbols.append(line)
    return symbols


def _read_toml(path: Path) -> dict[str, Any]:
    with path.open("rb") as fh:
        return tomllib.load(fh)


def load_config(config_dir: Path | None = None) -> Config:
    """設定一式を読み込む。"""
    settings = AppSettings()
    if config_dir is not None:
        settings = settings.model_copy(update={"config_dir": config_dir})
    return Config(settings=settings, file=load_file_config(settings.config_dir))
