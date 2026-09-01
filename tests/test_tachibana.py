"""立花証券 e支店 API のラッパー。HTTP はモックし、電文の中身（コード表・返済リスト・通番・復号）を検める。"""

from __future__ import annotations

import base64
import datetime as dt
import json
import stat
from decimal import Decimal
from pathlib import Path
from typing import Any
from zoneinfo import ZoneInfo

import pytest
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding, rsa

from wbcore.broker.base import BrokerError, InsufficientFundsError, OrderRejectedError
from wbcore.broker.tachibana import (
    TachibanaBroker,
    decrypt_virtual_url,
    parse_side,
    parse_status,
    parse_trade,
)
from wbcore.credentials import Environment, TachibanaCredentials
from wbcore.domain.models import (
    OrderRequest,
    OrderStatus,
    OrderType,
    Side,
    TaxAccountType,
    TradeType,
)

# --------------------------------------------------------------------------
# 道具
# --------------------------------------------------------------------------

_KEY = rsa.generate_private_key(public_exponent=65537, key_size=2048)
_PEM = _KEY.private_bytes(
    serialization.Encoding.PEM,
    serialization.PrivateFormat.PKCS8,
    serialization.NoEncryption(),
)
URL_REQUEST = "https://demo-kabuka.e-shiten.jp/e_api_v4r10/request/abc/"
URL_PRICE = "https://demo-kabuka.e-shiten.jp/e_api_v4r10/price/abc/"


def _encrypt(url: str) -> str:
    cipher = _KEY.public_key().encrypt(
        url.encode("ascii"),
        padding.OAEP(mgf=padding.MGF1(algorithm=hashes.SHA256()), algorithm=hashes.SHA256(), label=None),
    )
    return base64.b64encode(cipher).decode("ascii")


LOGIN_OK = {
    "sCLMID": "CLMAuthLoginAck",
    "p_errno": "0",
    "sResultCode": "0",
    "sResultText": "",
    "sUrlRequest": _encrypt(URL_REQUEST),
    "sUrlMaster": _encrypt("https://x/master/"),
    "sUrlPrice": _encrypt(URL_PRICE),
    "sUrlEvent": _encrypt("https://x/event/"),
    "sUrlEventWebSocket": _encrypt("wss://x/event/"),
}


class _Response:
    def __init__(self, payload: dict[str, Any], status: int = 200) -> None:
        self.status_code = status
        self.content = json.dumps(payload, ensure_ascii=False).encode("shift_jis")


class FakeHttp:
    """``requests.Session`` の代わり。CLMID ごとの応答を返し、送った電文を記録する。"""

    def __init__(self, responses: dict[str, Any]) -> None:
        self.responses = responses
        self.calls: list[tuple[str, dict[str, Any]]] = []

    def post(self, url: str, *, data: bytes, headers: dict[str, str], timeout: float) -> _Response:
        body = json.loads(data.decode("utf-8"))
        self.calls.append((url, body))
        response = self.responses[body["sCLMID"]]
        if callable(response):
            response = response(body)
        return _Response(response)


def _broker(tmp_path: Path, responses: dict[str, Any]) -> tuple[TachibanaBroker, FakeHttp]:
    creds = TachibanaCredentials(auth_id="AUTHID99", private_key_pem=_PEM, order_password="9876")
    broker = TachibanaBroker(
        creds,
        Environment.UAT,
        session_dir=tmp_path,
        clock=lambda: dt.datetime(2026, 9, 2, 9, 1, tzinfo=ZoneInfo("Asia/Tokyo")),
    )
    fake = FakeHttp({"CLMAuthLoginRequest": LOGIN_OK, **responses})
    broker._http = fake  # type: ignore[assignment]
    return broker, fake


def _request(side: Side, trade: TradeType, quantity: int = 100, **kw: Any) -> OrderRequest:
    return OrderRequest(
        client_order_id="c" * 10,
        symbol="7203",
        side=side,
        order_type=kw.pop("order_type", OrderType.MARKET),
        quantity=Decimal(quantity),
        tax_type=kw.pop("tax_type", TaxAccountType.SPECIFIC),
        trade=trade,
        **kw,
    )


# --------------------------------------------------------------------------
# コード表（リファレンス原文より: 売買区分 1=売 3=買、課税 1=特定 3=一般）
# --------------------------------------------------------------------------


def test_parse_side_is_one_sell_three_buy() -> None:
    assert parse_side("1") is Side.SELL
    assert parse_side("3") is Side.BUY
    with pytest.raises(BrokerError):
        parse_side("5")  # 現渡は扱わない


def test_parse_status_and_trade() -> None:
    assert parse_status("10") is OrderStatus.FILLED
    assert parse_status("7") is OrderStatus.CANCELLED
    assert parse_status("12") is OrderStatus.EXPIRED
    assert parse_status("2") is OrderStatus.REJECTED
    assert parse_status("1") is OrderStatus.SUBMITTED
    assert parse_status("99") is OrderStatus.UNKNOWN  # 未知は板に残っている扱い
    assert parse_trade("0") is TradeType.CASH
    assert parse_trade("2") is TradeType.MARGIN_OPEN
    assert parse_trade("4") is TradeType.MARGIN_CLOSE


def test_decrypt_virtual_url_round_trip() -> None:
    assert decrypt_virtual_url(_PEM, _encrypt(URL_REQUEST)) == URL_REQUEST


# --------------------------------------------------------------------------
# 発注電文
# --------------------------------------------------------------------------


def test_payload_cash_buy_and_sell(tmp_path: Path) -> None:
    broker, _ = _broker(tmp_path, {})
    buy = broker._to_payload(_request(Side.BUY, TradeType.CASH))
    assert buy["sBaibaiKubun"] == "3"
    assert buy["sGenkinShinyouKubun"] == "0"
    assert buy["sZyoutoekiKazeiC"] == "1"  # 特定
    assert buy["sOrderPrice"] == "0" and buy["sCondition"] == "0"
    assert buy["sGyakusasiPrice"] == "*" and buy["sTatebiType"] == "*"
    assert buy["sSecondPassword"] == "9876"
    assert "aCLMKabuHensaiData" not in buy
    sell = broker._to_payload(_request(Side.SELL, TradeType.CASH, tax_type=TaxAccountType.GENERAL))
    assert sell["sBaibaiKubun"] == "1"
    assert sell["sZyoutoekiKazeiC"] == "3"  # 一般


def test_payload_margin_open_short_and_limit(tmp_path: Path) -> None:
    broker, _ = _broker(tmp_path, {})
    short = broker._to_payload(_request(Side.SELL, TradeType.MARGIN_OPEN))
    assert short["sBaibaiKubun"] == "1" and short["sGenkinShinyouKubun"] == "2"
    assert short["sTatebiType"] == "*"
    limit = broker._to_payload(
        _request(Side.BUY, TradeType.MARGIN_OPEN, order_type=OrderType.LIMIT, limit_price=Decimal("2500.5"))
    )
    assert limit["sOrderPrice"] == "2500.5"


def test_short_sale_over_50_lots_must_not_be_market(tmp_path: Path) -> None:
    broker, _ = _broker(tmp_path, {})
    with pytest.raises(OrderRejectedError):
        broker._to_payload(_request(Side.SELL, TradeType.MARGIN_OPEN, quantity=5100))
    # 買建・現物・指値なら通る
    broker._to_payload(_request(Side.BUY, TradeType.MARGIN_OPEN, quantity=5100))
    broker._to_payload(
        _request(Side.SELL, TradeType.MARGIN_OPEN, quantity=5100, order_type=OrderType.LIMIT, limit_price=Decimal(100))
    )


def test_margin_close_lists_positions_of_opposite_side(tmp_path: Path) -> None:
    """売建の返済買いは、当日の売建玉を建玉番号で個別指定する。"""
    positions = {
        "sResultCode": "0",
        "aShinyouTategyokuList": [
            # 買建（反対側ではない）→ 使わない
            {"sOrderIssueCode": "7203", "sOrderBaibaiKubun": "3", "sOrderTategyokuNumber": "B1",
             "sOrderTategyokuDay": "20260902", "sOrderHensaiKanouSuryou": "300"},
            # 前日の売建 → 当日より後回し
            {"sOrderIssueCode": "7203", "sOrderBaibaiKubun": "1", "sOrderTategyokuNumber": "S0",
             "sOrderTategyokuDay": "20260901", "sOrderHensaiKanouSuryou": "300"},
            # 当日の売建 100 株
            {"sOrderIssueCode": "7203", "sOrderBaibaiKubun": "1", "sOrderTategyokuNumber": "S1",
             "sOrderTategyokuDay": "20260902", "sOrderHensaiKanouSuryou": "100"},
        ],
    }
    broker, _ = _broker(tmp_path, {"CLMShinyouTategyokuList": positions})
    payload = broker._to_payload(_request(Side.BUY, TradeType.MARGIN_CLOSE, quantity=200))
    assert payload["sBaibaiKubun"] == "3" and payload["sGenkinShinyouKubun"] == "4"
    assert payload["sTatebiType"] == "1"
    assert payload["aCLMKabuHensaiData"] == [
        {"sTategyokuNumber": "S1", "sTatebiZyuni": "1", "sOrderSuryou": "100"},
        {"sTategyokuNumber": "S0", "sTatebiZyuni": "2", "sOrderSuryou": "100"},
    ]
    with pytest.raises(OrderRejectedError):
        broker._to_payload(_request(Side.BUY, TradeType.MARGIN_CLOSE, quantity=500))


# --------------------------------------------------------------------------
# セッション（ログイン・復号・通番・保存）
# --------------------------------------------------------------------------


def test_login_decrypts_urls_and_persists_session(tmp_path: Path) -> None:
    broker, fake = _broker(
        tmp_path,
        {"CLMZanKaiSummary": {"sResultCode": "0", "sGenbutuKabuKaituke": "1500000", "sSinyouSinkidate": "4500000"}},
    )
    balance = broker.get_balance()
    assert balance.cash_balance == Decimal(1500000)
    assert balance.margin_buying_power == Decimal(4500000)
    assert balance.buying_power_for(TradeType.MARGIN_OPEN) == Decimal(4500000)
    assert balance.buying_power_for(TradeType.CASH) == Decimal(1500000)

    login_url, login_body = fake.calls[0]
    assert login_url.endswith("/e_api_v4r10/auth/")
    assert login_body["sCLMID"] == "CLMAuthLoginRequest" and login_body["sAuthId"] == "AUTHID99"
    assert login_body["p_no"] == "1" and login_body["sJsonOfmt"] == "5"
    assert login_body["p_sd_date"].startswith("2026.09.02-")
    request_url, request_body = fake.calls[1]
    assert request_url == URL_REQUEST  # 復号した仮想URLへ
    assert request_body["p_no"] == "2"  # 通番は続く

    saved = list(tmp_path.glob("session-uat-*.json"))
    assert len(saved) == 1
    assert stat.S_IMODE(saved[0].stat().st_mode) == 0o600
    data = json.loads(saved[0].read_text())
    assert data["url_request"] == URL_REQUEST and data["p_no"] == 2

    # 別インスタンス（別プロセス相当）は保存を読み、ログインせず通番を続ける
    broker2, fake2 = _broker(tmp_path, {"CLMZanKaiSummary": {"sResultCode": "0", "sGenbutuKabuKaituke": "1"}})
    broker2.get_balance()
    assert [b["sCLMID"] for _, b in fake2.calls] == ["CLMZanKaiSummary"]
    assert fake2.calls[0][1]["p_no"] == "3"


def test_session_lost_relogins_once(tmp_path: Path) -> None:
    attempts = {"n": 0}

    def summary(body: dict[str, Any]) -> dict[str, Any]:
        attempts["n"] += 1
        if attempts["n"] == 1:
            return {"p_errno": "2", "p_err": "セッションが切断しました。"}
        return {"sResultCode": "0", "sGenbutuKabuKaituke": "7"}

    broker, fake = _broker(tmp_path, {"CLMZanKaiSummary": summary})
    assert broker.get_balance().cash_balance == Decimal(7)
    assert [b["sCLMID"] for _, b in fake.calls] == [
        "CLMAuthLoginRequest", "CLMZanKaiSummary", "CLMAuthLoginRequest", "CLMZanKaiSummary",
    ]


def test_business_errors_are_translated(tmp_path: Path) -> None:
    broker, _ = _broker(
        tmp_path,
        {"CLMKabuNewOrder": {"sResultCode": "11421", "sResultText": "売付可能数量が不足しています"}},
    )
    with pytest.raises(InsufficientFundsError):
        broker.place(_request(Side.SELL, TradeType.CASH))
    broker2, _ = _broker(
        tmp_path, {"CLMKabuNewOrder": {"sResultCode": "11416", "sResultText": "空売り成行注文はできません"}}
    )
    with pytest.raises(OrderRejectedError):
        broker2.place(_request(Side.SELL, TradeType.MARGIN_OPEN))


# --------------------------------------------------------------------------
# 発注と照会
# --------------------------------------------------------------------------


def test_place_returns_native_id_and_get_order_reads_detail(tmp_path: Path) -> None:
    detail = {
        "sResultCode": "0",
        "sOrderNumber": "9000015",
        "sEigyouDay": "20260902",
        "sIssueCode": "7203",
        "sOrderBaibaiKubun": "1",
        "sGenkinSinyouKubun": "2",
        "sOrderOrderPriceKubun": "1",
        "sOrderOrderPrice": "0.0000",
        "sOrderOrderSuryou": "100",
        "sOrderStatusCode": "10",
        "sOrderOrderDateTime": "20260902090102",
        "sYakuzyouSuryou": "100",
        "sYakuzyouPrice": "2530.0000",
    }
    broker, _ = _broker(
        tmp_path,
        {
            "CLMKabuNewOrder": {"sResultCode": "0", "sOrderNumber": "9000015", "sEigyouDay": "20260902"},
            "CLMOrderListDetail": detail,
        },
    )
    ack = broker.place(_request(Side.SELL, TradeType.MARGIN_OPEN))
    assert ack.broker_order_id == "9000015/20260902"
    assert ack.status is OrderStatus.SUBMITTED

    # 台帳から渡された broker_order_id で、別インスタンスでも照会できる
    other, fake2 = _broker(tmp_path, {"CLMOrderListDetail": detail})
    order = other.get_order("c" * 10, broker_order_id="9000015/20260902")
    assert order is not None
    assert order.side is Side.SELL and order.trade is TradeType.MARGIN_OPEN
    assert order.order_type is OrderType.MARKET
    assert order.status is OrderStatus.FILLED
    assert order.filled_quantity == Decimal(100) and order.avg_fill_price == Decimal("2530")
    assert order.created_at == dt.datetime(2026, 9, 2, 0, 1, 2, tzinfo=dt.UTC)  # JST → UTC
    body = fake2.calls[-1][1]
    assert body["sOrderNumber"] == "9000015" and body["sEigyouDay"] == "20260902"

    with pytest.raises(BrokerError):
        other.get_order("unknown", broker_order_id=None)


def test_positions_mark_short_as_negative(tmp_path: Path) -> None:
    broker, _ = _broker(
        tmp_path,
        {
            "CLMGenbutuKabuList": {
                "sResultCode": "0",
                "aGenbutuKabuList": [
                    {"sUriOrderIssueCode": "7201", "sUriOrderZanKabuSuryou": "4200",
                     "sUriOrderUritukeKanouSuryou": "4200", "sUriOrderGaisanBokaTanka": "727",
                     "sUriOrderHyoukaTanka": "598", "sUriOrderZyoutoekiKazeiC": "1"}
                ],
            },
            "CLMShinyouTategyokuList": {
                "sResultCode": "0",
                "aShinyouTategyokuList": [
                    {"sOrderIssueCode": "7203", "sOrderBaibaiKubun": "1", "sOrderTategyokuSuryou": "100",
                     "sOrderHensaiKanouSuryou": "100", "sOrderTategyokuTanka": "2500",
                     "sOrderHyoukaTanka": "2400", "sOrderZyoutoekiKazeiC": "1"}
                ],
            },
        },
    )
    by_symbol = broker.positions_by_symbol()
    assert by_symbol["7201"].quantity == Decimal(4200)
    short = by_symbol["7203"]
    assert short.quantity == Decimal(-100) and short.trade is TradeType.MARGIN_OPEN
    assert short.unrealized_pnl == Decimal(10000)  # 2500 → 2400 の売建は +100 × 100


def test_market_prices_prefers_open(tmp_path: Path) -> None:
    broker, fake = _broker(
        tmp_path,
        {
            "CLMMfdsGetMarketPrice": {
                "aCLMMfdsMarketPrice": [
                    {"sIssueCode": "7203", "pDOP": "2510", "pDPP": "2515", "pPRP": "2480", "tDPP:T": "09:01:30"},
                    {"sIssueCode": "9984", "pDOP": "", "pDPP": "8000", "pPRP": "7900", "tDPP:T": ""},
                ]
            }
        },
    )
    prices = broker.market_prices(["7203", "9984"])
    assert fake.calls[-1][0] == URL_PRICE
    assert fake.calls[-1][1]["sTargetIssueCode"] == "7203,9984"
    assert prices["7203"]["open"] == Decimal(2510) and prices["7203"]["prev_close"] == Decimal(2480)
    assert prices["7203"]["at"] == dt.datetime(2026, 9, 2, 0, 1, 30, tzinfo=dt.UTC)
    assert prices["9984"]["open"] == Decimal(0) and prices["9984"]["last"] == Decimal(8000)


def test_lot_sizes_from_master_and_history_is_today_only(tmp_path: Path) -> None:
    master = {
        "aCLMStkIssueMstKabu": [
            {"sIssueCode": "1305", "sBaibaiTani": "10"},
            {"sIssueCode": "563A", "sBaibaiTani": "1"},
            {"sIssueCode": "7203", "sBaibaiTani": "100"},
        ]
    }
    orders = {
        "sResultCode": "0",
        "aOrderList": [
            {"sOrderOrderNumber": "1", "sOrderSikkouDay": "20260902", "sOrderIssueCode": "1305",
             "sOrderBaibaiKubun": "3", "sGenkinSinyouKubun": "0", "sOrderOrderPriceKubun": "2",
             "sOrderOrderPrice": "3000", "sOrderOrderSuryou": "10", "sOrderYakuzyouSuryo": "10",
             "sOrderYakuzyouPrice": "2990", "sOrderStatusCode": "10", "sOrderOrderDateTime": "20260902140102"}
        ],
    }
    broker, fake = _broker(tmp_path, {"CLMStkGetIssueMstKabu": master, "CLMOrderList": orders})
    lots = broker.lot_sizes(["1305", "563A", "9999"])
    assert lots == {"1305": Decimal(10), "563A": Decimal(1)}  # 無い銘柄は含めない
    assert fake.calls[-1][0] == "https://x/master/"  # マスタ機能の仮想URL
    broker.lot_sizes(["7203"])
    assert sum(1 for _, b in fake.calls if b["sCLMID"] == "CLMStkGetIssueMstKabu") == 1  # 1 日 1 回

    history = broker.get_order_history(dt.date(2026, 9, 1), dt.date(2026, 9, 2))
    assert len(history) == 1
    order = history[0]
    assert order.symbol == "1305" and order.side is Side.BUY and order.trade is TradeType.CASH
    assert order.order_type is OrderType.LIMIT and order.limit_price == Decimal(3000)
    assert order.filled_quantity == Decimal(10) and order.avg_fill_price == Decimal(2990)
    assert order.broker_order_id == "1/20260902"
    assert broker.get_order_history(dt.date(2026, 8, 1), dt.date(2026, 8, 31)) == []  # 過去日は取れない


def test_sequence_number_is_shared_across_instances(tmp_path: Path) -> None:
    """別プロセス（別インスタンス）が進めた通番に追いついてから +1 する。"""
    summary = {"sResultCode": "0", "sGenbutuKabuKaituke": "1"}
    first, fake1 = _broker(tmp_path, {"CLMZanKaiSummary": summary})
    second, fake2 = _broker(tmp_path, {"CLMZanKaiSummary": summary})
    first.get_balance()  # login(1) + summary(2)
    second.get_balance()  # 保存を読んで 3
    first.invalidate_cache()
    first.get_balance()  # 自分は 2 だと思っているが、保存の 3 に追いついて 4
    assert [b["p_no"] for _, b in fake1.calls] == ["1", "2", "4"]
    assert [b["p_no"] for _, b in fake2.calls] == ["3"]


def test_nisa_buys_use_growth_quota_code(tmp_path: Path) -> None:
    broker, _ = _broker(tmp_path, {})
    payload = broker._to_payload(_request(Side.BUY, TradeType.CASH, tax_type=TaxAccountType.NISA))
    assert payload["sZyoutoekiKazeiC"] == "6"  # 2024 年以降の NISA 買付は N成長（"5" は売却のみ）


def test_flat_rate_commission_table_and_marginal_fee(tmp_path: Path) -> None:
    from wbcore.broker.tachibana import flat_rate_commission, marginal_flat_rate_commission

    assert flat_rate_commission(Decimal(0)) == 0
    assert flat_rate_commission(Decimal(120_000)) == 0
    assert flat_rate_commission(Decimal(120_001)) == 176
    assert flat_rate_commission(Decimal(1_000_000)) == 506
    assert flat_rate_commission(Decimal(10_000_000)) == 2_783
    assert flat_rate_commission(Decimal(10_000_001)) == 2_783 + 253
    assert flat_rate_commission(Decimal(12_000_000)) == 2_783 + 253 * 2
    # 既に 10 万円買っている日に 5 万円買うと、合計 15 万円で 176 円の段階に入る
    assert marginal_flat_rate_commission(Decimal(100_000), Decimal(50_000)) == 176
    assert marginal_flat_rate_commission(Decimal(0), Decimal(100_000)) == 0

    # preview: 現物は当日の既約定分込みの差分、信用は 0
    broker, _ = _broker(
        tmp_path,
        {"CLMZanKaiSummary": {"sResultCode": "0", "sGenbutuKabuKaituke": "1", "sGenbutuBaibaiDaikin": "100000"}},
    )
    cash = broker.preview(
        _request(Side.BUY, TradeType.CASH, quantity=100, order_type=OrderType.LIMIT, limit_price=Decimal(500))
    )
    assert cash.estimated_cost == Decimal(50_000) and cash.estimated_fee == Decimal(176)
    margin = broker.preview(
        _request(Side.BUY, TradeType.MARGIN_OPEN, quantity=100, order_type=OrderType.LIMIT, limit_price=Decimal(500))
    )
    assert margin.estimated_fee == Decimal(0)
