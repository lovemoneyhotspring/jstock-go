package broker

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
)

const (
	BaseURLUAT  = "https://demo-kabuka.e-shiten.jp/e_api_v4r10/"
	BaseURLProd = "https://kabuka.e-shiten.jp/e_api_v4r10/"

	clmLogin           = "CLMAuthLoginRequest"
	clmBalanceSummary  = "CLMZanKaiSummary"
	clmCashPositions   = "CLMGenbutuKabuList"
	clmMarginPositions = "CLMShinyouTategyokuList"
	clmOrderList       = "CLMOrderList"
	clmOrderDetail     = "CLMOrderListDetail"
	clmNewOrder        = "CLMKabuNewOrder"
	clmCancelOrder     = "CLMKabuCancelOrder"
	clmMarketPrice     = "CLMMfdsGetMarketPrice"
	clmStockMaster     = "CLMStkGetIssueMstKabu"

	// 応答の配列のキー。電文ごとに違うので 1 箇所に集める。
	balanceSummaryKey  = "sGenbutuKabuKaituke" // ※ 残高は配列ではなく直下の項目
	cashPositionsKey   = "aGenbutuKabuList"
	marginPositionsKey = "aShinyouTategyokuList"
	stockMasterKey     = "aCLMStkIssueMstKabu"

	// orderNotFoundCode は「その注文は無い」を表す結果コード。
	orderNotFoundCode = "991005"
)

type TachibanaSession struct {
	PNo        int    `json:"p_no"`
	URLRequest string `json:"url_request"`
	URLPrice   string `json:"url_price"`
	URLMaster  string `json:"url_master"`
	Date       string `json:"date"`
}

type TachibanaBroker struct {
	mu         sync.Mutex
	env        settings.Environment
	creds      *credentials.TachibanaCredentials
	baseURL    string
	httpClient *http.Client
	stateDir   string
	session    *TachibanaSession
	privKey    *rsa.PrivateKey

	// nativeMu / nativeOrderIDs は client_order_id → "注文番号/営業日"。
	// 立花は client_order_id を保持しないので、同一プロセスで出した注文を
	// 台帳を経由せずに照会・取消できるようにここに控える。
	nativeMu       sync.Mutex
	nativeOrderIDs map[string]string

	// masterMu / lotSizeMaster は売買単位のマスタ。全銘柄が一括で返るので
	// 1 プロセスに 1 回だけ取る。
	masterMu      sync.Mutex
	lotSizeMaster map[string]decimal.Decimal

	// cashTradedToday は当日の現物約定代金の合計（定額コースの手数料の見積りに使う）。
	cashTradedToday decimal.Decimal
}

// rememberNativeOrderID は発注で得た注文番号を控える。
func (t *TachibanaBroker) rememberNativeOrderID(clientOrderID, brokerOrderID string) {
	t.nativeMu.Lock()
	defer t.nativeMu.Unlock()
	if t.nativeOrderIDs == nil {
		t.nativeOrderIDs = make(map[string]string)
	}
	t.nativeOrderIDs[clientOrderID] = brokerOrderID
}

// nativeOrderID は控えた注文番号（"番号/営業日"）。無ければ空。
func (t *TachibanaBroker) nativeOrderID(clientOrderID string) string {
	t.nativeMu.Lock()
	defer t.nativeMu.Unlock()
	return t.nativeOrderIDs[clientOrderID]
}

// clientOrderIDFor は注文番号から client_order_id を逆引きする。
// 同一プロセスで出していない注文は空（呼び出し側が broker_order_id を使う）。
func (t *TachibanaBroker) clientOrderIDFor(number string) string {
	t.nativeMu.Lock()
	defer t.nativeMu.Unlock()
	for clientID, native := range t.nativeOrderIDs {
		if n, _, ok := splitBrokerOrderID(native); ok && n == number {
			return clientID
		}
	}
	return ""
}

func NewTachibanaBroker(env settings.Environment, creds *credentials.TachibanaCredentials, stateDir string) (*TachibanaBroker, error) {
	baseURL := BaseURLUAT
	if env.IsProduction() {
		baseURL = BaseURLProd
	}

	keyBytes, err := os.ReadFile(creds.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("秘密鍵ファイルの読込に失敗しました (%s): %w", creds.PrivateKeyFile, err)
	}

	block, _ := pem.Decode(keyBytes)
	if block == nil {
		return nil, fmt.Errorf("秘密鍵のPEMデコードに失敗しました: %s", creds.PrivateKeyFile)
	}

	privKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// PKCS8 の場合も試行
		pk8, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("RSA秘密鍵パース失敗: %v / %v", err, err2)
		}
		var ok bool
		privKey, ok = pk8.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8の鍵がRSAではありません")
		}
	}

	return &TachibanaBroker{
		env:        env,
		creds:      creds,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		stateDir:   stateDir,
		privKey:    privKey,
	}, nil
}

func (t *TachibanaBroker) Name() string {
	return "tachibana"
}

func (t *TachibanaBroker) AccountID() string {
	return t.creds.AuthID
}

func (t *TachibanaBroker) sessionFilePath() string {
	today := clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("20060102")
	return filepath.Join(t.stateDir, "tachibana", fmt.Sprintf("session-%s-%s.json", t.env, today))
}

func (t *TachibanaBroker) decryptURL(encryptedBase64 string) (string, error) {
	cipherBytes, err := base64.StdEncoding.DecodeString(encryptedBase64)
	if err != nil {
		return "", fmt.Errorf("base64 decode error: %w", err)
	}

	plainBytes, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, t.privKey, cipherBytes, nil)
	if err != nil {
		return "", fmt.Errorf("RSA OAEP decrypt error: %w", err)
	}

	return string(plainBytes), nil
}

func (t *TachibanaBroker) ensureSession() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	sessionPath := t.sessionFilePath()
	if t.session != nil {
		return nil
	}

	// 既存セッションファイルを確認
	if data, err := os.ReadFile(sessionPath); err == nil {
		var s TachibanaSession
		if err := json.Unmarshal(data, &s); err == nil && s.URLRequest != "" {
			t.session = &s
			return nil
		}
	}

	// ログイン実行
	today := clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("20060102")
	loginPayload := map[string]any{
		"p_no":      1,
		"p_sd_date": today,
		"sJsonOfmt": "5",
		"sCLMID":    clmLogin,
		"sAuthId":   t.creds.AuthID,
	}

	bodyBytes, _ := json.Marshal(loginPayload)
	authURL := t.baseURL + "auth/"
	resp, err := t.httpClient.Post(authURL, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("立花証券ログインHTTPエラー: %w", err)
	}
	defer resp.Body.Close()

	utf8Reader := transform.NewReader(resp.Body, japanese.ShiftJIS.NewDecoder())
	respBytes, err := io.ReadAll(utf8Reader)
	if err != nil {
		return fmt.Errorf("立花証券ログイン応答読込エラー: %w", err)
	}

	var res map[string]any
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return fmt.Errorf("立花証券ログインJSONパースエラー: %w", err)
	}

	pErrno, _ := res["p_errno"].(string)
	if pErrno != "0" {
		return fmt.Errorf("立花証券ログインエラー p_errno=%s", pErrno)
	}

	encReq, _ := res["sUrlRequest"].(string)
	encPrice, _ := res["sUrlPrice"].(string)
	encMaster, _ := res["sUrlMaster"].(string)

	decReq, err := t.decryptURL(encReq)
	if err != nil {
		return fmt.Errorf("sUrlRequest 復号エラー: %w", err)
	}
	decPrice, _ := t.decryptURL(encPrice)
	decMaster, _ := t.decryptURL(encMaster)

	session := TachibanaSession{
		PNo:        2,
		URLRequest: decReq,
		URLPrice:   decPrice,
		URLMaster:  decMaster,
		Date:       today,
	}

	if err := os.MkdirAll(filepath.Dir(sessionPath), 0700); err == nil {
		sessData, _ := json.MarshalIndent(session, "", "  ")
		_ = os.WriteFile(sessionPath, sessData, 0600)
	}

	t.session = &session
	return nil
}

func (t *TachibanaBroker) postRequest(clmID string, params map[string]any) (map[string]any, error) {
	return t.postTo(interfaceRequest, clmID, params)
}

// postPriceRequest は時価問合（仮想URL sUrlPrice）へ送る。
// 発注系（sUrlRequest）とは別の口で、上限も別に数えられている。
func (t *TachibanaBroker) postPriceRequest(clmID string, params map[string]any) (map[string]any, error) {
	return t.postTo(interfacePrice, clmID, params)
}

// 仮想URL の種別。ログイン応答で口ごとに別の URL が返る。
const (
	interfaceRequest = "request"
	interfacePrice   = "price"
	interfaceMaster  = "master"
)

// postTo は仮想URL を選んで 1 リクエスト送る。postRequest / postPriceRequest の実体。
func (t *TachibanaBroker) postTo(iface string, clmID string, params map[string]any) (map[string]any, error) {
	if err := t.ensureSession(); err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	sessionPath := t.sessionFilePath()
	lockFile, err := os.OpenFile(sessionPath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err == nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
		defer func() {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
		}()
	}

	pNo := t.session.PNo
	t.session.PNo++

	payload := map[string]any{
		"p_no":      pNo,
		"p_sd_date": t.session.Date,
		"sJsonOfmt": "5",
		"sCLMID":    clmID,
	}
	for k, v := range params {
		payload[k] = v
	}

	bodyBytes, _ := json.Marshal(payload)
	// Shift_JIS にエンコードして送信
	sjisBytes, err := io.ReadAll(transform.NewReader(bytes.NewReader(bodyBytes), japanese.ShiftJIS.NewEncoder()))
	if err != nil {
		sjisBytes = bodyBytes
	}

	endpoint := t.session.URLRequest
	switch {
	case iface == interfacePrice && t.session.URLPrice != "":
		endpoint = t.session.URLPrice
	case iface == interfaceMaster && t.session.URLMaster != "":
		endpoint = t.session.URLMaster
	}
	resp, err := t.httpClient.Post(endpoint, "application/json", bytes.NewReader(sjisBytes))
	if err != nil {
		return nil, fmt.Errorf("立花証券API通信エラー: %w", err)
	}
	defer resp.Body.Close()

	utf8Reader := transform.NewReader(resp.Body, japanese.ShiftJIS.NewDecoder())
	respBytes, err := io.ReadAll(utf8Reader)
	if err != nil {
		return nil, fmt.Errorf("立花証券API応答読込エラー: %w", err)
	}

	var res map[string]any
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return nil, fmt.Errorf("立花証券API JSONパースエラー: %w", err)
	}

	if sessData, err := json.MarshalIndent(t.session, "", "  "); err == nil {
		_ = os.WriteFile(sessionPath, sessData, 0600)
	}

	return res, nil
}

// Place は 1 注文を出す。
//
// 現物・信用新規・信用返済を取引種別（req.Trade）で振り分ける。以前は
// 現物固定だったため、信用の設定（config/daytrade_margin）で回すと
// 別の商品として発注されていた。
//
// 返済は建玉の指定が要る。指定できないときは**発注せずにエラーにする**——
// 現物売りとして通すと、持っていない株を売ろうとすることになる。
func (t *TachibanaBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	params, err := t.orderPayload(req)
	if err != nil {
		return nil, err
	}

	res, err := t.postRequest(clmNewOrder, params)
	if err != nil {
		return nil, err
	}
	resCode := strings.TrimSpace(text(res["sResultCode"]))
	if resCode != "0" {
		return nil, &OrderRejectedError{Message: fmt.Sprintf(
			"立花発注拒否 [%s]: %s", resCode, strings.TrimSpace(text(res["sResultText"])))}
	}

	number := strings.TrimSpace(text(res["sOrderNumber"]))
	day := strings.TrimSpace(text(res["sEigyouDay"]))
	if number == "" || day == "" {
		// 受理されたのに注文番号が取れないと、以後この注文を照会も取消もできない。
		// 注文は出ているので発注失敗とはせず、番号なしで返して呼び出し側に記録させる。
		fmt.Fprintf(os.Stderr,
			"[error] 発注応答に注文番号／営業日がありません。この注文は照会・取消できません（%s）\n",
			req.ClientOrderID)
		return &domain.OrderAck{
			ClientOrderID: req.ClientOrderID,
			Status:        domain.OrderStatusSubmitted,
		}, nil
	}

	brokerOrderID := brokerOrderIDOf(number, day)
	t.rememberNativeOrderID(req.ClientOrderID, brokerOrderID)
	return &domain.OrderAck{
		ClientOrderID: req.ClientOrderID,
		BrokerOrderID: &brokerOrderID,
		Status:        domain.OrderStatusSubmitted,
	}, nil
}

// orderPayload は新規注文の電文を組み立てる。
//
// 項目は CLMKabuNewOrder の必須項目をすべて埋める（省略すると受付エラーになる）。
// 逆指値は使わないので固定値、返済のときだけ建玉の指定を足す。
func (t *TachibanaBroker) orderPayload(req domain.OrderRequest) (map[string]any, error) {
	if !req.OrderType.IsPlaceable() {
		return nil, fmt.Errorf("発注できない注文種別です: %s", req.OrderType)
	}
	sideKubun, err := sideCodeOf(req.Side)
	if err != nil {
		return nil, err
	}
	tradeKubun, err := tradeCodeOf(req.Trade)
	if err != nil {
		return nil, err
	}

	isMarket := req.OrderType == domain.OrderTypeMarket
	if req.Trade == domain.TradeTypeMarginOpen && req.Side == domain.SideSell && isMarket &&
		req.Quantity.GreaterThan(decimal.NewFromInt(int64(ShortSaleMarketLimit))) {
		// 空売り価格規制。個人は 50 単元以内なら適用除外だが、それを超えると成行で出せない
		return nil, &OrderRejectedError{Message: fmt.Sprintf(
			"%s: 51 単元以上の信用新規売りは成行では出せません（空売り価格規制）。"+
				"数量 %s を減らすか指値にしてください", req.Symbol, req.Quantity)}
	}

	price := "0"
	if !isMarket {
		if req.LimitPrice == nil {
			return nil, fmt.Errorf("%s: 指値注文に価格がありません", req.Symbol)
		}
		price = req.LimitPrice.String()
	}

	tatebiType := "*"
	if req.Trade == domain.TradeTypeMarginClose {
		tatebiType = "1" // 建日を個別指定する
	}

	params := map[string]any{
		"sZyoutoekiKazeiC":          taxCodeOf(req.TaxType),
		"sIssueCode":                req.Symbol,
		"sSizyouC":                  marketCodeTSE,
		"sBaibaiKubun":              sideKubun,
		"sCondition":                "0", // 執行条件なし
		"sOrderPrice":               price,
		"sOrderSuryou":              req.Quantity.String(),
		"sGenkinShinyouKubun":       tradeKubun,
		"sOrderExpireDay":           "0", // 当日限り
		"sGyakusasiOrderType":       "0",
		"sGyakusasiZyouken":         "0",
		"sGyakusasiPrice":           "*",
		"sTatebiType":               tatebiType,
		"sTategyokuZyoutoekiKazeiC": "*",
		"sSecondPassword":           t.creds.OrderPassword,
	}

	if req.Trade == domain.TradeTypeMarginClose {
		allocation, err := t.repaymentList(req)
		if err != nil {
			return nil, err
		}
		params["aCLMKabuHensaiData"] = allocation
	}
	return params, nil
}

func (t *TachibanaBroker) Cancel(clientOrderID string, brokerOrderID *string) error {
	var raw string
	if brokerOrderID != nil {
		raw = *brokerOrderID
	}
	if raw == "" {
		raw = t.nativeOrderID(clientOrderID)
	}
	number, day, ok := splitBrokerOrderID(raw)
	if !ok {
		return fmt.Errorf(
			"client_order_id=%q の立花証券の注文番号が分からないため取消できません", clientOrderID)
	}

	params := map[string]any{
		"sOrderNumber":    number,
		"sEigyouDay":      day,
		"sSecondPassword": t.creds.OrderPassword,
	}

	res, err := t.postRequest(clmCancelOrder, params)
	if err != nil {
		return err
	}

	resCode, _ := res["sResultCode"].(string)
	if resCode != "0" {
		resText, _ := res["sResultText"].(string)
		return fmt.Errorf("立花取消拒否 [%s]: %s", resCode, resText)
	}

	return nil
}
