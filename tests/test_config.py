"""設定と認証情報のテスト。

とくに「本番で意図せず実発注される」経路が存在しないことを確認する。
"""

from __future__ import annotations

import datetime as dt
from decimal import Decimal
from pathlib import Path

import pytest

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
    load_credentials,
    load_file_config,
)


@pytest.fixture(autouse=True)
def _clear_wbjp_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """テスト間で環境変数が漏れないようにする。"""
    import os

    for key in list(os.environ):
        if key.startswith("WBJP_"):
            monkeypatch.delenv(key, raising=False)


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
    monkeypatch.setattr("wbjp.config._from_keyring", lambda env: {})

    with pytest.raises(MissingCredentialsError) as exc:
        load_credentials(Environment.PROD)

    assert PUBLIC_UAT_CREDENTIALS["app_key"] not in str(exc.value)


def test_uat_falls_back_to_public_test_account(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("wbjp.config._from_keyring", lambda env: {})

    creds = load_credentials(Environment.UAT)

    assert creds.is_public_test_account is True
    assert creds.app_key == PUBLIC_UAT_CREDENTIALS["app_key"]


def test_public_fallback_can_be_disabled(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("wbjp.config._from_keyring", lambda env: {})

    with pytest.raises(MissingCredentialsError):
        load_credentials(Environment.UAT, allow_public_test_account=False)


def test_env_vars_take_priority_over_keyring(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "wbjp.config._from_keyring",
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
    monkeypatch.setattr("wbjp.config._from_keyring", lambda env: {})
    monkeypatch.setenv("WBJP_APP_KEY", "generic")
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "prod-specific")
    monkeypatch.setenv("WBJP_APP_SECRET", "s3cr3t-value")
    monkeypatch.setenv("WBJP_ACCOUNT_ID", "acct")

    assert load_credentials(Environment.PROD).app_key == "prod-specific"


def test_missing_credentials_error_names_what_is_missing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr("wbjp.config._from_keyring", lambda env: {})
    monkeypatch.setenv("WBJP_PROD_APP_KEY", "only-key")

    with pytest.raises(MissingCredentialsError, match="app_secret"):
        load_credentials(Environment.PROD)


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
    assert config.risk.max_order_value_jpy == Decimal("300000")
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
    (tmp_path / "settings.toml").write_text("[risk]\nmax_order_value = 100\n", encoding="utf-8")
    with pytest.raises(ValueError, match="max_order_value"):
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
