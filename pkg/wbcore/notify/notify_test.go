package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postMessage が新しいスレッドを作るとき、thread_name をボディに、
// wait=true をクエリに付け、応答の channel_id をスレッド ID として返す。
func TestPostMessageCreatesThreadAndReturnsID(t *testing.T) {
	var gotQuery url.Values
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"channel_id": "999888777"})
	}))
	defer srv.Close()

	id, err := postMessage(srv.URL, "本文", "新しいスレッド", "", true)
	if err != nil {
		t.Fatalf("postMessage error: %v", err)
	}
	if id != "999888777" {
		t.Errorf("threadID = %q, want 999888777", id)
	}
	if gotQuery.Get("wait") != "true" {
		t.Errorf("wait=true が付いていない: %v", gotQuery)
	}
	if gotBody["thread_name"] != "新しいスレッド" {
		t.Errorf("thread_name が本文に無い: %v", gotBody)
	}
}

// 既存スレッドへの追記は thread_id をクエリに付け、thread_name は送らない。
func TestPostMessageToExistingThreadUsesThreadID(t *testing.T) {
	var gotQuery url.Values
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if _, err := postMessage(srv.URL, "2ページ目", "", "12345", true); err != nil {
		t.Fatalf("postMessage error: %v", err)
	}
	if gotQuery.Get("thread_id") != "12345" {
		t.Errorf("thread_id が付いていない: %v", gotQuery)
	}
	if _, ok := gotBody["thread_name"]; ok {
		t.Errorf("既存スレッドへの投稿に thread_name が付いた: %v", gotBody)
	}
}

// 複数ページに分かれる本文は、最初のページだけ新規スレッドを作り、
// 残りは同じスレッド ID に追記する（固定 ID の使い回しではなく、
// PostDocument の呼び出しごとに新しいスレッドになる）。
func TestPostDocumentUsesOneThreadAcrossPages(t *testing.T) {
	var threadIDsUsed []string
	var threadNamesUsed []string
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		q := r.URL.Query()
		threadIDsUsed = append(threadIDsUsed, q.Get("thread_id"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if tn, ok := body["thread_name"].(string); ok {
			threadNamesUsed = append(threadNamesUsed, tn)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"channel_id": "thread-abc"})
	}))
	defer srv.Close()

	t.Setenv(WebhookEnvVar, srv.URL)
	long := strings.Repeat("x", 2500) // ChunkLimit(1900) 超えで複数ページになる
	ok, err := PostDocument(long, "日次レポート")
	if err != nil || !ok {
		t.Fatalf("PostDocument failed: ok=%v err=%v", ok, err)
	}
	if callCount < 2 {
		t.Fatalf("複数ページに分かれていない: callCount=%d", callCount)
	}
	if threadIDsUsed[0] != "" {
		t.Errorf("最初のページで thread_id が指定された: %q", threadIDsUsed[0])
	}
	for i, id := range threadIDsUsed[1:] {
		if id != "thread-abc" {
			t.Errorf("%d 番目のページが新規スレッドの ID を使っていない: %q", i+2, id)
		}
	}
	if len(threadNamesUsed) != 1 {
		t.Errorf("thread_name を送ったのは1回であるべき: %d 回", len(threadNamesUsed))
	}
}

func TestThreadNameTruncatesAndFallsBack(t *testing.T) {
	if got := threadName(""); got != "wbjp" {
		t.Errorf("空文字のフォールバック = %q", got)
	}
	long := strings.Repeat("あ", 150)
	got := threadName(long)
	if n := len([]rune(got)); n != threadNameLimit {
		t.Errorf("%d 文字に切り詰められていない: %d 文字", threadNameLimit, n)
	}
}

// Alert は呼ぶたびに thread_name 付きで送る＝毎回別スレッドになる。
func TestAlertUsesNewThreadPerCall(t *testing.T) {
	var threadNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if tn, ok := body["thread_name"].(string); ok {
			threadNames = append(threadNames, tn)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"channel_id": "t1"})
	}))
	defer srv.Close()

	t.Setenv(WebhookEnvVar, srv.URL)
	if !Alert("障害A", "詳細A", nil) {
		t.Fatal("1回目の Alert が失敗")
	}
	if !Alert("障害A", "詳細A", nil) {
		t.Fatal("2回目の Alert が失敗")
	}
	if len(threadNames) != 2 {
		t.Fatalf("thread_name 付きの投稿が2回されていない: %d 回", len(threadNames))
	}
}
