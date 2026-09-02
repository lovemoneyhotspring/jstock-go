"""設定と認証情報のテスト。

とくに「本番で意図せず実発注される」経路が存在しないことを確認する。
"""

from __future__ import annotations

import sys
from decimal import Decimal
from pathlib import Path

import pytest
from structlog.testing import capture_logs

from wbcore.credentials import (
    TachibanaCredentials,
    credential_source,
    load_tachibana_credentials,
)
from wbjp.config import (
    AppSettings,
    Config,
    Environment,
    FileConfig,
    MissingCredentialsError,
    RiskConfig,
    load_file_config,
)


@pytest.fixture(autouse=True)
def _clear_wbjp_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """テスト間で環境変数が漏れないようにする。

    開発者のリポジトリに実在する ``.env`` も遮断する。存在しない
    パスを既定にしておかないと、手元にだけ通る（あるいは落ちる）
    テストになってしまう。
    """
    import os

    for key in list(os.environ):
        if key.startswith(("WBJP_", "TACHIBANA_")):
            monkeypatch.delenv(key, raising=False)
    monkeypatch.setattr("wbcore.credentials.DEFAULT_ENV_FILE", tmp_path / "absent.env")


def _write_env_file(path: Path, body: str, *, mode: int = 0o600) -> Path:
    path.write_text(body, encoding="utf-8")
    path.chmod(mode)
    return path


@pytest.fixture
def key_file(tmp_path: Path) -> Path:
    """秘密鍵のファイル（中身は読めればよい。復号はブローカー側のテストで見る）。"""
    return _write_env_file(tmp_path / "key.pem", "-----BEGIN PRIVATE KEY-----\nMII\n")


def _set_env(monkeypatch: pytest.MonkeyPatch, key_file: Path, env: str = "PROD") -> None:
    monkeypatch.setenv(f"TACHIBANA_{env}_AUTH_ID", "from-env")
    monkeypatch.setenv(f"TACHIBANA_{env}_PRIVATE_KEY_FILE", str(key_file))
    monkeypatch.setenv(f"TACHIBANA_{env}_ORDER_PASSWORD", "pw-env")


def _dotenv_body(key_file: Path, env: str = "PROD", auth_id: str = "dotenv-id") -> str:
    return (
        f"TACHIBANA_{env}_AUTH_ID={auth_id}\n"
        f"TACHIBANA_{env}_PRIVATE_KEY_FILE={key_file}\n"
        f"TACHIBANA_{env}_ORDER_PASSWORD=dotenv-pw\n"
    )


_NO_KEYRING = lambda env, *_: {}  # noqa: E731


def _config(*, env: Environment, kill_switch: bool = False) -> Config:
    return Config(
        settings=AppSettings(env=env),
        file=FileConfig(risk=RiskConfig(kill_switch=kill_switch)),
    )


# --------------------------------------------------------------------------
# 実発注の二重ロック — ここが最重要
# --------------------------------------------------------------------------


def test_no_orders_without_live_flag_in_any_environment() -> None:
    """--live を付けない限り、どの口座でも発注しない（データ取得と判断は行う）。"""
    for env in Environment:
        allowed, reason = _config(env=env).allows_live_orders(live_flag=False)
        assert allowed is False
        assert "--live なし" in reason


def test_live_flag_required_in_production() -> None:
    allowed, _ = _config(env=Environment.PROD).allows_live_orders(live_flag=False)
    assert allowed is False


def test_production_with_live_flag_is_allowed() -> None:
    allowed, reason = _config(env=Environment.PROD).allows_live_orders(live_flag=True)
    assert allowed is True
    assert reason == "--live あり"


def test_kill_switch_overrides_everything() -> None:
    """キルスイッチは --live より強い。"""
    allowed, reason = _config(env=Environment.PROD, kill_switch=True).allows_live_orders(
        live_flag=True
    )
    assert allowed is False
    assert "キルスイッチ" in reason


def test_uat_with_live_flag_is_allowed_and_the_account_is_named_in_the_mode_line() -> None:
    """発注の可否は --live だけ。口座の区別（実弾かどうか）は describe_mode が示す。"""
    from wbcore.settings import describe_mode

    allowed, reason = _config(env=Environment.UAT).allows_live_orders(live_flag=True)
    assert allowed is True and reason == "--live あり"
    assert "実弾ではない" in describe_mode(Environment.UAT, True)


# --------------------------------------------------------------------------
# 認証情報（立花証券）
# --------------------------------------------------------------------------


def test_missing_credentials_are_an_error_in_every_environment(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """認証情報が無ければ、どの環境でも黙って代わりの口座に落ちたりしない。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)

    for env in (Environment.PROD, Environment.UAT):
        with pytest.raises(MissingCredentialsError, match="TACHIBANA"):
            load_tachibana_credentials(env)


def test_env_vars_take_priority_over_keyring(
    monkeypatch: pytest.MonkeyPatch, key_file: Path
) -> None:
    monkeypatch.setattr(
        "wbcore.credentials._from_keyring",
        lambda env, *_: {
            "auth_id": "kr",
            "private_key_file": str(key_file),
            "order_password": "kr",
        },
    )
    _set_env(monkeypatch, key_file)

    creds = load_tachibana_credentials(Environment.PROD)

    assert creds.auth_id == "from-env"
    assert creds.private_key_pem.startswith(b"-----BEGIN PRIVATE KEY-----")


def test_environment_scoped_env_var_beats_generic(
    monkeypatch: pytest.MonkeyPatch, key_file: Path
) -> None:
    """環境別の変数が、共通の変数より優先される。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    monkeypatch.setenv("TACHIBANA_AUTH_ID", "generic")
    monkeypatch.setenv("TACHIBANA_PROD_AUTH_ID", "prod-specific")
    monkeypatch.setenv("TACHIBANA_PRIVATE_KEY_FILE", str(key_file))
    monkeypatch.setenv("TACHIBANA_ORDER_PASSWORD", "pw")

    assert load_tachibana_credentials(Environment.PROD).auth_id == "prod-specific"


def test_missing_credentials_error_names_what_is_missing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    monkeypatch.setenv("TACHIBANA_PROD_AUTH_ID", "only-id")

    with pytest.raises(MissingCredentialsError, match="order_password"):
        load_tachibana_credentials(Environment.PROD)


def test_missing_private_key_file_is_an_error(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    _set_env(monkeypatch, tmp_path / "absent.pem")

    with pytest.raises(MissingCredentialsError, match="秘密鍵"):
        load_tachibana_credentials(Environment.PROD)


# --------------------------------------------------------------------------
# .env — キーチェーンの無い Linux サーバー向けの経路
# --------------------------------------------------------------------------


def test_dotenv_supplies_credentials(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    """.env に書いた鍵が実際に使われる（環境変数への export 不要）。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    env_file = _write_env_file(tmp_path / ".env", _dotenv_body(key_file))

    creds = load_tachibana_credentials(Environment.PROD, env_file=env_file)

    assert creds.auth_id == "dotenv-id"
    assert creds.order_password == "dotenv-pw"


def test_dotenv_does_not_leak_into_os_environ(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    """.env の秘密がプロセス環境に入らない。

    入ってしまうと子プロセスと /proc/<pid>/environ から読めてしまう。
    """
    import os

    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    env_file = _write_env_file(tmp_path / ".env", _dotenv_body(key_file))

    load_tachibana_credentials(Environment.PROD, env_file=env_file)

    assert "TACHIBANA_PROD_ORDER_PASSWORD" not in os.environ


def test_real_env_vars_beat_dotenv(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    """デプロイ時の環境変数で .env を上書きできる。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    monkeypatch.setenv("TACHIBANA_PROD_AUTH_ID", "from-env")
    env_file = _write_env_file(tmp_path / ".env", _dotenv_body(key_file))

    creds = load_tachibana_credentials(Environment.PROD, env_file=env_file)

    assert creds.auth_id == "from-env"
    assert creds.order_password == "dotenv-pw"  # 項目ごとに解決される


def test_dotenv_beats_keyring(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    monkeypatch.setattr(
        "wbcore.credentials._from_keyring",
        lambda env, *_: {
            "auth_id": "kr",
            "private_key_file": str(key_file),
            "order_password": "kr",
        },
    )
    env_file = _write_env_file(tmp_path / ".env", _dotenv_body(key_file))

    assert load_tachibana_credentials(Environment.PROD, env_file=env_file).auth_id == "dotenv-id"


def test_loose_permissions_on_dotenv_are_warned(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    import structlog

    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    # 他のテストで束縛済みのロガーは capture_logs を通らないので、未束縛のものに差し替える
    monkeypatch.setattr("wbcore.credentials.log", structlog.get_logger("wbcore.credentials"))
    env_file = _write_env_file(tmp_path / ".env", _dotenv_body(key_file), mode=0o644)

    with capture_logs() as entries:
        load_tachibana_credentials(Environment.PROD, env_file=env_file)

    assert any("chmod 600" in e["event"] for e in entries)


def test_tight_permissions_on_dotenv_are_quiet(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    env_file = _write_env_file(tmp_path / ".env", _dotenv_body(key_file))

    with capture_logs() as entries:
        load_tachibana_credentials(Environment.PROD, env_file=env_file)

    assert not any("chmod 600" in e["event"] for e in entries)


def test_env_file_override_locates_dotenv_outside_cwd(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    """cron は $HOME で起動する。相対パスに頼らず絶対パスで指定できる。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    env_file = _write_env_file(tmp_path / "elsewhere.env", _dotenv_body(key_file))
    monkeypatch.setenv("WBJP_ENV_FILE", str(env_file))

    assert load_tachibana_credentials(Environment.PROD).auth_id == "dotenv-id"


def test_missing_dotenv_is_not_an_error(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    _set_env(monkeypatch, key_file)

    creds = load_tachibana_credentials(Environment.PROD, env_file=tmp_path / "absent.env")

    assert creds.auth_id == "from-env"


# --------------------------------------------------------------------------
# キーチェーンの無いホスト
# --------------------------------------------------------------------------


class _NoBackend:
    @staticmethod
    def get_password(service: str, name: str) -> str:
        raise RuntimeError("No recommended backend was available")


def test_unavailable_keyring_does_not_crash(monkeypatch: pytest.MonkeyPatch) -> None:
    """ヘッドレスな Linux では keyring が例外を投げる。

    そこで落ちると、設定方法を案内する MissingCredentialsError に
    到達できない。
    """
    monkeypatch.setitem(sys.modules, "keyring", _NoBackend)

    with pytest.raises(MissingCredentialsError, match="auth_id"):
        load_tachibana_credentials(Environment.PROD)


def test_unavailable_keyring_still_allows_env_vars(
    monkeypatch: pytest.MonkeyPatch, key_file: Path
) -> None:
    monkeypatch.setitem(sys.modules, "keyring", _NoBackend)
    _set_env(monkeypatch, key_file)

    assert load_tachibana_credentials(Environment.PROD).auth_id == "from-env"


# --------------------------------------------------------------------------
# 取得元の診断
# --------------------------------------------------------------------------


def test_credential_source_reports_where_the_key_came_from(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    env_file = _write_env_file(tmp_path / ".env", _dotenv_body(key_file))

    assert credential_source(Environment.PROD, env_file=env_file) == str(env_file)


def test_credential_source_reports_what_is_missing(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """1項目でも欠ければ解決できない。診断もそう言うこと。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    env_file = _write_env_file(tmp_path / ".env", "TACHIBANA_UAT_AUTH_ID=k\n")

    source = credential_source(Environment.UAT, env_file=env_file)

    assert "解決できません" in source
    assert "order_password" in source


def test_credential_source_flags_mixed_origins(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, key_file: Path
) -> None:
    """項目ごとにソースが違うなら、その内訳を出す。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", _NO_KEYRING)
    monkeypatch.setenv("TACHIBANA_PROD_AUTH_ID", "from-env")
    env_file = _write_env_file(tmp_path / ".env", _dotenv_body(key_file))

    source = credential_source(Environment.PROD, env_file=env_file)

    assert "auth_id=環境変数" in source
    assert f"order_password={env_file}" in source


def test_credentials_repr_hides_the_secret() -> None:
    """うっかりログや例外に出しても漏れない。"""
    creds = TachibanaCredentials(
        auth_id="AUTHID12345", private_key_pem=b"supersecretkey", order_password="9876"
    )

    for rendered in (repr(creds), str(creds), f"{creds}"):
        assert "supersecretkey" not in rendered
        assert "9876" not in rendered
        assert "AUTHID12345" not in rendered


def test_db_path_is_separated_per_environment() -> None:
    """UAT の取引履歴が本番のDBに混ざらないようにする。"""
    uat = AppSettings(env=Environment.UAT).db_path
    prod = AppSettings(env=Environment.PROD).db_path
    assert uat != prod


# --------------------------------------------------------------------------
# ファイル設定
# --------------------------------------------------------------------------


def test_load_file_config_with_no_files_uses_defaults(tmp_path: Path) -> None:
    config = load_file_config(tmp_path)
    assert config.risk.kill_switch is False
    assert config.strategies.strategies == []


def test_load_file_config_reads_both_files(tmp_path: Path) -> None:
    (tmp_path / "settings.toml").write_text(
        """
[universe]
symbols = ["7203", "6758"]
topix500_symbols = ["7203"]

[risk]
kill_switch = true
max_order_value_jpy = "300000"
""",
        encoding="utf-8",
    )
    (tmp_path / "strategies.toml").write_text(
        """
combiner = "majority"
entry_threshold = 0.4
exit_threshold = 0.2

[[strategies]]
name = "sma_cross"
weight = 2.0
fast = 5
slow = 20
""",
        encoding="utf-8",
    )

    config = load_file_config(tmp_path)

    assert config.universe.symbols == ["7203", "6758"]
    assert config.risk.kill_switch is True
    # 旧名 max_order_value_jpy でも読める（後方互換）
    assert config.risk.max_order_value == Decimal("300000")
    assert config.strategies.combiner == "majority"

    entry = config.strategies.strategies[0]
    assert entry.name == "sma_cross"
    assert entry.weight == 2.0
    assert entry.params == {"fast": 5, "slow": 20}


def test_topix500_must_be_subset_of_universe(tmp_path: Path) -> None:
    """呼値の判定に使うため、対象外の銘柄が紛れ込むのを防ぐ。"""
    (tmp_path / "settings.toml").write_text(
        """
[universe]
symbols = ["7203"]
topix500_symbols = ["9999"]
""",
        encoding="utf-8",
    )
    with pytest.raises(ValueError, match="topix500_symbols"):
        load_file_config(tmp_path)


def test_exit_threshold_must_not_exceed_entry_threshold(tmp_path: Path) -> None:
    (tmp_path / "strategies.toml").write_text(
        "entry_threshold = 0.2\nexit_threshold = 0.5\n", encoding="utf-8"
    )
    with pytest.raises(ValueError, match="exit_threshold"):
        load_file_config(tmp_path)


def test_unknown_config_key_is_rejected(tmp_path: Path) -> None:
    """設定ミスを黙って無視しない（typo で上限が効かなくなるのを防ぐ）。"""
    (tmp_path / "settings.toml").write_text("[risk]\nmax_order_valeu = 100\n", encoding="utf-8")
    with pytest.raises(ValueError, match="max_order_valeu"):
        load_file_config(tmp_path)


def test_risk_ratios_must_be_within_zero_to_one() -> None:
    with pytest.raises(ValueError, match="比率"):
        RiskConfig(max_position_weight=Decimal("1.5"))


def test_sizing_method_must_be_known(tmp_path: Path) -> None:
    (tmp_path / "settings.toml").write_text('[sizing]\nmethod = "martingale"\n', encoding="utf-8")
    with pytest.raises(ValueError, match="method"):
        load_file_config(tmp_path)


def test_enabled_strategies_filters_disabled(tmp_path: Path) -> None:
    (tmp_path / "strategies.toml").write_text(
        """
[[strategies]]
name = "a"

[[strategies]]
name = "b"
enabled = false
""",
        encoding="utf-8",
    )
    config = load_file_config(tmp_path)
    assert [s.name for s in config.strategies.enabled] == ["a"]
