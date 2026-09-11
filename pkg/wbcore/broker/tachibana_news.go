package broker

import (
	"encoding/base64"
	"net/url"
	"strings"

	"golang.org/x/text/encoding/japanese"
)

// clmGetNews はニュース問合。過去 90 日ぶんを日付指定で取れる。
// 当日ぶんは 06:00〜23:59 に毎分、過去ぶんは毎朝 05:40〜05:50 に発信元から取り込まれる。
const clmGetNews = "CLMMfdsGetNews"

// NewsItem は 1 本のニュース。
type NewsItem struct {
	ID string
	// Time は配信時刻 HHMM（発信元の値）
	Time string
	// Categories / Genres は発信元の分類。レーティングは Genres で選ぶ
	Categories []string
	Genres     []string
	// Codes は関連銘柄コード。レーティングのまとめ記事では、
	// その記事で取り上げられた銘柄が全部入る
	Codes    []string
	Headline string
	Body     string
}

// ニュースのジャンル（発信元は QUICK）。レーティングに関わるものだけ名前を付ける。
// 【株価レーティング】【目標株価】のまとめは、前営業日ぶんを翌営業日の 07:10 に配信する。
const (
	GenreRatingUp     = "60220" // 株価レーティング 引き上げ
	GenreRatingDown   = "60221" // 株価レーティング 引き下げ
	GenreRatingNew    = "60222" // 株価レーティング 新規設定
	GenreTargetUp     = "60230" // 目標株価 引き上げ
	GenreTargetDown   = "60231" // 目標株価 引き下げ
	GenreTargetNew    = "60232" // 目標株価 新規設定
	GenreQuickConsens = "6521"  // QUICK 株価レーティング更新銘柄一覧（コンセンサス平均、社名なし）
)

// News は指定日（YYYYMMDD）のニュースを取る。
//
// ニュースは master 口（sUrlMaster）でしか受け付けられない。price 口は「引数エラー」を返す。
func (t *TachibanaBroker) News(day string) ([]NewsItem, error) {
	res, err := t.postTo(interfaceMaster, clmGetNews, map[string]any{"p_DT": day})
	if err != nil {
		return nil, err
	}
	raw, _ := res["aCLMMfdsNews"].([]any)
	items := make([]NewsItem, 0, len(raw))
	for _, entry := range raw {
		row, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		items = append(items, NewsItem{
			ID:         text(row["p_ID"]),
			Time:       text(row["p_TM"]),
			Categories: splitList(text(row["p_CGL"])),
			Genres:     splitList(text(row["p_GNL"])),
			Codes:      splitList(text(row["p_ISL"])),
			Headline:   DecodeNewsText(text(row["p_HDL"])),
			Body:       DecodeNewsText(text(row["p_TX"])),
		})
	}
	return items, nil
}

// splitList は「|」区切りの項目を配列にする。空なら nil。
func splitList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DecodeNewsText は見出し・本文を戻す。
// BASE64 を解くと %XX の URL エンコードが出てきて、その中身が Shift_JIS。
// 順序を逆にすると（先に Shift_JIS として読むと）壊れる。
func DecodeNewsText(s string) string {
	if s == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s
	}
	unescaped := string(raw)
	if u, err := url.QueryUnescape(unescaped); err == nil {
		unescaped = u
	}
	decoded, err := japanese.ShiftJIS.NewDecoder().String(unescaped)
	if err != nil {
		return unescaped
	}
	return decoded
}

// HasGenre はそのニュースが指定ジャンルかどうか。
func (n NewsItem) HasGenre(genre string) bool {
	for _, g := range n.Genres {
		if g == genre {
			return true
		}
	}
	return false
}

// PubDate は見出しの「（9/9）」から発表日の月日を取る。
// まとめ記事は前営業日ぶんなので、配信日とはずれる。
func (n NewsItem) PubDateLabel() string {
	open := strings.Index(n.Headline, "（")
	if open < 0 {
		return ""
	}
	rest := n.Headline[open+len("（"):]
	close := strings.Index(rest, "）")
	if close < 0 {
		return ""
	}
	label := rest[:close]
	if !strings.Contains(label, "/") {
		return ""
	}
	return label
}
