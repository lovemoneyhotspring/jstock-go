"""設定と認証情報のテスト。

とくに「本番で意図せず実発注される」経路が存在しないことを確認する。
"""

from __future__ import annotations

import datetime as dt
import sys
from decimal import Decimal
from pathlib import Path

import pytest
from structlog.testing import capture_logs

from wbjp.config import (
    ENDPOINTS,
    PUBLIC_UAT_CREDENTIALS,
    AppSettings,
    Config,
    Credentials,
    Environment,
    FileConfig,
    MissingCredentialsError,
    RiskConfig,
    credential_source,
    load_credentials,
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
        if key.startswith("WBJP_"):
            monkeypatch.delenv(key, raising=False)
    monkeypatch.setattr("wbcore.credentials.DEFAULT_ENV_FILE", tmp_path / "absent.env")


def _write_env_file(path: Path, body: str, *, mode: int = 0o600) -> Path:
    path.write_text(body, encoding="utf-8")
    path.chmod(mode)
    return path


def _config(*, env: Environment, kill_switch: bool = False) -> Config:
    return Config(
        settings=AppSettings(env=env),
        file=FileConfig(risk=RiskConfig(kill_switch=kill_switch)),
    )


# --------------------------------------------------------------------------
# 実発注の二重ロック — ここが最重要
# --------------------------------------------------------------------------


def test_dry_run_is_the_default_everywhere() -> None:
    """--live を付けない限り、どの環境でも発注しない。"""
    for env in Environment:
        allowed, reason = _config(env=env).allows_live_orders(live_flag=False)
        assert allowed is False
        assert "dry-run" in reason


def test_live_flag_required_in_production() -> None:
    allowed, _ = _config(env=Environment.PROD).allows_live_orders(live_flag=False)
    assert allowed is False


def test_production_with_live_flag_is_allowed() -> None:
    allowed, reason = _config(env=Environment.PROD).allows_live_orders(live_flag=True)
    assert allowed is True
    assert "本番" in reason


def test_kill_switch_overrides_everything() -> None:
    """キルスイッチは --live より強い。"""
    allowed, reason = _config(env=Environment.PROD, kill_switch=True).allows_live_orders(
        live_flag=True
    )
    assert allowed is False
    assert "キルスイッチ" in reason


def test_uat_with_live_flag_is_marked_as_not_real_money() -> None:
    allowed, reason = _config(env=Environment.UAT).allows_live_orders(live_flag=True)
    assert allowed is True
    assert "実弾ではない" in reason


# --------------------------------------------------------------------------
# 認証情報
# --------------------------------------------------------------------------


def test_production_never_falls_back_to_public_test_account(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """本番で公開テスト口座が使われる経路は存在しない。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})

    with pytest.raises(MissingCredentialsError) as exc:
        load_credentials(Environment.PROD)

    assert PUBLIC_UAT_CREDENTIALS["app_key"] not in str(exc.value)


def test_uat_falls_back_to_public_test_account(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})

    creds = load_credentials(Environment.UAT)

    assert creds.is_public_test_account is True
    assert creds.app_key == PUBLIC_UAT_CREDENTIALS["app_key"]


def test_public_fallback_can_be_disabled(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})

    with pytest.raises(MissingCredentialsError):
        load_credentials(Environment.UAT, allow_public_test_account=False)


def test_env_vars_take_priority_over_keyring(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "wbcore.credentials._from_keyring",
        lambda env: {"app_key": "kr", "app_secret": "kr", "account_id": "kr"},
    )
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "from-env")
    monkeypatch.setenv("WBJP_PROD_APP_SECRET", "secret-env")
    monkeypatch.setenv("WBJP_PROD_ACCOUNT_ID", "acct-env")

    creds = load_credentials(Environment.PROD)

    assert creds.app_key == "from-env"
    assert creds.is_public_test_account is False


def test_environment_scoped_env_var_beats_generic(monkeypatch: pytest.MonkeyPatch) -> None:
    """環境別の変数が、共通の変数より優先される。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    monkeypatch.setenv("WBJP_APP_KEY", "generic")
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "prod-specific")
    monkeypatch.setenv("WBJP_APP_SECRET", "s3cr3t-value")
    monkeypatch.setenv("WBJP_ACCOUNT_ID", "acct")

    assert load_credentials(Environment.PROD).app_key == "prod-specific"


def test_missing_credentials_error_names_what_is_missing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "only-key")

    with pytest.raises(MissingCredentialsError, match="app_secret"):
        load_credentials(Environment.PROD)


# --------------------------------------------------------------------------
# .env — キーチェーンの無い Linux サーバー向けの経路
# --------------------------------------------------------------------------


def test_dotenv_supplies_credentials(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """.env に書いた鍵が実際に使われる（環境変数への export 不要）。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    env_file = _write_env_file(
        tmp_path / ".env",
        "WBJP_PROD_APP_KEY=dotenv-key\n"
        "WBJP_PROD_APP_SECRET=dotenv-secret\n"
        "WBJP_PROD_ACCOUNT_ID=dotenv-acct\n",
    )

    creds = load_credentials(Environment.PROD, env_file=env_file)

    assert creds.app_key == "dotenv-key"
    assert creds.account_id == "dotenv-acct"


def test_dotenv_does_not_leak_into_os_environ(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """.env の秘密がプロセス環境に入らない。

    入ってしまうと子プロセスと /proc/<pid>/environ から読めてしまう。
    """
    import os

    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    env_file = _write_env_file(
        tmp_path / ".env",
        "WBJP_PROD_APP_KEY=k\nWBJP_PROD_APP_SECRET=leaky-secret\nWBJP_PROD_ACCOUNT_ID=a\n",
    )

    load_credentials(Environment.PROD, env_file=env_file)

    assert "WBJP_PROD_APP_SECRET" not in os.environ


def test_real_env_vars_beat_dotenv(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """デプロイ時の環境変数で .env を上書きできる。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "from-env")
    env_file = _write_env_file(
        tmp_path / ".env",
        "WBJP_PROD_APP_KEY=from-dotenv\n"
        "WBJP_PROD_APP_SECRET=dotenv-secret\n"
        "WBJP_PROD_ACCOUNT_ID=dotenv-acct\n",
    )

    creds = load_credentials(Environment.PROD, env_file=env_file)

    assert creds.app_key == "from-env"
    assert creds.app_secret == "dotenv-secret"  # 項目ごとに解決される


def test_dotenv_beats_keyring(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setattr(
        "wbcore.credentials._from_keyring",
        lambda env: {"app_key": "kr", "app_secret": "kr", "account_id": "kr"},
    )
    env_file = _write_env_file(
        tmp_path / ".env",
        "WBJP_PROD_APP_KEY=dotenv-key\n"
        "WBJP_PROD_APP_SECRET=dotenv-secret\n"
        "WBJP_PROD_ACCOUNT_ID=dotenv-acct\n",
    )

    assert load_credentials(Environment.PROD, env_file=env_file).app_key == "dotenv-key"


def test_dotenv_supplies_key_created_on(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """失効警告が .env 運用でも効く。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    env_file = _write_env_file(
        tmp_path / ".env",
        "WBJP_PROD_APP_KEY=k\n"
        "WBJP_PROD_APP_SECRET=s\n"
        "WBJP_PROD_ACCOUNT_ID=a\n"
        "WBJP_PROD_KEY_CREATED_ON=2026-01-01\n",
    )

    creds = load_credentials(Environment.PROD, env_file=env_file)

    assert creds.created_on == dt.date(2026, 1, 1)
    assert creds.expires_on == dt.date(2026, 2, 15)


def test_loose_permissions_on_dotenv_are_warned(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    env_file = _write_env_file(
        tmp_path / ".env",
        "WBJP_PROD_APP_KEY=k\nWBJP_PROD_APP_SECRET=s\nWBJP_PROD_ACCOUNT_ID=a\n",
        mode=0o644,
    )

    with capture_logs() as entries:
        load_credentials(Environment.PROD, env_file=env_file)

    assert any("chmod 600" in e["event"] for e in entries)


def test_tight_permissions_on_dotenv_are_quiet(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    env_file = _write_env_file(
        tmp_path / ".env",
        "WBJP_PROD_APP_KEY=k\nWBJP_PROD_APP_SECRET=s\nWBJP_PROD_ACCOUNT_ID=a\n",
    )

    with capture_logs() as entries:
        load_credentials(Environment.PROD, env_file=env_file)

    assert not any("chmod 600" in e["event"] for e in entries)


def test_env_file_override_locates_dotenv_outside_cwd(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """cron は $HOME で起動する。相対パスに頼らず絶対パスで指定できる。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    env_file = _write_env_file(
        tmp_path / "elsewhere.env",
        "WBJP_PROD_APP_KEY=k\nWBJP_PROD_APP_SECRET=s\nWBJP_PROD_ACCOUNT_ID=a\n",
    )
    monkeypatch.setenv("WBJP_ENV_FILE", str(env_file))

    assert load_credentials(Environment.PROD).app_key == "k"


def test_missing_dotenv_is_not_an_error(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "k")
    monkeypatch.setenv("WBJP_PROD_APP_SECRET", "s")
    monkeypatch.setenv("WBJP_PROD_ACCOUNT_ID", "a")

    creds = load_credentials(Environment.PROD, env_file=tmp_path / "absent.env")

    assert creds.app_key == "k"


# --------------------------------------------------------------------------
# キーチェーンの無いホスト
# --------------------------------------------------------------------------


def test_unavailable_keyring_does_not_crash(monkeypatch: pytest.MonkeyPatch) -> None:
    """ヘッドレスな Linux では keyring が例外を投げる。

    そこで落ちると、設定方法を案内する MissingCredentialsError に
    到達できない。
    """

    class _NoBackend:
        @staticmethod
        def get_password(service: str, name: str) -> str:
            raise RuntimeError("No recommended backend was available")

    monkeypatch.setitem(sys.modules, "keyring", _NoBackend)

    with pytest.raises(MissingCredentialsError, match="app_key"):
        load_credentials(Environment.PROD)


def test_unavailable_keyring_still_allows_env_vars(monkeypatch: pytest.MonkeyPatch) -> None:
    class _NoBackend:
        @staticmethod
        def get_password(service: str, name: str) -> str:
            raise RuntimeError("No recommended backend was available")

    monkeypatch.setitem(sys.modules, "keyring", _NoBackend)
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "k")
    monkeypatch.setenv("WBJP_PROD_APP_SECRET", "s")
    monkeypatch.setenv("WBJP_PROD_ACCOUNT_ID", "a")

    assert load_credentials(Environment.PROD).app_key == "k"


# --------------------------------------------------------------------------
# 取得元の診断
# --------------------------------------------------------------------------


_FULL_SET = "WBJP_PROD_APP_KEY=k\nWBJP_PROD_APP_SECRET=s\nWBJP_PROD_ACCOUNT_ID=a\n"


def test_credential_source_reports_where_the_key_came_from(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    env_file = _write_env_file(tmp_path / ".env", _FULL_SET)

    assert credential_source(Environment.PROD, env_file=env_file) == str(env_file)


def test_credential_source_matches_what_was_actually_used(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """1項目でも欠ければ公開テスト口座に落ちる。診断もそう言うこと。

    「.env から読んだ」と表示しながら実際は共有テスト口座、という
    食い違いが起きると、他人の口座を自分の口座だと思って眺めることになる。
    """
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    env_file = _write_env_file(
        tmp_path / ".env",
        "WBJP_UAT_APP_KEY=k\nWBJP_UAT_APP_SECRET=s\n",  # account_id が無い
    )

    creds = load_credentials(Environment.UAT, env_file=env_file)
    source = credential_source(Environment.UAT, env_file=env_file)

    assert creds.is_public_test_account is True
    assert "公開テスト口座" in source
    assert "account_id" in source
    assert str(env_file) not in source


def test_credential_source_flags_mixed_origins(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """項目ごとにソースが違うなら、その内訳を出す。"""
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env: {})
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "from-env")
    env_file = _write_env_file(tmp_path / ".env", _FULL_SET)

    source = credential_source(Environment.PROD, env_file=env_file)

    assert "app_key=環境変数" in source
    assert f"account_id={env_file}" in source


def test_credentials_repr_hides_the_secret() -> None:
    """うっかりログや例外に出しても漏れない。"""
    creds = Credentials(app_key="key12345678", app_secret="supersecret", account_id="12345")

    for rendered in (repr(creds), str(creds), f"{creds}"):
        assert "supersecret" not in rendered
        assert "key12345678" not in rendered
        assert "12345" not in rendered


def test_credentials_expiry_calculation() -> None:
    creds = Credentials(app_key="k", app_secret="s", account_id="a", created_on=dt.date(2026, 8, 1))
    assert creds.expires_on == dt.date(2026, 9, 15)  # 45日後
    assert creds.days_until_expiry(dt.date(2026, 9, 10)) == 5


def test_credentials_without_created_on_has_no_expiry() -> None:
    creds = Credentials(app_key="k", app_secret="s", account_id="a")
    assert creds.expires_on is None
    assert creds.days_until_expiry() is None


# --------------------------------------------------------------------------
# 接続先
# --------------------------------------------------------------------------


def test_endpoints_are_distinct_per_environment() -> None:
    """UAT と本番が同じ接続先を向くことは絶対にない。"""
    assert ENDPOINTS[Environment.UAT].trade != ENDPOINTS[Environment.PROD].trade
    assert ENDPOINTS[Environment.PROD].trade == "api.webull.co.jp"


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
