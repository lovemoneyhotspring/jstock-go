package rate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

// SourceURL はレーティング一覧のページ（直近 1 か月ぶんが載る）。
const SourceURL = "https://www.grail-legends.com/rate/rate_index.html"

// UserAgent は誰が取りに来ているか分かるようにするためのもの。
const UserAgent = "jstock-go/1.0 (personal research; contact via site owner)"

// ErrTooManyRequests は取得先が 429 を返したとき。呼ぶ側が間隔を広げるために使う。
var ErrTooManyRequests = errors.New("取得先が 429（要求が多すぎる）を返しました")

// ErrServerError は取得先が 5xx を返したとき。一時的なことが多いので取り直す。
var ErrServerError = errors.New("取得先がサーバーエラー（5xx）を返しました")

// FetchWithRetry は一時的な失敗（429・5xx・接続できない・途中で切れた）なら待って取り直す。
// 過去ぶんをまとめて取るときに使う。
//
// 相手の負担になるので、待ち時間は倍々に伸ばし、0.5〜1.5 倍の揺らぎを入れる
// （同時に待っていた回が同じ瞬間に叩き直さないように）。404 などは取り直しても
// 変わらないのですぐ返す。最後の試行のあとは待たない。
func FetchWithRetry(ctx context.Context, target string, attempts int, backoff time.Duration) (string, error) {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		var body string
		body, err = Fetch(ctx, target)
		if err == nil {
			return body, nil
		}
		if !retryable(ctx, err) || i == attempts-1 {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(jitter(backoff << i)):
		}
	}
	return "", err
}

// retryable は取り直せば通るかもしれない失敗か。
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false // 呼び出し側が止めた
	}
	if errors.Is(err, ErrTooManyRequests) || errors.Is(err, ErrServerError) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// 接続できない・タイムアウト（http.Client の失敗は *url.Error で、net.Error を満たす）
	var netErr net.Error
	return errors.As(err, &netErr)
}

// jitter は d を 0.5〜1.5 倍に揺らす。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}

// Fetch は一覧ページを取ってきて本文を返す。
func Fetch(ctx context.Context, target string) (string, error) {
	if target == "" {
		target = SourceURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Language", "ja")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s の取得に失敗: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("%s: %w", target, ErrTooManyRequests)
	}
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%s が %s を返しました: %w", target, resp.Status, ErrServerError)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s が %s を返しました", target, resp.Status)
	}
	// 1 か月ぶんで 400 KB 程度。桁違いに大きければページの作りが変わっている
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("本文の読み取りに失敗: %w", err)
	}
	return string(body), nil
}

// FetchAndSave は一覧を読み（read）、行を記録簿に足し、取りに行った記録を残す。
// 返り値はページの行数と初めて見た行数。
//
// 失敗した回も status にエラーの内容を入れて fetches に残す
// （「取れなかった時間帯」と「載っていなかった時間帯」を混ぜないため）。
func FetchAndSave(ctx context.Context, store *Store, now time.Time, read func(context.Context) (string, error)) (rows, added int, err error) {
	started := time.Now()
	html, err := read(ctx)
	var entries []Entry
	if err == nil {
		entries, err = Parse(html, now)
	}
	if err == nil {
		added, err = store.Save(ctx, entries, now)
	}
	if err != nil {
		if rerr := store.RecordFetch(ctx, now, 0, 0, err.Error(), time.Since(started)); rerr != nil {
			return 0, 0, errors.Join(err, rerr)
		}
		return 0, 0, err
	}
	if err := store.RecordFetch(ctx, now, len(entries), added, "ok", time.Since(started)); err != nil {
		return len(entries), added, err
	}
	return len(entries), added, nil
}
