package rate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// statusServer は calls 回目の応答を codes から返す（尽きたら 200 と本文）。
func statusServer(t *testing.T, codes ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if n <= len(codes) {
			w.WriteHeader(codes[n-1])
			return
		}
		fmt.Fprint(w, "本文")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestFetchWithRetryRetriesTransientErrors(t *testing.T) {
	cases := []struct {
		name  string
		codes []int
	}{
		{"429", []int{429}},
		{"5xx", []int{503, 500}},
		{"混在", []int{502, 429}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, calls := statusServer(t, c.codes...)
			body, err := FetchWithRetry(context.Background(), srv.URL, 5, time.Millisecond)
			if err != nil || body != "本文" {
				t.Fatalf("body=%q err=%v", body, err)
			}
			if got := int(calls.Load()); got != len(c.codes)+1 {
				t.Errorf("呼び出し = %d, want %d", got, len(c.codes)+1)
			}
		})
	}
}

// 404 は取り直しても変わらないので 1 回で返す。
func TestFetchWithRetryDoesNotRetryClientErrors(t *testing.T) {
	srv, calls := statusServer(t, 404, 404, 404)
	if _, err := FetchWithRetry(context.Background(), srv.URL, 5, time.Millisecond); err == nil {
		t.Fatal("404 で成功した")
	}
	if calls.Load() != 1 {
		t.Errorf("404 を取り直した: %d 回", calls.Load())
	}
}

// 回数を使い切ったら最後の失敗を返す（最後の試行のあとは待たない）。
func TestFetchWithRetryGivesUp(t *testing.T) {
	srv, calls := statusServer(t, 500, 500, 500, 500)
	_, err := FetchWithRetry(context.Background(), srv.URL, 3, time.Millisecond)
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("呼び出し = %d, want 3", calls.Load())
	}
}

// つながらないのも一時的な失敗として取り直す。
func TestFetchWithRetryRetriesNetworkErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	target := srv.URL
	srv.Close() // 閉じたのでつながらない
	_, err := Fetch(context.Background(), target)
	if err == nil {
		t.Fatal("閉じたサーバーに届いた")
	}
	if !retryable(context.Background(), err) {
		t.Errorf("接続の失敗を取り直さない: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if retryable(ctx, err) {
		t.Error("止めた後も取り直そうとする")
	}
}

func TestFetchWithRetryStopsWhenCanceled(t *testing.T) {
	srv, calls := statusServer(t, 500, 500, 500)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := FetchWithRetry(ctx, srv.URL, 3, time.Hour); err == nil {
		t.Fatal("成功した")
	}
	if time.Since(started) > 5*time.Second || calls.Load() != 1 {
		t.Errorf("止めても待ち続けた: %v, %d 回", time.Since(started), calls.Load())
	}
}

func TestJitter(t *testing.T) {
	d := 100 * time.Millisecond
	for i := 0; i < 200; i++ {
		if got := jitter(d); got < d/2 || got >= d*3/2 {
			t.Fatalf("jitter(%v) = %v", d, got)
		}
	}
	if jitter(0) != 0 {
		t.Error("0 は 0")
	}
}

func openRateStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "rate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func fetchRows(t *testing.T, store *Store) [][3]string {
	t.Helper()
	rows, err := store.DB().Query(`SELECT rows, new_rows, status FROM fetches ORDER BY fetched_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var r, n int
		var status string
		if err := rows.Scan(&r, &n, &status); err != nil {
			t.Fatal(err)
		}
		out = append(out, [3]string{fmt.Sprint(r), fmt.Sprint(n), status})
	}
	return out
}

func TestFetchAndSave(t *testing.T) {
	store := openRateStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 7, 30, 0, 0, jst)

	// 行を読めないページは失敗で、status にその内容が残る
	_, _, err := FetchAndSave(ctx, store, now, func(context.Context) (string, error) { return "<html>メンテ中</html>", nil })
	if err == nil {
		t.Fatal("読めないページで成功した")
	}
	// 取れなかった回も残る
	_, _, err = FetchAndSave(ctx, store, now.Add(5*time.Minute), func(context.Context) (string, error) {
		return "", errors.New("つながらない")
	})
	if err == nil {
		t.Fatal("取れないのに成功した")
	}
	rows, added, err := FetchAndSave(ctx, store, now.Add(10*time.Minute), func(context.Context) (string, error) { return sample, nil })
	if err != nil || rows != 4 || added != 4 {
		t.Fatalf("rows=%d added=%d err=%v", rows, added, err)
	}

	got := fetchRows(t, store)
	if len(got) != 3 {
		t.Fatalf("fetches = %v", got)
	}
	if got[0][2] == "ok" || !strings.Contains(got[0][2], "行が 1 つも取れません") {
		t.Errorf("読めない回の status = %q", got[0][2])
	}
	if got[1][2] != "つながらない" {
		t.Errorf("取れない回の status = %q", got[1][2])
	}
	if got[2] != [3]string{"4", "4", "ok"} {
		t.Errorf("成功の回 = %v", got[2])
	}
}
