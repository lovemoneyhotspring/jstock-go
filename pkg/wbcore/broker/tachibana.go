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

	clmLogin          = "CLMAuthLoginRequest"
	clmBalanceSummary = "CLMZanKaiSummary"
	clmCashPositions  = "CLMGenbutuKabuList"
	clmMarginPositions = "CLMShinyouTategyokuList"
	clmOrderList      = "CLMOrderList"
	clmNewOrder       = "CLMKabuNewOrder"
	clmCancelOrder    = "CLMKabuCancelOrder"
	clmMarketPrice    = "CLMMfdsGetMarketPrice"
	clmStockMaster    = "CLMStkGetIssueMstKabu"
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

	resp, err := t.httpClient.Post(t.session.URLRequest, "application/json", bytes.NewReader(sjisBytes))
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

func (t *TachibanaBroker) GetBalance() (*domain.Balance, error) {
	res, err := t.postRequest(clmBalanceSummary, nil)
	if err != nil {
		return nil, err
	}

	data, _ := res["aCLMKabuZan"].([]any)
	cash := decimal.Zero
	buyingPower := decimal.Zero
	if len(data) > 0 {
		row, _ := data[0].(map[string]any)
		if s, ok := row["sGenkinZandaka"].(string); ok {
			cash, _ = decimal.NewFromString(strings.TrimSpace(s))
		}
		if s, ok := row["sKabuKattukeKanouGaku"].(string); ok {
			buyingPower, _ = decimal.NewFromString(strings.TrimSpace(s))
		}
	}

	return &domain.Balance{
		Currency:    "JPY",
		CashBalance: cash,
		BuyingPower: buyingPower,
	}, nil
}

func (t *TachibanaBroker) GetPositions() ([]domain.Position, error) {
	res, err := t.postRequest(clmCashPositions, nil)
	if err != nil {
		return nil, err
	}

	data, _ := res["aCLMKabuGenbutu"].([]any)
	var positions []domain.Position
	for _, item := range data {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		sym, _ := row["sIssueCode"].(string)
		qtyStr, _ := row["sZandakaSuuryou"].(string)
		availStr, _ := row["sSyobunKanouSuu"].(string)
		costStr, _ := row["sHeikinSyutokuTanka"].(string)
		priceStr, _ := row["sGenzaiNe"].(string)

		qty, _ := decimal.NewFromString(strings.TrimSpace(qtyStr))
		avail, _ := decimal.NewFromString(strings.TrimSpace(availStr))
		cost, _ := decimal.NewFromString(strings.TrimSpace(costStr))
		price, _ := decimal.NewFromString(strings.TrimSpace(priceStr))

		positions = append(positions, domain.Position{
			Symbol:            strings.TrimSpace(sym),
			Quantity:          qty,
			AvailableQuantity: avail,
			CostPrice:         cost,
			LastPrice:         price,
			Currency:          "JPY",
			TaxType:           domain.TaxAccountSpecific,
			Trade:             domain.TradeTypeCash,
		})
	}

	return positions, nil
}

func (t *TachibanaBroker) PositionsBySymbol() (map[string]domain.Position, error) {
	positions, err := t.GetPositions()
	if err != nil {
		return nil, err
	}
	return PositionsBySymbolHelper(positions), nil
}

func (t *TachibanaBroker) GetOpenOrders() ([]domain.Order, error) {
	return nil, nil // 必要に応じて実装
}

func (t *TachibanaBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	return nil, nil // brokerOrderID を用いて CLMOrderListDetail を照会
}

func (t *TachibanaBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	return nil, nil
}

func (t *TachibanaBroker) LotSizes(symbols []string) map[string]decimal.Decimal {
	return make(map[string]decimal.Decimal)
}

func (t *TachibanaBroker) Preview(req domain.OrderRequest) (*domain.OrderPreview, error) {
	price := decimal.Zero
	if req.LimitPrice != nil {
		price = *req.LimitPrice
	}
	cost := price.Mul(req.Quantity).Round(0)
	fee := decimal.Zero // 立花定額テーブルに基づく概算
	return &domain.OrderPreview{
		EstimatedCost: cost,
		EstimatedFee:  fee,
	}, nil
}

func (t *TachibanaBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	// 注文電文パラメータ
	sideKubun := "3" // 買
	if req.Side == domain.SideSell {
		sideKubun = "1" // 売
	}

	priceStr := "0" // 成行
	if req.OrderType == domain.OrderTypeLimit && req.LimitPrice != nil {
		priceStr = req.LimitPrice.String()
	}

	params := map[string]any{
		"sBaibaiKubun":          sideKubun,
		"sGenkinShinyouKubun":   "0", // 現物
		"sIssueCode":            req.Symbol,
		"sOrderSuryo":           req.Quantity.String(),
		"sOrderPrice":           priceStr,
		"sSecondPassword":       t.creds.OrderPassword,
		"sZyoutoekiKazeiC":      "1", // 特定口座
		"sCondition":            "0", // なし
	}

	res, err := t.postRequest(clmNewOrder, params)
	if err != nil {
		return nil, err
	}

	resCode, _ := res["sResultCode"].(string)
	if resCode != "0" {
		resText, _ := res["sResultText"].(string)
		return nil, &OrderRejectedError{Message: fmt.Sprintf("立花発注拒否 [%s]: %s", resCode, resText)}
	}

	orderNum, _ := res["sOrderNumber"].(string)
	eigyouDay, _ := res["sEigyouDay"].(string)
	brokerOrderID := fmt.Sprintf("%s/%s", strings.TrimSpace(orderNum), strings.TrimSpace(eigyouDay))

	return &domain.OrderAck{
		ClientOrderID: req.ClientOrderID,
		BrokerOrderID: &brokerOrderID,
		Status:        domain.OrderStatusSubmitted,
	}, nil
}

func (t *TachibanaBroker) Cancel(clientOrderID string, brokerOrderID *string) error {
	if brokerOrderID == nil {
		return fmt.Errorf("立花証券の取消には broker_order_id が必須です")
	}
	parts := strings.Split(*brokerOrderID, "/")
	if len(parts) < 2 {
		return fmt.Errorf("不正な broker_order_id: %s", *brokerOrderID)
	}

	params := map[string]any{
		"sOrderNumber":    parts[0],
		"sEigyouDay":      parts[1],
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
