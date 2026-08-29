"""ブローカーの組み立て。認証情報の解決からキー失効の確認までを1か所に。

スイング売買も積立も同じ口座に発注する。接続の手順（認証情報を解決し、
ログから秘密を伏せ、失効間近なら警告する）をそれぞれの CLI に書くと
片方だけ直し忘れるので、ここに寄せる。
"""

from __future__ import annotations

from collections.abc import Callable

from wbcore.broker.webull import WebullBroker
from wbcore.credentials import Endpoints, Environment, load_credentials
from wbcore.domain.models import Market, TaxAccountType
from wbcore.logging import get_logger, register_secret

log = get_logger(__name__)

#: 公開テスト口座を使っているときの注意文。
PUBLIC_ACCOUNT_NOTICE = "公開テスト口座を使用中です。残高・建玉は他の利用者により変動します"


def connect_webull(
    env: Environment,
    endpoints: Endpoints,
    *,
    market: Market,
    tax_type: TaxAccountType,
    extended_hours: bool = False,
    notify: Callable[[str], None] | None = None,
) -> WebullBroker:
    """Webull に接続したブローカーを返す。

    dry-run でも本物のブローカーを使う。残高・建玉・未約定の実データが
    無ければ判断が意味を成さないため。発注だけを呼び出し側で止める。

    Args:
        notify: 公開テスト口座を使っているときに呼ぶ（CLI なら画面に出す）。
            省略時はログに警告する。
    """
    credentials = load_credentials(env)
    register_secret(credentials.app_key, credentials.app_secret, credentials.account_id)

    if credentials.is_public_test_account:
        (notify or log.warning)(PUBLIC_ACCOUNT_NOTICE)

    broker = WebullBroker(
        credentials,
        env,
        endpoints.trade,
        market=market,
        tax_type=tax_type,
        extended_hours=extended_hours,
    )
    broker.check_key_expiry()
    return broker
