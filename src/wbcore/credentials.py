"""証券会社 API の認証情報。

方針:
    - **秘密はリポジトリに置かない。** ローカル開発では OS のキーチェーンに
      保管する。キーチェーンの無いサーバー（ヘッドレスな Linux など）では
      環境変数か、パーミッションを絞った ``.env`` を使う。
    - 環境（uat / prod）ごとに認証情報を完全に分ける。取り違えて本番に
      発注する事故を、名前空間の分離で防ぐ。

名前空間:
    証券会社ごとに ``namespace`` で分ける。立花証券は ``TACHIBANA``——環境変数は
    ``TACHIBANA_PROD_AUTH_ID``、キーチェーンは ``tachibana/prod``。証券会社を
    足すときは、その :class:`~wbcore.broker.base.Broker` が自分の名前空間と
    項目（``fields``）を渡す。プロジェクト（売買 / 積立）を分けても口座は同じ
    なので、名前空間はプロジェクトではなく証券会社に紐づける。

データ源の鍵（J-Quants など、口座と紐づかないもの）は :func:`load_api_key` で
単体の変数名から引く。
"""

from __future__ import annotations

import os
import stat
from collections.abc import Mapping
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path

import structlog
from dotenv import dotenv_values

log = structlog.get_logger(__name__)

#: 既定の ``.env`` の場所（プロセスのカレントディレクトリ）。
DEFAULT_ENV_FILE = Path(".env")

#: ``.env`` の場所を絶対パスで上書きする環境変数。
ENV_FILE_OVERRIDE_VAR = "WBJP_ENV_FILE"

#: 立花証券 e支店 API（e_api_v4r10）の認証情報を構成する項目。
#: 出典: API リファレンスと公式サンプル（e_api_sample_v4r10.py / .txt）。
#:
#:     - ``auth_id`` — ログイン電文 ``CLMAuthLoginRequest`` の ``sAuthId``。
#:       e支店 Web の「ｅ支店・ＡＰＩ利用設定」で自動生成される認証ID
#:     - ``private_key_file`` — 同画面で登録した公開鍵と対の**秘密鍵（PEM）のファイルパス**。
#:       ログイン応答の仮想URL（``sUrlRequest`` 等）は公開鍵で暗号化されて返るので、
#:       これで復号する（RSA 2048/4096、OAEP、SHA-256）。立花証券側は秘密鍵を保存しない
#:     - ``order_password`` — 新規注文・取消注文の必須パラメータ ``sSecondPassword``
#:       （第二暗証番号）。API 利用時は Web 側で「暗証番号省略」を無効にしておくこと
#:
#: 認証ID・公開鍵は本番とデモで別管理（環境ごとの名前空間 ``TACHIBANA_<ENV>_...``）。
TACHIBANA_NAMESPACE = "TACHIBANA"
TACHIBANA_CREDENTIAL_FIELDS = ("auth_id", "private_key_file", "order_password")


class Environment(StrEnum):
    UAT = "uat"
    PROD = "prod"

    @property
    def is_production(self) -> bool:
        return self is Environment.PROD


class MissingCredentialsError(RuntimeError):
    """認証情報が見つからない。"""


# --------------------------------------------------------------------------
# 認証情報の解決
# --------------------------------------------------------------------------


def _keyring_service(env: Environment, namespace: str) -> str:
    return f"{namespace.lower()}/{env.value}"


def _from_keyring(
    env: Environment, namespace: str, fields: tuple[str, ...]
) -> dict[str, str | None]:
    try:
        import keyring
    except ImportError:  # pragma: no cover - keyring は必須依存
        return {}

    service = _keyring_service(env, namespace)
    try:
        return {name: keyring.get_password(service, name) for name in fields}
    except Exception as exc:
        # ヘッドレスな Linux サーバーには SecretService も D-Bus も無く、
        # keyring は NoKeyringError を投げる（バックエンド由来の例外が
        # そのまま漏れてくることもある）。ここで落とすと、設定方法を案内する
        # MissingCredentialsError に到達できない。「見つからなかった」として
        # 扱い、他のソースに任せる。
        log.debug("keyring は利用できません", error=str(exc), service=service)
        return {}


def _scoped_lookup(
    source: Mapping[str, str | None],
    env: Environment,
    namespace: str,
    *,
    fields: tuple[str, ...],
) -> dict[str, str | None]:
    """``TACHIBANA_UAT_AUTH_ID`` 形式を優先し、``TACHIBANA_AUTH_ID`` を後方互換とする。"""
    prefix = f"{namespace}_{env.value.upper()}_"
    return {
        name: source.get(f"{prefix}{name.upper()}") or source.get(f"{namespace}_{name.upper()}")
        for name in fields
    }


def _from_env_vars(
    env: Environment, namespace: str, *, fields: tuple[str, ...]
) -> dict[str, str | None]:
    """環境変数から読む。systemd の ``EnvironmentFile=`` もここに来る。"""
    return _scoped_lookup(os.environ, env, namespace, fields=fields)


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
    *,
    fields: tuple[str, ...],
) -> tuple[dict[str, str | None], dict[str, str]]:
    """項目ごとに値と、その取得元のラベルを返す。

    :func:`load_tachibana_credentials` と :func:`credential_source` が**同じ**
    解決を見るための土台。別々に判定すると、実際に使われた認証情報と診断表示が
    ずれる。``fields`` を差し替えれば項目構成が違う証券会社にも同じ優先順位
    （環境変数 → ``.env`` → OS キーチェーン）を使い回せる。
    """
    candidates = (
        ("環境変数", _from_env_vars(env, namespace, fields=fields)),
        (str(env_file), _scoped_lookup(dotenv, env, namespace, fields=fields)),
        ("OS キーチェーン", _from_keyring(env, namespace, fields)),
    )
    resolved: dict[str, str | None] = dict.fromkeys(fields)
    origins: dict[str, str] = {}
    for label, source in candidates:
        for key, value in source.items():
            if resolved[key] is None and value:
                resolved[key] = value
                origins[key] = label
    return resolved, origins


def load_api_key(var: str, *, env_file: Path | None = None) -> str | None:
    """単体の API キー（J-Quants など、口座と紐づかないデータ源の鍵）を解決する。

    環境変数 → ``.env`` の順に ``var`` の名前で探す。見つからなければ None を
    返し、どう設定するかの案内は呼び出し側（取得元）が出す。キーチェーンは
    使わない（口座の認証情報と違い、環境ごとに分ける意味が無い）。
    """
    value = os.environ.get(var)
    if value:
        return value
    return _read_dotenv(_resolve_env_file(env_file)).get(var) or None


def credential_source(
    env: Environment,
    *,
    env_file: Path | None = None,
    namespace: str = TACHIBANA_NAMESPACE,
    fields: tuple[str, ...] = TACHIBANA_CREDENTIAL_FIELDS,
) -> str:
    """認証情報がどこから来たかを人間向けに説明する。

    どこから読まれているか分からないまま本番に発注する事故を防ぐための、
    ``credentials check`` 用の診断。秘密そのものは返さない。

    **実際に採用されるものを説明すること。** 項目ごとに取得元が違いうるので、
    1 項目だけを見て「.env から読んだ」と表示すると嘘になる。
    """
    path = _resolve_env_file(env_file)
    resolved, origins = _resolve_fields(env, _read_dotenv(path), path, namespace, fields=fields)

    missing = [k for k, v in resolved.items() if not v]
    if missing:
        return f"解決できません（{', '.join(missing)} が未設定）"

    labels = set(origins.values())
    if len(labels) == 1:
        return labels.pop()
    # 項目ごとにソースが違うのは事故のもと。どれがどこから来たかを出す。
    return ", ".join(f"{k}={origins[k]}" for k in fields)


# --------------------------------------------------------------------------
# 立花証券（e支店 API）
# --------------------------------------------------------------------------



@dataclass(frozen=True, slots=True)
class TachibanaCredentials:
    """立花証券 e支店 API（e_api_v4r10）の認証情報。

    ``__repr__`` を潰してあるので、うっかりログや例外に出しても秘密が漏れない。
    """

    auth_id: str
    #: 秘密鍵（PEM、``-----BEGIN PRIVATE KEY-----`` から）。
    private_key_pem: bytes
    order_password: str

    def __repr__(self) -> str:
        return f"TachibanaCredentials(auth_id='***{self.auth_id[-2:]}')"

    __str__ = __repr__


def load_tachibana_credentials(
    env: Environment,
    *,
    env_file: Path | None = None,
    namespace: str = TACHIBANA_NAMESPACE,
) -> TachibanaCredentials:
    """立花証券の認証情報を解決する。

    優先順位は環境変数 → ``.env`` → OS キーチェーン。秘密鍵は
    ``private_key_file`` のパスから読む（``chmod 600`` のこと）。

    Raises:
        MissingCredentialsError: どこにも見つからない、または秘密鍵のファイルが無いとき。
    """
    path = _resolve_env_file(env_file)
    dotenv = _read_dotenv(path)
    resolved, _ = _resolve_fields(
        env, dotenv, path, namespace, fields=TACHIBANA_CREDENTIAL_FIELDS
    )

    if all(resolved.values()):
        key_path = Path(str(resolved["private_key_file"])).expanduser()
        if not key_path.is_file():
            raise MissingCredentialsError(
                f"立花証券の秘密鍵ファイルがありません: {key_path}"
                f"（{namespace}_{env.value.upper()}_PRIVATE_KEY_FILE）"
            )
        _warn_if_readable_by_others(key_path)
        return TachibanaCredentials(
            auth_id=str(resolved["auth_id"]),
            private_key_pem=key_path.read_bytes(),
            order_password=str(resolved["order_password"]),
        )

    missing = [k for k, v in resolved.items() if not v]
    upper = env.value.upper()
    raise MissingCredentialsError(
        f"{env.value} 環境の認証情報（{namespace}）が不足しています: {', '.join(missing)}\n"
        f"  環境変数:     {namespace}_{upper}_AUTH_ID / _PRIVATE_KEY_FILE / _ORDER_PASSWORD\n"
        f"  または .env に記載: {namespace}_{upper}_AUTH_ID=... （chmod 600 のこと）"
    )
