package broker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
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

// 電文 1 本の待ち時間。
//
// 発注・照会（sUrlRequest）は 30 秒——受理の応答が遅れているだけの可能性があり、
// 早く切ると「結果不明」を増やす。時価問合（sUrlPrice）は 10 秒——1 回の open で
// 10 本以上送るので、1 本 30 秒粘ると寄付の時間帯を食い尽くす。時価は取れなければ
// 次の cron が取り直せばよい。どちらも SetDeadline の締め切りが近ければそこまで。
const (
	requestTimeout = 30 * time.Second
	priceTimeout   = 10 * time.Second
	// netRetryBackoff は通信エラーで照会を送り直すまでの間。
	netRetryBackoff = time.Second
	// responseSnippet はログに残す応答本文の上限（JSON でない応答＝メンテ画面等の切り分け用）。
	responseSnippet = 300
)

// Logger は電文ごとの記録の出口。*cli.Run がこれを満たす。
//
// 「9:01:12 に CLMKabuNewOrder を送って 28 秒待った」という事実は、ここに残さないと
// 後から追えない。nil なら警告だけを stderr に出す。
type Logger interface {
	Info(code, msg string, extra ...map[string]any)
	Warn(code, msg string, extra ...map[string]any)
	Error(code, msg string, extra ...map[string]any)
}

// LoggerSetter は電文の記録先を差し替えられるブローカー。
type LoggerSetter interface {
	SetLogger(Logger)
}

// DeadlineSetter は電文の締め切りを持てるブローカー。
type DeadlineSetter interface {
	SetDeadline(time.Time)
}

// SetDeadline は b が締め切りを持てるなら設定する（ペーパー等は何もしない）。
func SetDeadline(b Broker, deadline time.Time) {
	if d, ok := b.(DeadlineSetter); ok {
		d.SetDeadline(deadline)
	}
}

// ErrDeadline は締め切りを過ぎていたので**電文を送らなかった**。
//
// 送信中に締め切りが来て打ち切った場合はこれではなく通信エラー（届いたか分からない）。
// 発注でこれが返ったら、その注文は確実に出ていない。
type ErrDeadline struct {
	CLMID    string
	Deadline time.Time
}

func (e *ErrDeadline) Error() string {
	return fmt.Sprintf("%s: 締め切り（%s）を過ぎたため送りませんでした",
		e.CLMID, clock.ToZone(e.Deadline, clock.Tokyo).Format("15:04:05"))
}

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

	// logger は電文ごとの記録先。nil なら警告だけ stderr。
	logger Logger
	// deadline はこの接続で送る全電文の締め切り。ゼロ値なら無し。
	deadline time.Time
}

// SetLogger は電文の記録先を差し替える。
func (t *TachibanaBroker) SetLogger(l Logger) { t.logger = l }

// SetDeadline はこの接続で送る全電文の締め切り。
//
// 過ぎていれば送らずに ErrDeadline を返し、送信中なら HTTP をそこで打ち切る。
// 寄付・引けの時間帯の終わり（と 1 回の実行に許す時間）を渡す。ゼロ値で解除。
func (t *TachibanaBroker) SetDeadline(deadline time.Time) { t.deadline = deadline }

// Deadline は設定された締め切り（ゼロ値なら無し）。
func (t *TachibanaBroker) Deadline() time.Time { return t.deadline }

// expired は締め切りを過ぎているか。
func (t *TachibanaBroker) expired() bool {
	return !t.deadline.IsZero() && !time.Now().Before(t.deadline)
}

// canWait は d 待っても締め切りに掛からないか。
func (t *TachibanaBroker) canWait(d time.Duration) bool {
	return t.deadline.IsZero() || time.Now().Add(d).Before(t.deadline)
}

// requestContext は 1 電文の context。timeout と締め切りの近い方で切れる。
// 締め切りを過ぎていれば送らずに ErrDeadline。
func (t *TachibanaBroker) requestContext(clmID string, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if !t.deadline.IsZero() {
		remaining := time.Until(t.deadline)
		if remaining <= 0 {
			return nil, nil, &ErrDeadline{CLMID: clmID, Deadline: t.deadline}
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return ctx, cancel, nil
}

func (t *TachibanaBroker) logInfo(code, msg string, fields map[string]any) {
	if t.logger != nil {
		t.logger.Info(code, msg, fields)
	}
}

func (t *TachibanaBroker) logWarn(code, msg string, fields map[string]any) {
	if t.logger != nil {
		t.logger.Warn(code, msg, fields)
		return
	}
	fmt.Fprintf(os.Stderr, "[warn] %s %v\n", msg, fields)
}

func (t *TachibanaBroker) logError(code, msg string, fields map[string]any) {
	if t.logger != nil {
		t.logger.Error(code, msg, fields)
		return
	}
	fmt.Fprintf(os.Stderr, "[error] %s %v\n", msg, fields)
}

// snippet は応答本文の先頭（ログ用）。
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > responseSnippet {
		return text[:responseSnippet] + "…"
	}
	return text
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
	ctx, cancel, err := t.requestContext(clmLogin, requestTimeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("立花証券ログイン電文の組み立てに失敗しました: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := t.httpClient.Do(req)
	fields := map[string]any{"clm": clmLogin, "iface": "auth", "elapsed_ms": time.Since(started).Milliseconds()}
	if err != nil {
		fields["error"] = err.Error()
		t.logWarn("broker.request_failed", "立花証券ログイン HTTP エラー", fields)
		return nil, fmt.Errorf("立花証券ログインHTTPエラー: %w", err)
	}
	defer resp.Body.Close()
	fields["http_status"] = resp.StatusCode

	utf8Reader := transform.NewReader(resp.Body, japanese.ShiftJIS.NewDecoder())
	respBytes, err := io.ReadAll(utf8Reader)
	if err != nil {
		fields["error"] = err.Error()
		t.logWarn("broker.request_failed", "立花証券ログイン応答読込エラー", fields)
		return nil, fmt.Errorf("立花証券ログイン応答読込エラー: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		fields["body"] = snippet(respBytes)
		t.logWarn("broker.request_failed", "立花証券ログインが HTTP エラー", fields)
		return nil, fmt.Errorf("立花証券ログイン HTTP %d: %s", resp.StatusCode, snippet(respBytes))
	}

	var res map[string]any
	if err := json.Unmarshal(respBytes, &res); err != nil {
		fields["body"] = snippet(respBytes)
		t.logWarn("broker.request_failed", "立花証券ログイン応答が JSON でない", fields)
		return nil, fmt.Errorf("立花証券ログインJSONパースエラー: %w", err)
	}

	pErrno := strings.TrimSpace(text(res["p_errno"]))
	fields["p_errno"] = pErrno
	if pErrno != "0" {
		fields["p_err"] = strings.TrimSpace(text(res["p_err"]))
		t.logWarn("broker.request_failed", "立花証券ログインが拒否された", fields)
		return nil, fmt.Errorf("立花証券ログインエラー p_errno=%s %s", pErrno, strings.TrimSpace(text(res["p_err"])))
	}
	t.logInfo("broker.request", "立花証券API 電文（ログイン）", fields)

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

	// 送り直しは 2 種類を各 1 回まで。通信エラー（届いたか分からない）は照会だけ、
	// セッション失効（p_errno）はログインし直して照会だけ。新規注文はどちらも送り直さない
	netRetried, sessionRetried := false, false
	retryNet := func(stage string, err error) bool {
		var deadline *ErrDeadline
		if netRetried || !resendable(clmID) || errors.As(err, &deadline) || !t.canWait(netRetryBackoff) {
			return false
		}
		netRetried = true
		t.logWarn("broker.retry", "通信エラーのため 1 度だけ送り直す", map[string]any{
			"clm": clmID, "iface": iface, "stage": stage, "error": err.Error(),
			"backoff_ms": netRetryBackoff.Milliseconds(),
		})
		time.Sleep(netRetryBackoff)
		return true
	}
	for {
		if err := t.ensureSessionLocked(sessionPath); err != nil {
			if retryNet("login", err) {
				continue
			}
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
			if retryNet("send", err) {
				continue
			}
			return nil, err
		}
		errno := strings.TrimSpace(text(res["p_errno"]))
		if errno == "" || errno == "0" {
			return res, nil
		}
		t.invalidateSessionLocked(sessionPath)
		if !sessionRetried && resendable(clmID) {
			sessionRetried = true
			t.logWarn("broker.retry", "セッションが失効したためログインし直して送り直す", map[string]any{
				"clm": clmID, "iface": iface, "p_errno": errno, "p_err": strings.TrimSpace(text(res["p_err"])),
			})
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
	timeout := requestTimeout
	switch iface {
	case interfacePrice:
		endpoint = t.session.URLPrice
		timeout = priceTimeout
	case interfaceMaster:
		endpoint = t.session.URLMaster
	}

	// 1 電文 1 行の記録。何を送ったか（電文の種類・銘柄）と、どれだけ待って何が返ったか
	// （所要・HTTP 状態・p_errno・結果コード）。パラメータ本体は残さない——
	// 発注パスワードが入っている
	fields := map[string]any{"clm": clmID, "iface": iface, "p_no": pNo}
	if symbol := strings.TrimSpace(text(params["sIssueCode"])); symbol != "" {
		fields["symbol"] = symbol
	}
	if number := strings.TrimSpace(text(params["sOrderNumber"])); number != "" {
		fields["order_number"] = number
	}
	fail := func(msg string, err error) (map[string]any, error) {
		fields["error"] = err.Error()
		t.logWarn("broker.request_failed", msg, fields)
		return nil, err
	}

	ctx, cancel, err := t.requestContext(clmID, timeout)
	if err != nil {
		return fail("締め切りを過ぎたため送らない", err)
	}
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(sjisBytes))
	if err != nil {
		return nil, fmt.Errorf("立花証券API 電文の組み立てに失敗しました: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := t.httpClient.Do(req)
	fields["elapsed_ms"] = time.Since(started).Milliseconds()
	fields["timeout_ms"] = timeout.Milliseconds()
	if err != nil {
		return fail("立花証券API 通信エラー", fmt.Errorf("立花証券API通信エラー: %w", err))
	}
	defer resp.Body.Close()
	fields["http_status"] = resp.StatusCode

	utf8Reader := transform.NewReader(resp.Body, japanese.ShiftJIS.NewDecoder())
	respBytes, err := io.ReadAll(utf8Reader)
	if err != nil {
		return fail("立花証券API 応答読込エラー", fmt.Errorf("立花証券API応答読込エラー: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		fields["body"] = snippet(respBytes)
		return fail("立花証券API が HTTP エラー", fmt.Errorf("立花証券API HTTP %d: %s", resp.StatusCode, snippet(respBytes)))
	}

	var res map[string]any
	if err := json.Unmarshal(respBytes, &res); err != nil {
		fields["body"] = snippet(respBytes)
		return fail("立花証券API 応答が JSON でない", fmt.Errorf("立花証券API JSONパースエラー: %w", err))
	}
	fields["p_errno"] = strings.TrimSpace(text(res["p_errno"]))
	if code := strings.TrimSpace(text(res["sResultCode"])); code != "" {
		fields["result_code"] = code
	}
	if msg := strings.TrimSpace(text(res["sResultText"])); msg != "" {
		fields["result_text"] = msg
	}
	if number := strings.TrimSpace(text(res["sOrderNumber"])); number != "" {
		fields["order_number"] = number
	}
	t.logInfo("broker.request", "立花証券API 電文", fields)
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
		t.logError("broker.order_number_missing",
			"発注応答に注文番号／営業日がありません。この注文は照会・取消できません",
			map[string]any{"client_order_id": req.ClientOrderID, "symbol": req.Symbol})
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
