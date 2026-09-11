package rate

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TradersURL は月別の一覧。/premium/rating/YYYYMM で 2005 年 1 月まで遡れる。
const TradersURL = "https://www.traders.co.jp/premium/rating/"

// TradersEntry は月別一覧の 1 行。
//
// grail の Entry と分けているのは、こちらが銘柄コード・市場を持ち、
// 「格上げ/格下げ」の語を持たない（"Buy→Hold" のように前後だけ）ため。
type TradersEntry struct {
	PubDate string // YYYY-MM-DD
	Code    string
	Name    string
	Market  string // 東P / 東1 など
	Firm    string
	Rating  string // 原文（"Buy→Hold" "新規Buy2" "1継続"）
	Target  string // 原文（"3600→2600円" "1500円" "-"）

	RatingFrom string
	RatingTo   string
	TargetFrom float64
	TargetTo   float64
}

var (
	tradersRowRe  = regexp.MustCompile(`(?is)<tr class="rating_row[^"]*">(.*?)</tr>`)
	tradersCellRe = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	tradersDateRe = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})$`)
	tradersNumRe  = regexp.MustCompile(`[0-9][0-9,.]*`)
	tradersNameRe = regexp.MustCompile(`^(.*?)\s*\(([0-9A-Z]{4})/([^)]*)\)$`)
)

// ParseTraders は月別一覧の HTML から行を取り出す。yearMonth は "200501" の形。
func ParseTraders(html, yearMonth string) ([]TradersEntry, error) {
	if len(yearMonth) != 6 {
		return nil, fmt.Errorf("年月は YYYYMM で渡してください: %q", yearMonth)
	}
	year, err := strconv.Atoi(yearMonth[:4])
	if err != nil {
		return nil, err
	}
	month, err := strconv.Atoi(yearMonth[4:])
	if err != nil {
		return nil, err
	}

	var entries []TradersEntry
	for _, row := range tradersRowRe.FindAllStringSubmatch(html, -1) {
		cells := tradersCellRe.FindAllStringSubmatch(row[1], -1)
		if len(cells) != 5 {
			continue
		}
		var text [5]string
		for i, c := range cells {
			text[i] = cleanCell(c[1])
		}
		m := tradersDateRe.FindStringSubmatch(text[0])
		if m == nil {
			continue
		}
		day, _ := strconv.Atoi(m[2])
		rowMonth, _ := strconv.Atoi(m[1])
		// 月をまたぐ行は無いはずだが、あればページの年月を優先せず日付側に合わせる
		pub := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		if rowMonth != month {
			pub = time.Date(year, time.Month(rowMonth), day, 0, 0, 0, 0, time.UTC)
		}

		e := TradersEntry{PubDate: pub.Format("2006-01-02"), Firm: text[2], Rating: text[3], Target: text[4]}
		if nm := tradersNameRe.FindStringSubmatch(text[1]); nm != nil {
			e.Name, e.Code, e.Market = strings.TrimSpace(nm[1]), nm[2], nm[3]
		} else {
			e.Name = text[1]
		}
		if e.Code == "" {
			continue // コードが取れない行は使えない
		}
		e.RatingFrom, e.RatingTo = splitTradersRating(e.Rating)
		e.TargetFrom, e.TargetTo = splitTradersTarget(e.Target)
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s: レーティングの行が 1 つも取れません", yearMonth)
	}
	return entries, nil
}

// splitTradersRating は "Buy→Hold" "新規Buy2" "1継続" を前後に割る。
//
// grail と違って「格上げ」「格下げ」の語が無いので、ここでは方向を決めない。
// 上下は証券会社ごとの序列が要るので RatingDirection で別に出す。
func splitTradersRating(s string) (from, to string) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "新規"):
		return "", strings.TrimPrefix(s, "新規")
	case strings.HasSuffix(s, "継続"):
		r := strings.TrimSuffix(s, "継続")
		return r, r
	}
	if f, t, ok := strings.Cut(s, "→"); ok {
		return strings.TrimSpace(f), strings.TrimSpace(t)
	}
	return "", s
}

// splitTradersTarget は "3600→2600円" "1500円" "490円継続" "-" から前後の目標株価を取る。
//
// grail は "9300円→9500円" と両方に「円」が付くが、こちらは末尾に 1 つだけ。
// grail 用の splitTarget を使うと前半を取りこぼす。
func splitTradersTarget(s string) (from, to float64) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" || s == "－" {
		return 0, 0
	}
	num := func(v string) float64 {
		v = tradersNumRe.FindString(v)
		if v == "" {
			return 0
		}
		f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
		if err != nil {
			return 0
		}
		return f
	}
	if f, t, ok := strings.Cut(s, "→"); ok {
		return num(f), num(t)
	}
	v := num(s)
	return v, v
}

// SaveTraders は月別一覧の行を書き込み、初めて入った行数を返す。
func (s *Store) SaveTraders(ctx context.Context, yearMonth string, entries []TradersEntry) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO traders_ratings (pub_date, code, name, market, firm, rating, target,
                             rating_from, rating_to, target_from, target_to, direction)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (pub_date, code, firm, rating, target) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()

	added := 0
	for _, e := range entries {
		res, err := stmt.ExecContext(ctx, e.PubDate, e.Code, e.Name, e.Market, e.Firm, e.Rating, e.Target,
			e.RatingFrom, e.RatingTo, e.TargetFrom, e.TargetTo, RatingDirection(e.RatingFrom, e.RatingTo))
		if err != nil {
			return 0, fmt.Errorf("%s %s %s の書き込みに失敗: %w", e.PubDate, e.Code, e.Firm, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO traders_months (year_month, row_count, imported_at) VALUES (?,?,?)`,
		yearMonth, len(entries), time.Now().Format(time.RFC3339)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// RecomputeDirections は traders_ratings の direction を計算し直す。
// 序列表（ratingWordRank）を直したときに全行へ反映するために使う。
func (s *Store) RecomputeDirections(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, rating_from, rating_to FROM traders_ratings`)
	if err != nil {
		return 0, err
	}
	type update struct {
		rowID     int64
		direction string
	}
	var updates []update
	for rows.Next() {
		var id int64
		var from, to string
		if err := rows.Scan(&id, &from, &to); err != nil {
			_ = rows.Close()
			return 0, err
		}
		updates = append(updates, update{id, RatingDirection(from, to)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `UPDATE traders_ratings SET direction = ? WHERE rowid = ?`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()
	for _, u := range updates {
		if _, err := stmt.ExecContext(ctx, u.direction, u.rowID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(updates), nil
}

// ImportedMonths は既に取り込んだ年月。
func (s *Store) ImportedMonths(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT year_month FROM traders_months`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	done := map[string]bool{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		done[m] = true
	}
	return done, rows.Err()
}
