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
	clmCorrectOrder    = "CLMKabuCorrectOrder"
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

// セッションと採番
//
// 立花の仮想URL はログインで発行され、以後の電文は p_no（通番）を付けて送る。
// 同じ口座を cron の daytrade / wbjp / accum が別プロセスで叩くので、
// **セッションファイルを採番の権威にし、flock の中で「読む → 進める → 書く → 送る」**
// を 1 つの区間にする。メモリ上の値だけを進めると、並走する 2 プロセスが同じ番号を
// 送り、後に書いた方が相手の採番を巻き戻す。
//
// p_errno（電文の枠組みのエラー。セッション失効など）が返ったらファイルを捨て、
// 照会系なら 1 度だけログインし直して送り直す。発注（CLMKabuNewOrder）は
// 送り直さない——届いた上で失効の応答が来た可能性を否定できないため。
// 呼び出し側は「結果不明」として台帳に PENDING を残す。

// ErrSession は p_errno が 0 以外だった応答。セッションは捨ててある。
type ErrSession struct {
	CLMID string
	Errno string
	Text  string
}

func (e *ErrSession) Error() string {
	return fmt.Sprintf("%s の電文が受け付けられませんでした p_errno=%s %s（セッションを破棄。再実行で再ログインします）",
		e.CLMID, e.Errno, strings.TrimSpace(e.Text))
}

func (t *TachibanaBroker) sessionFilePath() string {
	today := clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("20060102")
	return filepath.Join(t.stateDir, "tachibana", fmt.Sprintf("session-%s-%s.json", t.env, today))
}

// readSessionFile は保存済みのセッション。壊れている・仮想URL が無いなら偽。
func readSessionFile(path string) (*TachibanaSession, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var s TachibanaSession
	if err := json.Unmarshal(data, &s); err != nil || s.URLRequest == "" {
		return nil, false
	}
	return &s, true
}

// writeSessionFile は一時ファイルに書いて rename する（途中で落ちても壊れた
// ファイルを残さない。壊れたファイルは「セッション無し」と読まれ再ログインになる）。
func writeSessionFile(path string, s *TachibanaSession) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// lockSession はセッションの排他ロック（プロセス間）。取れなければエラー——
// ロック無しで進めると採番が衝突するので、黙って続けない。
func lockSession(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("セッションの置き場を作れません: %w", err)
	}
	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("セッションのロックファイルを開けません: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("セッションのロックを取れません: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
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

// login はログイン電文を送り、仮想URL を復号したセッションを返す（保存はしない）。
func (t *TachibanaBroker) login() (*TachibanaSession, error) {
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
		return nil, fmt.Errorf("立花証券ログインHTTPエラー: %w", err)
	}
	defer resp.Body.Close()

	utf8Reader := transform.NewReader(resp.Body, japanese.ShiftJIS.NewDecoder())
	respBytes, err := io.ReadAll(utf8Reader)
	if err != nil {
		return nil, fmt.Errorf("立花証券ログイン応答読込エラー: %w", err)
	}

	var res map[string]any
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return nil, fmt.Errorf("立花証券ログインJSONパースエラー: %w", err)
	}

	if pErrno := strings.TrimSpace(text(res["p_errno"])); pErrno != "0" {
		return nil, fmt.Errorf("立花証券ログインエラー p_errno=%s %s", pErrno, strings.TrimSpace(text(res["p_err"])))
	}

	// 3 つの仮想URL はどれも要る。復号できないものを空で通すと、postTo が
	// 発注口（sUrlRequest）へ黙ってフォールバックし、時価問合が発注口の
	// 上限を食う。ログインの時点で落とす。
	urls := map[string]string{}
	for _, key := range []string{"sUrlRequest", "sUrlPrice", "sUrlMaster"} {
		decoded, err := t.decryptURL(text(res[key]))
		if err != nil {
			return nil, fmt.Errorf("%s 復号エラー: %w", key, err)
		}
		if decoded == "" {
			return nil, fmt.Errorf("%s がログイン応答にありません", key)
		}
		urls[key] = decoded
	}

	return &TachibanaSession{
		PNo:        2,
		URLRequest: urls["sUrlRequest"],
		URLPrice:   urls["sUrlPrice"],
		URLMaster:  urls["sUrlMaster"],
		Date:       today,
	}, nil
}

// ensureSessionLocked は t.mu と flock を持った状態で呼ぶ。ファイルがあればそれを
// 権威にし（採番はファイルとメモリの大きい方）、無ければログインして書く。
func (t *TachibanaBroker) ensureSessionLocked(path string) error {
	if saved, ok := readSessionFile(path); ok {
		if t.session != nil && t.session.PNo > saved.PNo {
			saved.PNo = t.session.PNo
		}
		t.session = saved
		return nil
	}
	session, err := t.login()
	if err != nil {
		return err
	}
	if err := writeSessionFile(path, session); err != nil {
		return fmt.Errorf("セッションを保存できません: %w", err)
	}
	t.session = session
	return nil
}

// invalidateSessionLocked はセッションを捨てる（次の電文でログインし直す）。
func (t *TachibanaBroker) invalidateSessionLocked(path string) {
	t.session = nil
	_ = os.Remove(path)
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

// resendable は p_errno でセッションを捨てたあと、ログインし直して送り直してよい電文か。
// 新規注文だけは送り直さない（届いていた場合に二重発注になる）。
func resendable(clmID string) bool {
	return clmID != clmNewOrder
}

// postTo は仮想URL を選んで 1 リクエスト送る。postRequest / postPriceRequest の実体。
func (t *TachibanaBroker) postTo(iface string, clmID string, params map[string]any) (map[string]any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	sessionPath := t.sessionFilePath()
	unlock, err := lockSession(sessionPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	for attempt := 0; ; attempt++ {
		if err := t.ensureSessionLocked(sessionPath); err != nil {
			return nil, err
		}
		// 採番はファイルを権威にする。送る前に進めて書く——送信後に落ちても
		// 次のプロセスが同じ番号を使わない
		pNo := t.session.PNo
		t.session.PNo++
		if err := writeSessionFile(sessionPath, t.session); err != nil {
			return nil, fmt.Errorf("セッションを保存できません: %w", err)
		}

		res, err := t.send(iface, pNo, clmID, params)
		if err != nil {
			return nil, err
		}
		errno := strings.TrimSpace(text(res["p_errno"]))
		if errno == "" || errno == "0" {
			return res, nil
		}
		t.invalidateSessionLocked(sessionPath)
		if attempt == 0 && resendable(clmID) {
			continue
		}
		return nil, &ErrSession{CLMID: clmID, Errno: errno, Text: text(res["p_err"])}
	}
}

// send は 1 電文を Shift_JIS で送り、応答を UTF-8 の map にする。
func (t *TachibanaBroker) send(iface string, pNo int, clmID string, params map[string]any) (map[string]any, error) {
	payload := map[string]any{
		"p_no":      pNo,
		"p_sd_date": t.session.Date,
		"sJsonOfmt": "5",
		"sCLMID":    clmID,
	}
	for k, v := range params {
		payload[k] = v
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("立花証券API 電文の組み立てに失敗しました: %w", err)
	}
	sjisBytes, err := io.ReadAll(transform.NewReader(bytes.NewReader(bodyBytes), japanese.ShiftJIS.NewEncoder()))
	if err != nil {
		return nil, fmt.Errorf("立花証券API 電文を Shift_JIS にできません: %w", err)
	}

	endpoint := t.session.URLRequest
	switch iface {
	case interfacePrice:
		endpoint = t.session.URLPrice
	case interfaceMaster:
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
// 逆指値（req.Stop）は sGyakusasiOrderType 1（逆指値だけ。sOrderPrice は "*"）か
// 2（通常＋逆指値。sOrderPrice に通常の指値）。返済のときは建玉の指定を足す。
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

	// 逆指値。条件価格は正、発火後の値段は 0 が成行
	stopType, stopTrigger, stopPrice := "0", "0", "*"
	if req.Stop != nil {
		if req.Stop.Trigger.LessThanOrEqual(decimal.Zero) {
			return nil, fmt.Errorf("%s: 逆指値の条件価格がありません", req.Symbol)
		}
		stopTrigger = req.Stop.Trigger.String()
		stopPrice = "0"
		if req.Stop.Price != nil {
			stopPrice = req.Stop.Price.String()
		}
		if req.IsStopOnly() {
			stopType = "1"
			price = "*" // 逆指値だけの注文に通常の値段は無い
		} else {
			stopType = "2"
		}
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
		"sGyakusasiOrderType":       stopType,
		"sGyakusasiZyouken":         stopTrigger,
		"sGyakusasiPrice":           stopPrice,
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

// orderNumberOf は取消・訂正に使う立花証券の注文番号と営業日。
// broker_order_id が無ければ発注時に覚えた対応表から引く。
func (t *TachibanaBroker) orderNumberOf(clientOrderID string, brokerOrderID *string, purpose string) (number, day string, err error) {
	var raw string
	if brokerOrderID != nil {
		raw = *brokerOrderID
	}
	if raw == "" {
		raw = t.nativeOrderID(clientOrderID)
	}
	number, day, ok := splitBrokerOrderID(raw)
	if !ok {
		return "", "", fmt.Errorf(
			"client_order_id=%q の立花証券の注文番号が分からないため%sできません", clientOrderID, purpose)
	}
	return number, day, nil
}

func (t *TachibanaBroker) Cancel(clientOrderID string, brokerOrderID *string) error {
	number, day, err := t.orderNumberOf(clientOrderID, brokerOrderID, "取消")
	if err != nil {
		return err
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

// CorrectStop は未発火の逆指値の条件価格と発火後の値段を訂正する（CLMKabuCorrectOrder）。
//
// トレーリングはこれを定期的に呼んで条件を引き上げる。発火した後は条件を訂正できない
// （リファレンスの注意書き）ので、その場合はブローカーが拒否を返す。数量・期日・
// 通常の値段は変えない（"*"）。
func (t *TachibanaBroker) CorrectStop(clientOrderID string, brokerOrderID *string, stop domain.StopSpec) error {
	number, day, err := t.orderNumberOf(clientOrderID, brokerOrderID, "訂正")
	if err != nil {
		return err
	}
	params, err := correctStopPayload(number, day, stop, t.creds.OrderPassword)
	if err != nil {
		return err
	}

	res, err := t.postRequest(clmCorrectOrder, params)
	if err != nil {
		return err
	}

	resCode, _ := res["sResultCode"].(string)
	if resCode != "0" {
		resText, _ := res["sResultText"].(string)
		return fmt.Errorf("立花訂正拒否 [%s]: %s", resCode, resText)
	}
	return nil
}

// correctStopPayload は逆指値の訂正電文。変えない項目は "*"。
func correctStopPayload(number, day string, stop domain.StopSpec, password string) (map[string]any, error) {
	if stop.Trigger.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("逆指値の条件価格は正の数: %s", stop.Trigger)
	}
	price := "0" // 発火後は成行
	if stop.Price != nil {
		if stop.Price.LessThanOrEqual(decimal.Zero) {
			return nil, fmt.Errorf("逆指値の値段は正の数: %s", stop.Price)
		}
		price = stop.Price.String()
	}
	return map[string]any{
		"sOrderNumber":      number,
		"sEigyouDay":        day,
		"sCondition":        "*",
		"sOrderPrice":       "*",
		"sOrderSuryou":      "*",
		"sOrderExpireDay":   "*",
		"sGyakusasiZyouken": stop.Trigger.String(),
		"sGyakusasiPrice":   price,
		"sSecondPassword":   password,
	}, nil
}
