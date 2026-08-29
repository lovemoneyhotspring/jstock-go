"""Webull API の認証情報。

方針:
    - **秘密はリポジトリに置かない。** ローカル開発では OS のキーチェーンに
      保管する。キーチェーンの無いサーバー（ヘッドレスな Linux など）では
      環境変数か、パーミッションを絞った ``.env`` を使う。
    - 環境（uat / prod）ごとに認証情報を完全に分ける。取り違えて本番に
      発注する事故を、名前空間の分離で防ぐ。

名前空間:
    証券会社ごとに ``namespace`` で分ける。Webull は ``WBJP``——環境変数は
    ``WBJP_PROD_APP_KEY``、キーチェーンは ``wbjp/prod``。別の証券会社を
    足すときは、その :class:`~wbcore.broker.base.Broker` が自分の名前空間を
    渡す。プロジェクト（売買 / 積立）を分けても口座は同じなので、
    名前空間はプロジェクトではなく証券会社に紐づける。
"""

from __future__ import annotations

import datetime as dt
import os
import stat
from collections.abc import Mapping
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path

import structlog
from dotenv import dotenv_values

log = structlog.get_logger(__name__)

#: 既定の名前空間（Webull）。環境変数の接頭辞とキーチェーンのサービス名になる。
DEFAULT_NAMESPACE = "WBJP"

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


def _keyring_service(env: Environment, namespace: str) -> str:
    return f"{namespace.lower()}/{env.value}"


def _from_keyring(env: Environment, namespace: str = DEFAULT_NAMESPACE) -> dict[str, str | None]:
    try:
        import keyring
    except ImportError:  # pragma: no cover - keyring は必須依存
        return {}

    service = _keyring_service(env, namespace)
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


def _scoped_lookup(
    source: Mapping[str, str | None], env: Environment, namespace: str
) -> dict[str, str | None]:
    """``WBJP_UAT_APP_KEY`` 形式を優先し、``WBJP_APP_KEY`` を後方互換とする。"""
    prefix = f"{namespace}_{env.value.upper()}_"
    return {
        name: source.get(f"{prefix}{name.upper()}") or source.get(f"{namespace}_{name.upper()}")
        for name in _CREDENTIAL_FIELDS
    }


def _from_env_vars(env: Environment, namespace: str) -> dict[str, str | None]:
    """環境変数から読む。systemd の ``EnvironmentFile=`` もここに来る。"""
    return _scoped_lookup(os.environ, env, namespace)


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
    namespace: str,
) -> tuple[dict[str, str | None], dict[str, str]]:
    """項目ごとに値と、その取得元のラベルを返す。

    ``load_credentials`` と ``credential_source`` が**同じ**解決を見るための
    土台。別々に判定すると、実際に使われた認証情報と診断表示がずれる。
    """
    candidates = (
        ("環境変数", _from_env_vars(env, namespace)),
        (str(env_file), _scoped_lookup(dotenv, env, namespace)),
        ("OS キーチェーン", _from_keyring(env, namespace)),
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
    namespace: str = DEFAULT_NAMESPACE,
) -> Credentials:
    """認証情報を解決する。

    優先順位:
        1. 環境変数（CI・systemd の ``EnvironmentFile=`` 向け）
        2. ``.env``（キーチェーンの無いサーバー向け。0600 にすること）
        3. OS キーチェーン（ローカル開発の既定）
        4. Webull の UAT のみ: 公開のテスト口座

    Args:
        namespace: 証券会社ごとの名前空間（環境変数の接頭辞）。既定は Webull。

    Raises:
        MissingCredentialsError: どこにも見つからないとき。
    """
    path = _resolve_env_file(env_file)
    dotenv = _read_dotenv(path)
    resolved, _ = _resolve_fields(env, dotenv, path, namespace)

    created_key = f"{namespace}_{env.value.upper()}_KEY_CREATED_ON"
    created_on = _parse_date(os.environ.get(created_key) or dotenv.get(created_key))

    if all(resolved.values()):
        return Credentials(
            app_key=str(resolved["app_key"]),
            app_secret=str(resolved["app_secret"]),
            account_id=str(resolved["account_id"]),
            created_on=created_on,
        )

    # 本番で公開テスト口座にフォールバックすることは絶対にない。
    # 公開テスト口座は Webull のものなので、他の名前空間にも使わない。
    if env is Environment.UAT and allow_public_test_account and namespace == DEFAULT_NAMESPACE:
        return Credentials(
            app_key=PUBLIC_UAT_CREDENTIALS["app_key"],
            app_secret=PUBLIC_UAT_CREDENTIALS["app_secret"],
            account_id=PUBLIC_UAT_CREDENTIALS["account_id"],
            is_public_test_account=True,
        )

    missing = [k for k, v in resolved.items() if not v]
    upper = env.value.upper()
    raise MissingCredentialsError(
        f"{env.value} 環境の認証情報（{namespace}）が不足しています: {', '.join(missing)}\n"
        f"  キーチェーンに登録: uv run wbjp credentials set --env {env.value}\n"
        f"  または環境変数:     {namespace}_{upper}_APP_KEY 等を設定\n"
        f"  または .env に記載: {namespace}_{upper}_APP_KEY=... （chmod 600 のこと）"
    )


def credential_source(
    env: Environment, *, env_file: Path | None = None, namespace: str = DEFAULT_NAMESPACE
) -> str:
    """認証情報がどこから来たかを人間向けに説明する。

    どこから読まれているか分からないまま本番に発注する事故を防ぐための、
    ``credentials check`` 用の診断。秘密そのものは返さない。

    **``load_credentials`` が実際に採用したものを説明すること。** 1項目でも
    欠けていれば全体が公開テスト口座にフォールバックするので、``app_key``
    だけを見て「.env から読んだ」と表示すると嘘になる。
    """
    path = _resolve_env_file(env_file)
    resolved, origins = _resolve_fields(env, _read_dotenv(path), path, namespace)

    missing = [k for k, v in resolved.items() if not v]
    if missing:
        detail = f"{', '.join(missing)} が未設定"
        if env is Environment.UAT and namespace == DEFAULT_NAMESPACE:
            return f"公開テスト口座（UAT 共有）— {detail}のため"
        return f"解決できません（{detail}）"

    labels = set(origins.values())
    if len(labels) == 1:
        return labels.pop()
    # 項目ごとにソースが違うのは事故のもと。どれがどこから来たかを出す。
    return ", ".join(f"{k}={origins[k]}" for k in _CREDENTIAL_FIELDS)


def store_credentials(
    env: Environment, creds: Credentials, *, namespace: str = DEFAULT_NAMESPACE
) -> None:
    """認証情報を OS キーチェーンに保存する。"""
    import keyring

    service = _keyring_service(env, namespace)
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
