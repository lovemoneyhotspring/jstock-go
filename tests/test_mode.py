"""発注の可否は --live だけで決まり、WBJP_ENV は口座の選択。"""

from __future__ import annotations

from wbcore.credentials import Environment
from wbcore.settings import allows_live_orders, describe_mode


def test_live_flag_alone_decides_whether_orders_go_out() -> None:
    for env in Environment:
        allowed, reason = allows_live_orders(env, False)
        assert not allowed and "--live なし" in reason
        allowed, reason = allows_live_orders(env, True)
        assert allowed and reason == "--live あり"


def test_kill_switch_overrides_live() -> None:
    allowed, reason = allows_live_orders(Environment.PROD, True, kill_switch=True)
    assert not allowed and "キルスイッチ" in reason


def test_mode_line_shows_account_and_orders_separately() -> None:
    assert describe_mode(Environment.PROD, False) == (
        "口座: 本番口座（WBJP_ENV=prod）  発注: しない（--live なし（データ取得と判断は行い、注文は出さない））"
    )
    assert describe_mode(Environment.UAT, True).startswith(
        "口座: テスト口座（実弾ではない）（WBJP_ENV=uat）  発注: する"
    )
