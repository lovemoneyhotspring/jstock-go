package broker

import (
	"fmt"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// 立花証券 e支店 API（e_api_v4r10）のコード表。
//
// 出典はリファレンスの CLMKabuNewOrder / CLMOrderList の項目説明。
// https://www.e-shiten.jp/e_api/mfds_json_api_ref_text.html
//
// 値の取り違えは注文の向きや口座区分を間違えることに直結するので、
// 推測で書かず、電文ごとの表をそのまま写す。

const (
	// 売買区分（sBaibaiKubun）。**1＝売、3＝買**。5 現渡、7 現引は扱わない。
	tachibanaSideSell = "1"
	tachibanaSideBuy  = "3"

	// 現金信用区分（sGenkinShinyouKubun）。制度信用（6ヶ月）を使う。
	// 一般信用（6/8）は新規売りができないので発注には使わない。
	kubunCash        = "0"
	kubunMarginOpen  = "2"
	kubunMarginClose = "4"

	// 譲渡益課税区分（sZyoutoekiKazeiC）。**1＝特定、3＝一般**。
	// NISA は 5（一般NISA。2024 年以降は売却のみ）と 6（N成長＝新NISA 成長投資枠）。
	// このシステムが NISA で出すのは買付なので 6 を送る。
	zeiSpecific = "1"
	zeiGeneral  = "3"
	zeiNISA     = "6"

	// 市場コード（sSizyouC）。東証。
	marketCodeTSE = "00"
)

// ShortSaleMarketLimit は成行で出せる信用新規売りの上限株数。
//
// 空売り価格規制は個人の 50 単元以内を適用除外とする。それを超える新規売りは
// 成行で出せないので、発注前に弾く（ブローカーに拒否させると理由が分かりにくい）。
var ShortSaleMarketLimit = 50 * 100

var (
	sideCode = map[domain.Side]string{
		domain.SideSell: tachibanaSideSell,
		domain.SideBuy:  tachibanaSideBuy,
	}
	sideFromCode = map[string]domain.Side{
		tachibanaSideSell: domain.SideSell,
		tachibanaSideBuy:  domain.SideBuy,
	}

	taxCode = map[domain.TaxAccountType]string{
		domain.TaxAccountSpecific: zeiSpecific,
		domain.TaxAccountGeneral:  zeiGeneral,
		domain.TaxAccountNISA:     zeiNISA,
	}
	taxFromCode = map[string]domain.TaxAccountType{
		"1": domain.TaxAccountSpecific,
		"3": domain.TaxAccountGeneral,
		"5": domain.TaxAccountNISA,
		"6": domain.TaxAccountNISA,
	}

	tradeCode = map[domain.TradeType]string{
		domain.TradeTypeCash:        kubunCash,
		domain.TradeTypeMarginOpen:  kubunMarginOpen,
		domain.TradeTypeMarginClose: kubunMarginClose,
	}
	tradeFromCode = map[string]domain.TradeType{
		"0": domain.TradeTypeCash,
		"2": domain.TradeTypeMarginOpen,
		"6": domain.TradeTypeMarginOpen,
		"4": domain.TradeTypeMarginClose,
		"8": domain.TradeTypeMarginClose,
	}

	// orderTypeFromCode は sOrderOrderPriceKubun → 注文種別。
	orderTypeFromCode = map[string]domain.OrderType{
		"1": domain.OrderTypeMarket,
		"2": domain.OrderTypeLimit,
		"3": domain.OrderTypeMarket, // 引け成行
		"4": domain.OrderTypeLimit,  // 引け指値
	}

	// statusFromCode は sOrderStatusCode → 注文の状態。
	//
	// **終局を誤認すると二重発注になる**ので、確信の無い値は Unknown
	// （IsTerminal が false ＝ 板に残っている扱い）に落とす。
	statusFromCode = map[string]domain.OrderStatus{
		"0":  domain.OrderStatusPending,         // 受付未済
		"1":  domain.OrderStatusSubmitted,       // 未約定
		"2":  domain.OrderStatusRejected,        // 受付エラー
		"3":  domain.OrderStatusSubmitted,       // 訂正中
		"4":  domain.OrderStatusSubmitted,       // 訂正完了
		"5":  domain.OrderStatusSubmitted,       // 訂正失敗（注文は残る）
		"6":  domain.OrderStatusSubmitted,       // 取消中
		"7":  domain.OrderStatusCancelled,       // 取消完了
		"8":  domain.OrderStatusSubmitted,       // 取消失敗（注文は残る）
		"9":  domain.OrderStatusPartiallyFilled, // 一部約定
		"10": domain.OrderStatusFilled,          // 全部約定
		"11": domain.OrderStatusExpired,         // 一部失効（約定分は filled に残る）
		"12": domain.OrderStatusExpired,         // 全部失効
		"13": domain.OrderStatusPending,         // 発注待ち
		"14": domain.OrderStatusRejected,        // 無効
		"15": domain.OrderStatusSubmitted,       // 切替注文 / 逆指注文（切替中）
		"16": domain.OrderStatusSubmitted,       // 切替完了 / 逆指注文（未約定）
		"17": domain.OrderStatusRejected,        // 切替注文失敗
		"19": domain.OrderStatusExpired,         // 繰越失効
		"50": domain.OrderStatusPending,         // 発注中
	}
)

// ParseSide は sBaibaiKubun を解釈する。1＝売、3＝買。
// 現渡(5)・現引(7)は扱わないのでエラー——買いとして通すと向きを間違える。
func ParseSide(value string) (domain.Side, error) {
	if side, ok := sideFromCode[strings.TrimSpace(value)]; ok {
		return side, nil
	}
	return "", fmt.Errorf("注文の売買区分を解釈できません: %q（sBaibaiKubun。1=売 3=買 以外）", value)
}

// ParseStatus は sOrderStatusCode を解釈する。未知の値は Unknown（＝板に残っている扱い）。
func ParseStatus(value string) domain.OrderStatus {
	if status, ok := statusFromCode[strings.TrimSpace(value)]; ok {
		return status
	}
	return domain.OrderStatusUnknown
}

// ParseTrade は sGenkinSinyouKubun を解釈する。
// 未知の値は現物扱いにせずエラー——信用の建玉を現物として数えると手仕舞いを誤る。
func ParseTrade(value string) (domain.TradeType, error) {
	key := strings.TrimSpace(value)
	if key == "" {
		return domain.TradeTypeCash, nil
	}
	if trade, ok := tradeFromCode[key]; ok {
		return trade, nil
	}
	return "", fmt.Errorf("現金信用区分を解釈できません: %q（sGenkinSinyouKubun）", value)
}

// ParseTaxAccount は譲渡益課税区分を解釈する。未知の値は fallback。
func ParseTaxAccount(value string, fallback domain.TaxAccountType) domain.TaxAccountType {
	if tax, ok := taxFromCode[strings.TrimSpace(value)]; ok {
		return tax
	}
	return fallback
}

// taxCodeOf は課税区分のコード。未設定は特定口座。
func taxCodeOf(tax domain.TaxAccountType) string {
	if code, ok := taxCode[tax]; ok {
		return code
	}
	return zeiSpecific
}

// tradeCodeOf は現金信用区分のコード。未知の取引種別は現物に落とさずエラー。
func tradeCodeOf(trade domain.TradeType) (string, error) {
	if trade == "" {
		return kubunCash, nil
	}
	if code, ok := tradeCode[trade]; ok {
		return code, nil
	}
	return "", fmt.Errorf("立花証券に送れない取引種別です: %s", trade)
}

// sideCodeOf は売買区分のコード。
func sideCodeOf(side domain.Side) (string, error) {
	if code, ok := sideCode[side]; ok {
		return code, nil
	}
	return "", fmt.Errorf("立花証券に送れない売買区分です: %s", side)
}
