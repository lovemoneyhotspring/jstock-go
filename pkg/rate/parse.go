// Package rate はグレイル NET のレーティング一覧（証券会社の投資判断・目標株価）を
// 取り出して溜める。ページには「いつ載ったか」の時刻が無いので、
// 短い間隔で取りに行き、行を初めて見た時刻（first_seen_at）を時刻の代わりに使う。
package rate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Entry はレーティング一覧の 1 行。
type Entry struct {
	// PubDate は一覧に載っている日付（ページは MM/DD しか持たないので年は補う）
	PubDate string
	Code    string
	Name    string
	Firm    string
	// Rating は「買い継続」「2→3格下げ」など原文のまま
	Rating string
	// Target は「9300円→9500円」など原文のまま
	Target string

	// 以下は Rating / Target から機械的に割ったもの（読めなければ空・0）
	Action     string // new / up / down / keep
	RatingFrom string
	RatingTo   string
	TargetFrom float64
	TargetTo   float64
}

// Key は同じ行かどうかを決める鍵。日付・銘柄・証券会社まで同じで内容が違う行は
// 訂正されたものとして別の行に数える（初出時刻を訂正で塗り替えないため）。
func (e Entry) Key() string {
	return strings.Join([]string{e.PubDate, e.Code, e.Firm, e.Rating, e.Target}, "\t")
}

var (
	rowRe  = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	cellRe = regexp.MustCompile(`(?is)<t[dh][^>]*>(.*?)</t[dh]>`)
	tagRe  = regexp.MustCompile(`(?is)<[^>]+>`)
	dateRe = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})$`)
	yenRe  = regexp.MustCompile(`([0-9][0-9,.]*)円`)
)

// Parse は一覧ページの HTML から行を取り出す。
// now は年の補い（ページに年が無い）と、翌年またぎの判定に使う。
func Parse(html string, now time.Time) ([]Entry, error) {
	var entries []Entry
	for _, row := range rowRe.FindAllStringSubmatch(html, -1) {
		cells := cellRe.FindAllStringSubmatch(row[1], -1)
		if len(cells) != 6 {
			continue
		}
		var text [6]string
		for i, c := range cells {
			text[i] = cleanCell(c[1])
		}
		m := dateRe.FindStringSubmatch(text[0])
		if m == nil {
			continue // 見出し行や広告行
		}
		pub, err := resolveYear(m[1], m[2], now)
		if err != nil {
			continue
		}
		e := Entry{
			PubDate: pub,
			Code:    text[1],
			Name:    text[2],
			Firm:    text[3],
			Rating:  text[4],
			Target:  text[5],
		}
		e.Action, e.RatingFrom, e.RatingTo = splitRating(e.Rating)
		e.TargetFrom, e.TargetTo = splitTarget(e.Target)
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("レーティングの行が 1 つも取れません（ページの作りが変わった可能性）")
	}
	return entries, nil
}

// cleanCell はセルの中身からタグと空白・実体参照を落とす。
func cleanCell(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	r := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "　", " ")
	return strings.TrimSpace(r.Replace(s))
}

// resolveYear は MM/DD に年を付ける。取得日より先の日付は前年のものとみなす
// （一覧は 1 か月ぶんしか無いので、年末年始だけの話）。
func resolveYear(mm, dd string, now time.Time) (string, error) {
	month, err := strconv.Atoi(mm)
	if err != nil {
		return "", err
	}
	day, err := strconv.Atoi(dd)
	if err != nil {
		return "", err
	}
	d := time.Date(now.Year(), time.Month(month), day, 0, 0, 0, 0, now.Location())
	if d.After(now.AddDate(0, 0, 1)) {
		d = d.AddDate(-1, 0, 0)
	}
	return d.Format("2006-01-02"), nil
}

// splitRating は「2→3格下げ」「新規OP」「買い継続」を動きと前後の判断に割る。
func splitRating(s string) (action, from, to string) {
	switch {
	case strings.HasPrefix(s, "新規"):
		return "new", "", strings.TrimPrefix(s, "新規")
	case strings.HasSuffix(s, "格上げ"), strings.HasSuffix(s, "格下げ"):
		action = "up"
		body := strings.TrimSuffix(s, "格上げ")
		if strings.HasSuffix(s, "格下げ") {
			action = "down"
			body = strings.TrimSuffix(s, "格下げ")
		}
		if f, t, ok := strings.Cut(body, "→"); ok {
			return action, strings.TrimSpace(f), strings.TrimSpace(t)
		}
		return action, "", strings.TrimSpace(body)
	case strings.HasSuffix(s, "継続"):
		r := strings.TrimSuffix(s, "継続")
		return "keep", r, r
	}
	return "", "", s
}

// splitTarget は「9300円→9500円」「3000円」から前後の目標株価を取る。
// 新規・据え置きで 1 つしか無いときは from = to とする。
func splitTarget(s string) (from, to float64) {
	m := yenRe.FindAllStringSubmatch(s, -1)
	if len(m) == 0 {
		return 0, 0
	}
	parse := func(v string) float64 {
		f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
		if err != nil {
			return 0
		}
		return f
	}
	if len(m) == 1 {
		v := parse(m[0][1])
		return v, v
	}
	return parse(m[0][1]), parse(m[len(m)-1][1])
}
