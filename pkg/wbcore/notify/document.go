package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ChunkLimit は 1 通の文字数の上限。
//
// Discord の 1 通の上限は 2000 文字。ページ番号（"_(2/3)_"）に使う余白を
// 引いてある。超えると 400 が返り、レポートが丸ごと消える。
const ChunkLimit = 1900

// betweenPages は連投のレート制限を避けるための間隔。
const betweenPages = 500 * time.Millisecond

// Chunks は改行の位置で limit 文字以内に切る。
// 1 行が長すぎるときだけ行の途中で切る。
func Chunks(text string, limit int) []string {
	if limit <= 0 {
		limit = ChunkLimit
	}
	var out []string
	var buf strings.Builder

	flush := func() {
		if buf.Len() > 0 {
			out = append(out, buf.String())
			buf.Reset()
		}
	}

	for _, line := range splitKeepEnds(text) {
		for len([]rune(line)) > limit { // 1 行が上限を超える異常な入力
			flush()
			runes := []rune(line)
			out = append(out, string(runes[:limit]))
			line = string(runes[limit:])
		}
		if len([]rune(buf.String()))+len([]rune(line)) > limit {
			flush()
		}
		buf.WriteString(line)
	}
	if strings.TrimSpace(buf.String()) != "" {
		out = append(out, buf.String())
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

// splitKeepEnds は改行を残したまま行に分ける。
func splitKeepEnds(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			lines = append(lines, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// PostDocument は本文を Webhook に流す（日次レポートの配達係）。
//
// Alert との違いは 2 つ。[wbjp] の接頭辞を付けないこと（レポートは自分で
// 見出しを持っている）と、上限で分割して連投すること。
// 送り先が未設定なら何もせず false を返す——レポート本体はファイルに
// 残っているので、配達の失敗で処理は止めない。
func PostDocument(body, title string) (ok bool, err error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return false, fmt.Errorf("本文が空です")
	}
	webhookURL := os.Getenv(WebhookEnvVar)
	if webhookURL == "" {
		return false, fmt.Errorf("%s が未設定です", WebhookEnvVar)
	}
	if title != "" {
		body = fmt.Sprintf("**%s**\n%s", title, body)
	}

	pages := Chunks(body, ChunkLimit)
	ok = true
	for i, page := range pages {
		suffix := ""
		if len(pages) > 1 {
			suffix = fmt.Sprintf("\n_(%d/%d)_", i+1, len(pages))
		}
		if perr := postOnce(webhookURL, page+suffix, true); perr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", perr)
			ok = false
		}
		if i < len(pages)-1 {
			time.Sleep(betweenPages)
		}
	}
	return ok, nil
}

// postOnce は 1 通送る。Discord も Slack も通るように text と content の
// 両方を入れる。429（レート制限）は Retry-After を待って 1 度だけやり直す。
func postOnce(webhookURL, content string, mayRetry bool) error {
	payload, err := json.Marshal(map[string]string{"content": content, "text": content})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// 省略できない。Discord は Cloudflare の後ろにいて、既定の User-Agent だと
	// 本文を見ずに 403 を返すことがある。
	req.Header.Set("User-Agent", UserAgent)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("通知先 Discord への送信に失敗: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests && mayRetry {
		wait := 1.0
		if v, perr := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); perr == nil && v > 0 {
			wait = v
		}
		if wait > 30 {
			wait = 30
		}
		time.Sleep(time.Duration(wait * float64(time.Second)))
		return postOnce(webhookURL, content, false)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("通知先 Discord がエラーを返した: %d", resp.StatusCode)
	}
	return nil
}
