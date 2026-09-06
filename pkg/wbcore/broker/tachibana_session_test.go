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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
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
}

func newFakeTachibana(t *testing.T, pub *rsa.PublicKey) *fakeTachibana {
	f := &fakeTachibana{t: t, pub: pub, failErrno: "-62"}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"p_errno":     "0",
			"sUrlRequest": f.encrypt(f.server.URL + "/request/"),
			"sUrlPrice":   f.encrypt(f.server.URL + "/price/"),
			"sUrlMaster":  f.encrypt(f.server.URL + "/master/"),
		})
		return
	}
	pNo, _ := req["p_no"].(float64)
	f.pNos = append(f.pNos, int(pNo))
	f.clmIDs = append(f.clmIDs, text(req["sCLMID"]))
	if r.URL.Path == "/price/" {
		codes := strings.Split(text(req["sTargetIssueCode"]), ",")
		f.priceBatches = append(f.priceBatches, codes[0])
		if f.priceFail[codes[0]] > 0 {
			f.priceFail[codes[0]]--
			w.WriteHeader(http.StatusInternalServerError)
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
		_ = json.NewEncoder(w).Encode(map[string]any{"p_errno": f.failErrno, "p_err": "session expired"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"p_errno": "0", "sResultCode": "0", "path": r.URL.Path,
	})
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
