package broker

import (
	"errors"
	"testing"
)

// 認証の拒否は再試行せず、同じプロセスでは 2 度目のログインも送らない（口座のロックを避ける）
func TestLoginRejectionIsNotRetriedAndSticks(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.loginExtra = map[string]any{"sResultCode": "10031", "sResultText": "password", "sUrlRequest": ""}

	for i := 0; i < 3; i++ {
		_, err := b.postRequest(clmBalanceSummary, nil)
		var rejected *ErrLoginRejected
		if !errors.As(err, &rejected) {
			t.Fatalf("%d 回目: ErrLoginRejected でない: %v", i, err)
		}
	}
	if fake.logins != 1 {
		t.Errorf("拒否のあともログインを送っている: logins=%d, want 1", fake.logins)
	}

	// 同じプロセスの別のブローカー（同じ接続先・同じ ID）もログインしない
	b2 := newSessionTestBroker(t, fake, b.creds.PrivateKeyFile, t.TempDir())
	if _, err := b2.postRequest(clmBalanceSummary, nil); err == nil {
		t.Fatal("拒否された ID でログインが通った")
	}
	if fake.logins != 1 {
		t.Errorf("別のブローカーがログインを送っている: logins=%d", fake.logins)
	}
}

// p_errno の拒否も再試行しない。時間外（-62）はプロセスに固定せず、次の電文でログインし直す
func TestLoginOutsideHoursIsNotSticky(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.loginExtra = map[string]any{"p_errno": pErrnoOutsideHours, "p_err": "outside hours"}
	if _, err := b.postRequest(clmBalanceSummary, nil); err == nil {
		t.Fatal("時間外のログインが通った")
	}
	if fake.logins != 1 {
		t.Errorf("時間外のログインを再試行している: logins=%d", fake.logins)
	}
	fake.mu.Lock()
	fake.loginExtra = nil
	fake.mu.Unlock()
	if _, err := b.postRequest(clmBalanceSummary, nil); err != nil {
		t.Fatalf("時間が明けてもログインできない: %v", err)
	}
	if fake.logins != 2 {
		t.Errorf("logins=%d, want 2", fake.logins)
	}
}
