package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// threadNameLimit は Discord のスレッド名（フォーラムの投稿タイトル）の上限。
const threadNameLimit = 100

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
//
// 送り先はフォーラムチャンネルの Webhook を想定していて、最初のページで
// thread_name により新しい投稿（スレッド）を作り、残りのページはその
// スレッドに thread_id で追記する。固定のスレッド ID は使い回さない
// ——呼ぶたびに別スレッドになる。
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
	threadID := ""
	for i, page := range pages {
		suffix := ""
		if len(pages) > 1 {
			suffix = fmt.Sprintf("\n_(%d/%d)_", i+1, len(pages))
		}
		newThreadName := ""
		if i == 0 {
			newThreadName = threadName(title)
		}
		createdID, perr := postMessage(webhookURL, page+suffix, newThreadName, threadID, true)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", perr)
			ok = false
			continue
		}
		if i == 0 {
			threadID = createdID
		}
		if i < len(pages)-1 {
			time.Sleep(betweenPages)
		}
	}
	return ok, nil
}

// threadName はスレッド名（フォーラムの投稿タイトル）を作る。
// 空・100 文字超えを避ける。
func threadName(title string) string {
	name := strings.TrimSpace(title)
	if name == "" {
		name = "wbjp"
	}
	r := []rune(name)
	if len(r) > threadNameLimit {
		r = r[:threadNameLimit]
	}
	return string(r)
}

// postMessage は 1 通送る。Discord も Slack も通るように text と content の
// 両方を入れる。429（レート制限）は Retry-After を待って 1 度だけやり直す。
//
//   - newThreadName を指定すると、フォーラム/メディアチャンネル向けに
//     新しい投稿（スレッド）を作りながら送る（thread_name）。戻り値の
//     createdThreadID はその投稿のスレッド ID（Discord の応答の channel_id）。
//   - threadID を指定すると、既存のスレッドに追記する（thread_id）。
//
// 両方は同時に使わない。
func postMessage(webhookURL, content, newThreadName, threadID string, mayRetry bool) (createdThreadID string, err error) {
	u, err := url.Parse(webhookURL)
	if err != nil {
		return "", fmt.Errorf("通知先の Webhook URL を解釈できません: %w", err)
	}
	q := u.Query()
	if threadID != "" {
		q.Set("thread_id", threadID)
	}
	if newThreadName != "" {
		// 作った投稿の channel_id（= スレッド ID）を次のページ送信で
		// 使うので、応答を待つ
		q.Set("wait", "true")
	}
	u.RawQuery = q.Encode()

	payload := map[string]any{"content": content, "text": content}
	if newThreadName != "" {
		payload["thread_name"] = newThreadName
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// 省略できない。Discord は Cloudflare の後ろにいて、既定の User-Agent だと
	// 本文を見ずに 403 を返すことがある。
	req.Header.Set("User-Agent", UserAgent)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("通知先 Discord への送信に失敗: %w", err)
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
		return postMessage(webhookURL, content, newThreadName, threadID, false)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("通知先 Discord がエラーを返した: %d", resp.StatusCode)
	}

	if newThreadName == "" {
		return "", nil
	}
	var msg struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return "", fmt.Errorf("通知先 Discord の応答からスレッド ID を読めません: %w", err)
	}
	return msg.ChannelID, nil
}
