package notify

import (
	"fmt"
	"strings"
)

// threadNameLimit は Discord のスレッド名の上限。
const threadNameLimit = 100

// ChunkLimit は 1 通の文字数の上限。
//
// Discord の 1 通の上限は 2000 文字。ページ番号（"_(2/3)_"）に使う余白を
// 引いてある。超えると 400 が返り、レポートが丸ごと消える。
const ChunkLimit = 1900

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

// pagesOf は本文を上限で切り、2 通以上になるならページ番号を付ける。
// 空の本文は 0 通。
func pagesOf(body string) []string {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	pages := Chunks(body, ChunkLimit)
	if len(pages) == 1 {
		return pages
	}
	out := make([]string, len(pages))
	for i, page := range pages {
		out[i] = fmt.Sprintf("%s\n_(%d/%d)_", page, i+1, len(pages))
	}
	return out
}

// PostDocument は本文を日次レポートのチャンネルに送る（レポートの配達係）。
//
// Alert との違いは 2 つ。[wbjp] の接頭辞を付けないこと（レポートは自分で
// 見出しを持っている）と、送り先が ReportChannelID であること。
// 送り先が未設定なら何もせずエラーを返す——レポート本体はファイルに
// 残っているので、配達の失敗で処理は止めない。
//
// 呼ぶたびに新しいスレッドを作り、その中に本文を入れる。スレッド名は title、
// 無ければ本文の 1 行目。
func PostDocument(body, title string) (ok bool, err error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return false, fmt.Errorf("本文が空です")
	}
	channelID := ReportChannelID()
	if reason := missingConfig(channelID, ReportChannelEnvVar); reason != "" {
		return false, fmt.Errorf("送り先が未設定です（%s）", reason)
	}
	if title != "" {
		body = fmt.Sprintf("**%s**\n%s", title, body)
	} else {
		// 見出しが無ければ本文の 1 行目をスレッド名にする（"wbjp" だけの名前で並ばないように）
		title = firstLine(body)
	}

	if err := PostThread(channelID, title, body); err != nil {
		return false, err
	}
	return true, nil
}

// firstLine は本文の最初の空でない行から Markdown の飾り（** / # / 先頭の - ）を外したもの。
func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "#-* ")
		line = strings.ReplaceAll(line, "**", "")
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// threadName はスレッド名を作る。空・100 文字超えを避ける。
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
