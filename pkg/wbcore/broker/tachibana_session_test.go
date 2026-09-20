package broker

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

// fakeTachibana は立花 API の最小の模型。ログインで仮想URL を返し、以後の電文の
// p_no を記録する。p_errno を仕込むと「セッション失効」を演じる。
type fakeTachibana struct {
	t      *testing.T
	pub    *rsa.PublicKey
	server *httptest.Server

	mu        sync.Mutex
	logins    int
	pNos      []int
	clmIDs    []string
	failNext  int // 次の n 電文に p_errno を返す
	failErrno string
	// failHTTPNext は次の n 電文を HTTP 500 で返す（通信エラーの模型）。
	failHTTPNext int
	// priceFail は時価問合で、先頭の銘柄がこのコードのバッチをあと n 回 HTTP 500 にする。
	priceFail map[string]int
	// priceBatches は時価問合で受け取ったバッチの先頭銘柄（送信順）。
	priceBatches []string
	// priceOmitRows は時価問合の応答から配列のキーを落とす（形が違う応答の模型）。
	priceOmitRows bool
	// responses は電文（sCLMID）ごとの応答の固定値。無ければ既定の {"p_errno":"0","sResultCode":"0"}。
	// 実機で確かめた行の形（docs/BROKER_VERIFY.md）をここに仕込む。
	responses map[string]map[string]any
	// payloads は受け取った電文（ログイン以外・送信順）。発注の中身を確かめるのに使う。
	payloads []map[string]any
	// loginExtra はログイン応答に足す項目（sResultCode の業務エラーを仕込む等）。
	loginExtra map[string]any
}

func newFakeTachibana(t *testing.T, pub *rsa.PublicKey) *fakeTachibana {
	// 既定の p_errno は 2（セッション切断）。-1 / -62 はセッションを捨てない別の扱いになる
	f := &fakeTachibana{t: t, pub: pub, failErrno: pErrnoSessionLost, responses: map[string]map[string]any{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// lastPayload は clmID の電文で最後に受け取ったもの。無ければ nil。
func (f *fakeTachibana) lastPayload(clmID string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.payloads) - 1; i >= 0; i-- {
		if text(f.payloads[i]["sCLMID"]) == clmID {
			return f.payloads[i]
		}
	}
	return nil
}

// countCLM は clmID の電文を受け取った回数。
func (f *fakeTachibana) countCLM(clmID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.clmIDs {
		if c == clmID {
			n++
		}
	}
	return n
}

func (f *fakeTachibana) encrypt(url string) string {
	cipher, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, f.pub, []byte(url), nil)
	if err != nil {
		f.t.Fatalf("暗号化に失敗: %v", err)
	}
	return base64.StdEncoding.EncodeToString(cipher)
}

func (f *fakeTachibana) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/auth/" {
		f.logins++
		login := map[string]any{
			"p_errno":     "0",
			"sResultCode": "0",
			"sUrlRequest": f.encrypt(f.server.URL + "/request/"),
			"sUrlPrice":   f.encrypt(f.server.URL + "/price/"),
			"sUrlMaster":  f.encrypt(f.server.URL + "/master/"),
		}
		for k, v := range f.loginExtra {
			login[k] = v
		}
		_ = json.NewEncoder(w).Encode(login)
		return
	}
	// p_no は文字列で送る決まり（数値だと本番の基盤が p_errno=-1 で弾く）。
	// 控えが数値で来たらここで気づけるように、文字列以外は記録しない
	pNo, _ := strconv.Atoi(text(req["p_no"]))
	f.pNos = append(f.pNos, pNo)
	f.clmIDs = append(f.clmIDs, text(req["sCLMID"]))
	f.payloads = append(f.payloads, req)
	if r.URL.Path == "/price/" {
		codes := strings.Split(text(req["sTargetIssueCode"]), ",")
		f.priceBatches = append(f.priceBatches, codes[0])
		if f.priceFail[codes[0]] > 0 {
			f.priceFail[codes[0]]--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if f.priceOmitRows {
			_ = json.NewEncoder(w).Encode(map[string]any{"p_errno": "0"})
			return
		}
		rows := make([]map[string]any, 0, len(codes))
		for _, c := range codes {
			rows = append(rows, map[string]any{"sIssueCode": c, "pDPP": "1000", "pPRP": "1010"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"p_errno": "0", "aCLMMfdsMarketPrice": rows})
		return
	}
	if f.failHTTPNext > 0 {
		f.failHTTPNext--
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html>maintenance</html>"))
		return
	}
	if f.failNext > 0 {
		f.failNext--
		_ = json.NewEncoder(w).Encode(map[string]any{"p_errno": f.failErrno, "p_err": "platform error " + f.failErrno})
		return
	}
	if res, ok := f.responses[text(req["sCLMID"])]; ok {
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"p_errno": "0", "sResultCode": "0", "path": r.URL.Path,
	})
}

// recordingLogger は電文の記録を貯める Logger（警告が残ることを確かめる）。
type recordingLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	level, code, msg string
	extra            map[string]any
}

func (l *recordingLogger) record(level, code, msg string, extra []map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	merged := map[string]any{}
	for _, m := range extra {
		for k, v := range m {
			merged[k] = v
		}
	}
	l.entries = append(l.entries, logEntry{level: level, code: code, msg: msg, extra: merged})
}

func (l *recordingLogger) Info(code, msg string, extra ...map[string]any) {
	l.record("info", code, msg, extra)
}
func (l *recordingLogger) Warn(code, msg string, extra ...map[string]any) {
	l.record("warn", code, msg, extra)
}
func (l *recordingLogger) Error(code, msg string, extra ...map[string]any) {
	l.record("error", code, msg, extra)
}

// find は level と code の一致する記録。
func (l *recordingLogger) find(level, code string) []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []logEntry
	for _, e := range l.entries {
		if e.level == level && e.code == code {
			out = append(out, e)
		}
	}
	return out
}

// newSessionTestBroker は模型に繋ぐブローカー。stateDir を共有すると別プロセスの体になる。
func newSessionTestBroker(t *testing.T, fake *fakeTachibana, keyPath, stateDir string) *TachibanaBroker {
	t.Helper()
	b, err := NewTachibanaBroker(settings.EnvUAT, &credentials.TachibanaCredentials{
		AuthID: "test", PrivateKeyFile: keyPath, OrderPassword: "x",
	}, stateDir)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	b.baseURL = fake.server.URL + "/"
	return b
}

func writeTestKey(t *testing.T, dir string) (string, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "key.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path, &key.PublicKey
}

func TestSessionNumberingIsSharedAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)

	// 同じ state を見る 2 つのブローカー = 別プロセスの体
	a := newSessionTestBroker(t, fake, keyPath, dir)
	b := newSessionTestBroker(t, fake, keyPath, dir)

	for i := 0; i < 3; i++ {
		if _, err := a.postRequest(clmBalanceSummary, nil); err != nil {
			t.Fatalf("a: %v", err)
		}
		if _, err := b.postPriceRequest(clmMarketPrice, nil); err != nil {
			t.Fatalf("b: %v", err)
		}
	}
	if fake.logins != 1 {
		t.Errorf("ログインは 1 回で足りる: %d", fake.logins)
	}
	want := []int{2, 3, 4, 5, 6, 7}
	if len(fake.pNos) != len(want) {
		t.Fatalf("p_no の数: %v", fake.pNos)
	}
	for i, n := range want {
		if fake.pNos[i] != n {
			t.Fatalf("p_no が単調に進んでいない: %v", fake.pNos)
		}
	}
}

func TestSessionExpiryRelogsInForQueriesOnly(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)

	// 照会: 失効 → 再ログイン → 送り直し
	fake.failNext = 1
	if _, err := b.postRequest(clmBalanceSummary, nil); err != nil {
		t.Fatalf("照会は送り直されるはず: %v", err)
	}
	if fake.logins != 2 {
		t.Errorf("再ログインされていない: logins=%d", fake.logins)
	}
	if got := fake.clmIDs; len(got) != 2 || got[0] != clmBalanceSummary || got[1] != clmBalanceSummary {
		t.Errorf("送り直しの電文: %v", got)
	}

	// 発注: 失効 → 送り直さずにエラー（セッションは捨てる）
	fake.failNext = 1
	_, err := b.postRequest(clmNewOrder, nil)
	var sessionErr *ErrSession
	if !errors.As(err, &sessionErr) {
		t.Fatalf("発注は ErrSession で止まるはず: %v", err)
	}
	if fake.clmIDs[len(fake.clmIDs)-1] != clmNewOrder || len(fake.clmIDs) != 3 {
		t.Errorf("発注が送り直されている: %v", fake.clmIDs)
	}
	if _, ok := readSessionFile(b.sessionFilePath()); ok {
		t.Error("失効したセッションファイルが残っている")
	}
	// 次の電文でログインし直せる
	if _, err := b.postRequest(clmBalanceSummary, nil); err != nil {
		t.Fatalf("再ログイン後の照会: %v", err)
	}
	if fake.logins != 3 {
		t.Errorf("logins=%d", fake.logins)
	}
}

func TestLoginRejectsUndecodableURL(t *testing.T) {
	dir := t.TempDir()
	keyPath, _ := writeTestKey(t, dir)
	// 別の鍵で暗号化された URL は復号できない → ログインを失敗にする
	_, otherPub := writeTestKey(t, t.TempDir())
	fake := newFakeTachibana(t, otherPub)
	b := newSessionTestBroker(t, fake, keyPath, dir)
	if _, err := b.postRequest(clmBalanceSummary, nil); err == nil {
		t.Fatal("復号できない仮想URL でログインが通ってはいけない")
	}
}

// TestQueryIsResentOnceOnHTTPError は、照会が通信エラー（HTTP 500）になったら 1 度だけ送り直すこと。
func TestQueryIsResentOnceOnHTTPError(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)

	fake.failHTTPNext = 1
	if _, err := b.postRequest(clmOrderList, map[string]any{}); err != nil {
		t.Fatalf("1 度の通信エラーで諦めた: %v", err)
	}
	if got := len(fake.clmIDs); got != 2 {
		t.Fatalf("送信回数 %d, want 2（失敗 1 ＋ 再送 1）", got)
	}

	// 2 度続けて失敗したら諦める（無限に粘らない）
	fake.failHTTPNext = 2
	if _, err := b.postRequest(clmOrderList, map[string]any{}); err == nil {
		t.Fatal("2 度目の失敗で諦めていない")
	}
	if got := len(fake.clmIDs); got != 4 {
		t.Fatalf("送信回数 %d, want 4", got)
	}
}

// TestNewOrderIsNotResentOnHTTPError は、新規注文は通信エラーでも送り直さないこと
// （届いていた場合に二重発注になる）。
func TestNewOrderIsNotResentOnHTTPError(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)

	fake.failHTTPNext = 1
	_, err := b.postRequest(clmNewOrder, map[string]any{"sIssueCode": "7203"})
	if err == nil {
		t.Fatal("HTTP 500 が成功になった")
	}
	var deadline *ErrDeadline
	if errors.As(err, &deadline) {
		t.Fatalf("通信エラーが締め切りとして返った: %v", err)
	}
	if got := len(fake.clmIDs); got != 1 {
		t.Fatalf("新規注文の送信回数 %d, want 1（送り直してはいけない）", got)
	}
}

// TestDeadlinePreventsSending は、締め切りを過ぎていれば電文を送らずに ErrDeadline を返すこと。
func TestDeadlinePreventsSending(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)

	// まず 1 本通してセッションを作る
	if _, err := b.postRequest(clmOrderList, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	sent := len(fake.clmIDs)

	b.SetDeadline(time.Now().Add(-time.Second))
	_, err := b.postRequest(clmNewOrder, map[string]any{"sIssueCode": "7203"})
	var deadline *ErrDeadline
	if !errors.As(err, &deadline) {
		t.Fatalf("ErrDeadline ではない: %v", err)
	}
	if len(fake.clmIDs) != sent {
		t.Fatal("締め切り後に電文が送られた")
	}
	if _, err := b.MarketPricesRaw([]string{"7203"}, ""); err == nil {
		t.Fatal("締め切り後の時価問合が成功した")
	}

	// 解除すれば送れる
	b.SetDeadline(time.Time{})
	if _, err := b.postRequest(clmOrderList, map[string]any{}); err != nil {
		t.Fatalf("解除後に送れない: %v", err)
	}
}

// 立花証券が配る秘密鍵は DER（e_api_private_key.der）。PEM に変換しなくても読めること。
func TestParseRSAPrivateKeyAcceptsPEMAndDER(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"PKCS1 DER": pkcs1,
		"PKCS8 DER": pkcs8,
		"PKCS1 PEM": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: pkcs1}),
		"PKCS8 PEM": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
		// 立花証券が実際に配る形。拡張子は .der だが中身は base64 テキスト（ヘッダ無し）
		"PKCS8 base64":    []byte(base64.StdEncoding.EncodeToString(pkcs8)),
		"PKCS1 base64+改行": []byte(wrap76(base64.StdEncoding.EncodeToString(pkcs1))),
	}
	for name, data := range cases {
		got, err := parseRSAPrivateKey(data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !got.Equal(key) {
			t.Errorf("%s: 読めた鍵が元と違う", name)
		}
	}
	if _, err := parseRSAPrivateKey([]byte("これは鍵ではない")); err == nil {
		t.Error("鍵でないデータが通ってしまう")
	}
}

// wrap76 は 76 文字ごとに改行を入れる（配られるファイルは折り返してある）。
func wrap76(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i += 76 {
		end := min(i+76, len(text))
		b.WriteString(text[i:end])
		b.WriteString("\n")
	}
	return b.String()
}

// TestRepaymentQueryFailureIsNotSent は、返済注文で建玉の照会が通信エラーになったら
// 「送っていない」（ErrNotSent）として返すこと。結果不明にすると、届いてもいない注文を
// 一覧照会で判定するまで送り直せない。
func TestRepaymentQueryFailureIsNotSent(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)

	fake.failHTTPNext = 2 // 照会は 1 度送り直すので、2 回落とせば諦める
	_, err := b.Place(domain.OrderRequest{
		ClientOrderID: "c1", Symbol: "7203", Side: domain.SideBuy,
		OrderType: domain.OrderTypeMarket, Quantity: decimal.NewFromInt(100),
		Trade: domain.TradeTypeMarginClose,
	})
	if err == nil {
		t.Fatal("建玉を照会できないのに発注が通った")
	}
	var notSent *ErrNotSent
	if !errors.As(err, &notSent) {
		t.Fatalf("送る前の失敗が ErrNotSent でない: %v", err)
	}
	for _, clm := range fake.clmIDs {
		if clm == clmNewOrder {
			t.Fatal("建玉を照会できないのに新規注文の電文を送った")
		}
	}
}

// 本番は state の無い場所からブローカーを作らせない（別のセッションファイルで新規ログインし、
// 動いている本番のセッションを切るため）。UAT は従来どおり作れる。
func TestNewTachibanaBrokerRefusesProdWithoutStateDir(t *testing.T) {
	dir := t.TempDir()
	keyPath, _ := writeTestKey(t, dir)
	creds := &credentials.TachibanaCredentials{AuthID: "test", PrivateKeyFile: keyPath, OrderPassword: "x"}
	missing := filepath.Join(dir, "無い", "state")

	if _, err := NewTachibanaBroker(settings.EnvProd, creds, missing); err == nil {
		t.Error("state が無いのに本番のブローカーを作れました")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("state を黙って作っています")
	}
	if _, err := NewTachibanaBroker(settings.EnvProd, creds, dir); err != nil {
		t.Errorf("state が在るのに拒否されました: %v", err)
	}
	if _, err := NewTachibanaBroker(settings.EnvUAT, creds, missing); err != nil {
		t.Errorf("UAT まで拒否されました: %v", err)
	}
}
