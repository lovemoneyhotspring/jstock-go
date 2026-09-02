"""ブローカーの登録簿と、取引所を差し替える仕組み。

「Broker を継承して name と connect を書き、登録する」だけで、設定の
``execution.broker`` から選べるようになることを確認する。
"""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path
from typing import ClassVar, Self

import pytest

from wbcore.broker.base import Broker
from wbcore.broker.paper import PaperBroker
from wbcore.broker.registry import BROKERS, available, connect
from wbcore.credentials import (
    Environment,
    MissingCredentialsError,
    load_tachibana_credentials,
)
from wbcore.domain.models import Market, TaxAccountType


@pytest.fixture(autouse=True)
def _isolate(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    import os

    for key in list(os.environ):
        if key.startswith(("WBJP_", "TACHIBANA_", "OTHER_")):
            monkeypatch.delenv(key, raising=False)
    monkeypatch.setattr("wbcore.credentials.DEFAULT_ENV_FILE", tmp_path / "absent.env")
    monkeypatch.setattr("wbcore.credentials._from_keyring", lambda env, *_: {})
    # 登録簿への追加をテストの外に漏らさない
    monkeypatch.setattr(BROKERS, "_items", dict(BROKERS._items))


# --- 登録簿 -------------------------------------------------------------


def test_builtin_brokers_are_registered() -> None:
    assert {"paper"} <= set(available())


def test_unknown_broker_lists_the_alternatives() -> None:
    with pytest.raises(ValueError, match="未知のブローカー 'nope'。利用可能:"):
        connect("nope", Environment.UAT, market=Market.JP)


def test_broker_without_a_name_is_rejected_at_registration() -> None:
    class Nameless(PaperBroker):
        pass

    Nameless.name = ""
    with pytest.raises(ValueError, match="name が設定されていません"):
        BROKERS.register(Nameless)


# --- 差し替え -------------------------------------------------------------


def test_paper_broker_connects_without_network_and_follows_the_market() -> None:
    notices: list[str] = []
    broker = connect("paper", Environment.UAT, market=Market.US, notify=notices.append)
    assert isinstance(broker, PaperBroker)
    assert broker.currency == "USD"
    assert notices and "ペーパー口座" in notices[0]


def test_a_new_exchange_plugs_in_by_subclassing() -> None:
    """取引所を足す手順そのもの: 継承 → name と connect → 登録 → 名前で接続。"""
    seen: dict[str, object] = {}

    class DummyExchange(PaperBroker):
        name: ClassVar[str] = "dummy"

        @classmethod
        def connect(
            cls,
            env: Environment,
            *,
            market: Market,
            tax_type: TaxAccountType = TaxAccountType.SPECIFIC,
            notify: Callable[[str], None] | None = None,
        ) -> Self:
            seen.update(env=env, market=market, tax_type=tax_type)
            return cls(currency=market.currency, tax_type=tax_type)

    BROKERS.register(DummyExchange)
    broker = connect("dummy", Environment.PROD, market=Market.JP, tax_type=TaxAccountType.NISA)
    assert isinstance(broker, DummyExchange)
    assert isinstance(broker, Broker)
    assert seen == {"env": Environment.PROD, "market": Market.JP, "tax_type": TaxAccountType.NISA}


# --- 認証情報の名前空間 ---------------------------------------------------


def test_credential_namespaces_do_not_leak_into_each_other(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """別の名前空間のキーが立花証券に流れ込まない（逆も）。"""
    key = tmp_path / "k.pem"
    key.write_text("-----BEGIN PRIVATE KEY-----\n", encoding="utf-8")
    monkeypatch.setenv("OTHER_PROD_AUTH_ID", "oid")
    monkeypatch.setenv("OTHER_PROD_PRIVATE_KEY_FILE", str(key))
    monkeypatch.setenv("OTHER_PROD_ORDER_PASSWORD", "opw")

    other = load_tachibana_credentials(Environment.PROD, namespace="OTHER")
    assert other.auth_id == "oid"

    with pytest.raises(MissingCredentialsError, match="（TACHIBANA）"):
        load_tachibana_credentials(Environment.PROD)
