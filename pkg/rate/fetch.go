package rate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SourceURL はレーティング一覧のページ（直近 1 か月ぶんが載る）。
const SourceURL = "https://www.grail-legends.com/rate/rate_index.html"

// UserAgent は誰が取りに来ているか分かるようにするためのもの。
const UserAgent = "jstock-go/1.0 (personal research; contact via site owner)"

// ErrTooManyRequests は取得先が 429 を返したとき。呼ぶ側が間隔を広げるために使う。
var ErrTooManyRequests = errors.New("取得先が 429（要求が多すぎる）を返しました")

// FetchWithRetry は 429 を受けたら待って取り直す。過去ぶんをまとめて取るときに使う。
// 相手の負担になるので、待ち時間は倍々に伸ばす。
func FetchWithRetry(ctx context.Context, url string, attempts int, backoff time.Duration) (string, error) {
	var err error
	for i := 0; i < attempts; i++ {
		var body string
		body, err = Fetch(ctx, url)
		if err == nil {
			return body, nil
		}
		if !errors.Is(err, ErrTooManyRequests) {
			return "", err
		}
		wait := backoff << i
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
	return "", err
}

// Fetch は一覧ページを取ってきて本文を返す。
func Fetch(ctx context.Context, url string) (string, error) {
	if url == "" {
		url = SourceURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Language", "ja")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s の取得に失敗: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("%s: %w", url, ErrTooManyRequests)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s が %s を返しました", url, resp.Status)
	}
	// 1 か月ぶんで 400 KB 程度。桁違いに大きければページの作りが変わっている
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("本文の読み取りに失敗: %w", err)
	}
	return string(body), nil
}
