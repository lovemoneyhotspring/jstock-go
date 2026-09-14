package news

import "strings"

// ListFilter は `news list` の絞り込み。空の項目は絞らない。
type ListFilter struct {
	Code  string // 銘柄コード
	Genre string // ジャンル
	Day   string // 配信日（YYYY-MM-DD）
	Word  string // 見出しか本文に含まれる語
	Limit int    // 件数。0 以下で無制限
}

// ListQuery は記事を新しい順に引く SQL と引数を組む。
// 列は feed_date, time, genres, codes, headline, body の順。値はすべてプレースホルダで渡す。
func ListQuery(f ListFilter) (string, []any) {
	where := []string{"1=1"}
	var params []any
	from := "news n"
	if f.Code != "" {
		from += " JOIN news_codes c ON c.feed_date = n.feed_date AND c.news_id = n.news_id"
		where = append(where, "c.code = ?")
		params = append(params, f.Code)
	}
	if f.Genre != "" {
		from += " JOIN news_genres g ON g.feed_date = n.feed_date AND g.news_id = n.news_id"
		where = append(where, "g.genre = ?")
		params = append(params, f.Genre)
	}
	if f.Day != "" {
		where = append(where, "n.feed_date = ?")
		params = append(params, f.Day)
	}
	if f.Word != "" {
		where = append(where, "(n.headline LIKE ? OR n.body LIKE ?)")
		params = append(params, "%"+f.Word+"%", "%"+f.Word+"%")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = -1 // SQLite の LIMIT -1 は無制限
	}
	params = append(params, limit)
	query := `SELECT n.feed_date, n.time, n.genres, n.codes, n.headline, n.body
FROM ` + from + ` WHERE ` + strings.Join(where, " AND ") + `
ORDER BY n.feed_date DESC, n.time DESC LIMIT ?`
	return query, params
}
