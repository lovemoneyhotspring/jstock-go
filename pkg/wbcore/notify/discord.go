package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// discordAPIBase は Discord の REST API。テストで差し替える。
var discordAPIBase = "https://discord.com/api/v10"

// threadAutoArchiveMinutes はスレッドが自動でアーカイブされるまでの分（1 日）。
// アーカイブされても内容は残り、書き込めば戻る。
const threadAutoArchiveMinutes = 1440

// betweenPages は連投のレート制限を避けるための間隔。
const betweenPages = 500 * time.Millisecond

// Discord のチャンネル種別（GET /channels/{id} の type）。
const (
	channelText         = 0
	channelAnnouncement = 5
	channelForum        = 15
	channelMedia        = 16
)

// isThreadChannel はスレッド（公開 11 / 非公開 12 / アナウンス 10）か。
func isThreadChannel(kind int) bool { return kind >= 10 && kind <= 12 }

// discordError は Discord が 2xx 以外を返したときのエラー。
type discordError struct {
	Status  int
	Code    int
	Message string
}

func (e *discordError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Discord がエラーを返した: %d (code %d: %s)", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("Discord がエラーを返した: %d", e.Status)
}

// api は Bot トークンで Discord の API を叩く。out が非 nil なら応答を JSON として読む。
// 429（レート制限）は Retry-After を待って 1 度だけやり直す。
func api(method, path string, payload, out any) error {
	return apiWithRetry(method, path, payload, out, true)
}

func apiWithRetry(method, path string, payload, out any, mayRetry bool) error {
	var reader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, discordAPIBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+BotToken())
	req.Header.Set("User-Agent", UserAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("Discord への送信に失敗: %w", err)
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
		return apiWithRetry(method, path, payload, out, false)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		de := &discordError{Status: resp.StatusCode}
		var body struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) == nil {
			de.Code, de.Message = body.Code, body.Message
		}
		return de
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("Discord の応答を読めません: %w", err)
	}
	return nil
}

// ChannelType は channelID の種別。
func ChannelType(channelID string) (int, error) {
	var channel struct {
		Type int `json:"type"`
	}
	if err := api(http.MethodGet, "/channels/"+channelID, nil, &channel); err != nil {
		return 0, fmt.Errorf("チャンネル %s を読めません: %w", channelID, err)
	}
	return channel.Type, nil
}

// createThread は channelID に投稿（スレッド）を作り、その ID を返す。
//
// テキスト/アナウンスは中身の無いスレッドを作る（type 11）ので、本文は呼び出し側が
// 後から送る（posted は false）。フォーラム/メディアは最初のメッセージが必須なので
// first を入れて作る（posted は true ＝ 1 ページ目は送信済み）。
func createThread(channelID string, kind int, name, first string) (threadID string, posted bool, err error) {
	payload := map[string]any{
		"name":                  name,
		"auto_archive_duration": threadAutoArchiveMinutes,
	}
	switch kind {
	case channelForum, channelMedia:
		payload["message"] = map[string]any{"content": first}
		posted = true
	default:
		payload["type"] = 11 // 公開スレッド
	}

	var thread struct {
		ID string `json:"id"`
	}
	if err := api(http.MethodPost, "/channels/"+channelID+"/threads", payload, &thread); err != nil {
		return "", false, fmt.Errorf("スレッド「%s」を作れません: %w", name, err)
	}
	if thread.ID == "" {
		return "", false, fmt.Errorf("スレッド「%s」の応答に ID がありません", name)
	}
	return thread.ID, posted, nil
}

// PostMessage は channelID（スレッドでもよい）にメッセージを 1 通送る。
func PostMessage(channelID, content string) error {
	return api(http.MethodPost, "/channels/"+channelID+"/messages",
		map[string]any{"content": content}, nil)
}

// PostThread は channelID に新しいスレッドを作り、本文をその中に送る。
// 返すのは投稿先のスレッド ID——続きを PostMessage で足せる。
//
// 本文が 1 通の上限を超えるときは分割して連投する（すべて同じスレッド）。
// 送り先が既にスレッドなら、新しく作らずそこへ追記する（その ID をそのまま返す）。
func PostThread(channelID, title, body string) (threadID string, err error) {
	pages := pagesOf(body)
	if len(pages) == 0 {
		return "", fmt.Errorf("本文が空です")
	}

	kind, err := ChannelType(channelID)
	if err != nil {
		return "", err
	}

	// 呼びかけは 1 通目だけ。ページごとに付けると 1 投稿で何度も通知が飛ぶ
	pages[0] = mentionPrefix() + pages[0]

	target := channelID
	start := 0
	if !isThreadChannel(kind) {
		id, posted, err := createThread(channelID, kind, threadName(title), pages[0])
		if err != nil {
			return "", err
		}
		target = id
		if posted {
			start = 1 // フォーラムは 1 ページ目をスレッド作成で送っている
		}
	}

	for i := start; i < len(pages); i++ {
		if err := PostMessage(target, pages[i]); err != nil {
			return target, fmt.Errorf("%d/%d ページ目を送れません: %w", i+1, len(pages), err)
		}
		if i < len(pages)-1 {
			time.Sleep(betweenPages)
		}
	}
	return target, nil
}
