package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// retryServer は指定した応答を順に返し、尽きたら 200 を返す受け皿。
// 待ち時間は実際には寝ず、記録だけする。
func retryServer(t *testing.T, statuses ...int) (hits *int32, slept *[]time.Duration) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i < len(statuses) {
			if statuses[i] == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "2")
			}
			w.WriteHeader(statuses[i])
			_, _ = w.Write([]byte(`{"code":0,"message":"x"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg"}`))
	}))
	t.Cleanup(srv.Close)

	old := discordAPIBase
	discordAPIBase = srv.URL
	t.Cleanup(func() { discordAPIBase = old })
	t.Setenv(BotTokenEnvVar, "test-token")

	var waits []time.Duration
	oldSleep := apiSleep
	apiSleep = func(d time.Duration) { waits = append(waits, d) }
	t.Cleanup(func() { apiSleep = oldSleep })
	return &n, &waits
}

// 5xx は送り直し、上限の中で成功すれば成功。
func TestAPIRetriesOn5xx(t *testing.T) {
	hits, slept := retryServer(t, 503, 502)
	if err := PostMessage("ch", "本文"); err != nil {
		t.Fatalf("2 回の 5xx の後に成功するはず: %v", err)
	}
	if *hits != 3 {
		t.Errorf("送信回数 = %d, want 3", *hits)
	}
	if len(*slept) != 2 || (*slept)[0] != apiBackoff || (*slept)[1] != 2*apiBackoff {
		t.Errorf("待ち時間 = %v, want [%v %v]", *slept, apiBackoff, 2*apiBackoff)
	}
}

// 上限を超えたら最後のエラーを返す（無限に粘らない）。
func TestAPIGivesUpAfterAttempts(t *testing.T) {
	hits, _ := retryServer(t, 500, 500, 500, 500)
	err := PostMessage("ch", "本文")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("3 回とも 5xx なら失敗: %v", err)
	}
	if *hits != int32(apiAttempts) {
		t.Errorf("送信回数 = %d, want %d", *hits, apiAttempts)
	}
}

// 429 は Retry-After を待って送り直す。
func TestAPIHonorsRetryAfter(t *testing.T) {
	hits, slept := retryServer(t, 429)
	if err := PostMessage("ch", "本文"); err != nil {
		t.Fatal(err)
	}
	if *hits != 2 || len(*slept) != 1 || (*slept)[0] != 2*time.Second {
		t.Errorf("hits = %d, 待ち = %v, want 2 回・2s", *hits, *slept)
	}
}

// 429 以外の 4xx は設定の誤りなので送り直さない。
func TestAPIDoesNotRetry4xx(t *testing.T) {
	hits, slept := retryServer(t, 403)
	if err := PostMessage("ch", "本文"); err == nil {
		t.Fatal("403 は失敗")
	}
	if *hits != 1 || len(*slept) != 0 {
		t.Errorf("hits = %d, 待ち = %v, want 1 回・待ちなし", *hits, *slept)
	}
}

// 接続できない（ネットワークエラー）も送り直す。
func TestAPIRetriesNetworkError(t *testing.T) {
	_, slept := retryServer(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	discordAPIBase = srv.URL
	srv.Close() // 閉じた先に送る → 接続エラー
	err := PostMessage("ch", "本文")
	if err == nil || !strings.Contains(err.Error(), "送信に失敗") {
		t.Fatalf("接続エラーは失敗: %v", err)
	}
	if len(*slept) != apiAttempts-1 {
		t.Errorf("待ちの回数 = %d, want %d（%d 回試す）", len(*slept), apiAttempts-1, apiAttempts)
	}
}

// Alert は送れなかったとき false（送り直しても届かなければ）。
func TestAlertReturnsFalseWhenUndeliverable(t *testing.T) {
	retryServer(t, 500, 500, 500)
	t.Setenv(AlertChannelEnvVar, "alert-ch")
	t.Setenv(MentionEnvVar, "")
	t.Setenv(archiveDirEnvVar, t.TempDir())
	if Alert("障害", "詳細", nil) {
		t.Error("届いていないのに true")
	}
}
