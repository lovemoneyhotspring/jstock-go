package news

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"golang.org/x/text/unicode/norm"
)

// 価格の行き先が決まる材料（TOB・MBO など）を TDnet 適時開示の見出しから判定する。
// daytrade はこれを読んで、張り付き（買付価格までサヤ寄せしてストップ高）の銘柄を
// ショートの候補から外す。計画は ~/obsidian-vault/30-projects/daytrade-short-corp-events.md。

// GenreTDnet は TDnet 適時開示のジャンル。
const GenreTDnet = "62199"

// 材料の種類。Excludes が真の種類は、価格の行き先が決まるのでショートから外す。
const (
	KindTOBTarget     = "tob_target"     // TOB の対象（開始・賛同・結果・条件の変更を含む）
	KindMBO           = "mbo"            // MBO
	KindSqueezeOut    = "squeeze_out"    // 株式併合・株式等売渡請求（TOB 後の締め出し）
	KindShareExchange = "share_exchange" // 株式交換で完全子会社になる側
	KindDelisting     = "delisting"      // 上場廃止
	KindSelfTender    = "self_tender"    // 自己株式の公開買付（価格の上限は決まらない。記録だけ）
	KindAccumulation  = "accumulation"   // 公開買付に準ずる買集め（記録だけ）
	KindPressReport   = "press_report"   // 一部報道への回答（TOB の前触れのことがある。記録だけ）
)

// Excludes はその種類でショートから外すか。
func Excludes(kind string) bool {
	switch kind {
	case KindTOBTarget, KindMBO, KindSqueezeOut, KindShareExchange, KindDelisting:
		return true
	}
	return false
}

// Event は材料の開示 1 件を銘柄ごとに展開したもの。
type Event struct {
	Code     string
	Kind     string
	FeedDate string
	// Time は配信時刻 HHMM、SavedAt は記録簿に初めて入った時刻（いつ知れたか）。
	Time     string
	SavedAt  time.Time
	Headline string
}

var (
	// 対象会社が自社について書く形。「当社株券等に対する公開買付」「当社株式に係る株式売渡請求」
	selfTarget = regexp.MustCompile(`当(社|行)の?(普通)?(株券|株式|投資口|新株予約権)(等)?[^に]{0,16}に(対|係|関)する(公開買付|株式(等)?売渡請求)`)
	// 買付者側の開示は対象を「（証券コード：8848）」で書く。記事の銘柄は買付者なので外さない
	securityCode = regexp.MustCompile(`証券コード[:\s]*([0-9][0-9A-Z]{3})`)
	selfTender   = regexp.MustCompile(`自己株式(等)?の公開買付`)
	selfSubsid   = regexp.MustCompile(`当(社|行)を[^、。]{0,30}完全子会社`)
	// 対象会社にしか出せない開示。意見表明・応募の推奨は対象の取締役会が出し、
	// 「親会社の異動」は買われた側に起きる（買付者側は「子会社の異動」）
	targetOnly = regexp.MustCompile(`意見表明|応募(を)?推奨|考え方|親会社`)
	// 親会社が出す「上場廃止となった子会社（…）に関するお知らせ」「合併により上場廃止となった〇〇」。
	// 廃止になったのは他社で、記事の銘柄は廃止にならない
	subsidDelisted = regexp.MustCompile(`上場廃止となった|子会社[^、。]{0,20}の上場廃止`)
)

// Classify は見出しから材料の種類と対象の銘柄を返す。codes は記事の銘柄（電文の p_ISL）。
// 当たらなければ kind は空。全角・半角の揺れ（ＭＢＯ / MBO）は NFKC で揃え、空白を除いてから見る
// （「当社株式に対する 公開買付け」のように語の間に空白が入る見出しがある）。
func Classify(headline string, codes []string) (kind string, targets []string) {
	h := strings.Join(strings.Fields(norm.NFKC.String(headline)), "")
	self := selfTarget.MatchString(h)
	var quoted []string
	for _, m := range securityCode.FindAllStringSubmatch(h, -1) {
		quoted = appendUnique(quoted, m[1])
	}
	// 対象: 自社の書き方なら記事の銘柄、証券コードが書いてあればそのコード（買付者は外さない）
	pick := func() []string {
		if self || len(quoted) == 0 {
			return codes
		}
		return quoted
	}
	switch {
	case strings.Contains(h, "買集め"):
		return KindAccumulation, pick()
	case selfTender.MatchString(h):
		return KindSelfTender, pick()
	case strings.Contains(h, "MBO"):
		return KindMBO, pick()
	case self && strings.Contains(h, "公開買付"):
		return KindTOBTarget, codes
	case strings.Contains(h, "公開買付") && len(quoted) > 0:
		return KindTOBTarget, quoted
	case strings.Contains(h, "公開買付") && targetOnly.MatchString(h):
		return KindTOBTarget, codes
	case strings.Contains(h, "売渡請求") || strings.Contains(h, "株式併合"):
		return KindSqueezeOut, pick()
	case strings.Contains(h, "株式交換") || strings.Contains(h, "完全子会社"):
		// 買収側の「簡易株式交換による完全子会社化」は外さない。自社が子会社になる形か、
		// 対象の証券コードがあるか、上場廃止を伴うものだけ
		if selfSubsid.MatchString(h) || strings.Contains(h, "上場廃止") {
			return KindShareExchange, pick()
		}
		if len(quoted) > 0 {
			return KindShareExchange, quoted
		}
		return "", nil
	case strings.Contains(h, "上場廃止") && !subsidDelisted.MatchString(h):
		return KindDelisting, pick()
	case strings.Contains(h, "報道"):
		return KindPressReport, codes
	}
	return "", nil
}

func appendUnique(xs []string, x string) []string {
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

// CorporateEvents は配信日 from〜to（YYYY-MM-DD、両端を含む）の適時開示から材料を判定し、
// 銘柄 → 開示（古い順）を返す。knownAt より後に記録簿へ入った記事は使わない（その時点で
// 知り得なかった材料で判定しない）。記録だけの種類も含むので、外すかは Excludes で見る。
func (s *Store) CorporateEvents(ctx context.Context, from, to string, knownAt time.Time) (map[string][]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT n.feed_date, n.time, n.codes, n.headline, n.saved_at
        FROM news n
        JOIN news_genres g ON g.feed_date = n.feed_date AND g.news_id = n.news_id
        WHERE g.genre = ? AND n.feed_date BETWEEN ? AND ?
        ORDER BY n.feed_date, n.time, n.news_id`, GenreTDnet, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]Event{}
	for rows.Next() {
		var feedDate, hhmm, codes, headline, savedAt string
		if err := rows.Scan(&feedDate, &hhmm, &codes, &headline, &savedAt); err != nil {
			return nil, err
		}
		saved, err := time.Parse(time.RFC3339, savedAt)
		if err != nil {
			return nil, fmt.Errorf("saved_at を読めません（%s %s）: %w", feedDate, savedAt, err)
		}
		if saved.After(knownAt) {
			continue
		}
		kind, targets := Classify(headline, splitList(codes))
		if kind == "" {
			continue
		}
		for _, code := range targets {
			out[code] = append(out[code], Event{
				Code: code, Kind: kind, FeedDate: feedDate, Time: hhmm, SavedAt: saved, Headline: headline,
			})
		}
	}
	return out, rows.Err()
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "|")
}

// FreshDays は鮮度を見る日数（今日を含む暦日）。土日と祝日をはさんでも、前の営業日の
// 夜の配信まで入る。朝の news sync（--days 5）が取り直す範囲と揃えてある。
const FreshDays = 5

// Fresh は記録簿がいつの時点まで揃っているか。**必要な日（今日から days 日さかのぼる）ごとに見る。**
//
//   - 今日: 最後に取れた（ok の）時刻
//   - 過去の日: 日が明けてから取れていれば揃っている（制約にならない）。明ける前にしか
//     取れていなければその時刻（その日の夜の配信が抜けている）
//   - 取れていない日（失敗・未取得）: ゼロ値
//
// のうち最も古いものを at に、それを決めた日を day に返す。以前は全日の最大値を見ていたので、
// 前日だけ取り込みに失敗した朝も「新しい」と判定していた（2026-09-24 のレビュー）。
// 失敗の記録（RecordFailure）は fetched_at を上書きするので、status = 'ok' の行だけを見る。
// 時刻は書いた側の時間帯のまま入っている（UTC / JST）ので、文字列ではなく読んでから比べる。
func (s *Store) Fresh(ctx context.Context, now time.Time, days int) (at time.Time, day string, err error) {
	if days < 1 {
		days = 1
	}
	today := now.In(clock.Tokyo)
	from := today.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	to := today.Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx,
		`SELECT feed_date, fetched_at FROM news_days WHERE status = 'ok' AND feed_date BETWEEN ? AND ?`, from, to)
	if err != nil {
		return time.Time{}, "", err
	}
	defer func() { _ = rows.Close() }()
	fetched := map[string]time.Time{}
	for rows.Next() {
		var d, v string
		if err := rows.Scan(&d, &v); err != nil {
			return time.Time{}, "", err
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("fetched_at を読めません（%s）: %w", v, err)
		}
		fetched[d] = t
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, "", err
	}
	first := true
	for i := days - 1; i >= 0; i-- {
		d := today.AddDate(0, 0, -i).Format("2006-01-02")
		t, ok := fetched[d]
		if !ok {
			return time.Time{}, d, nil // 取れていない日がある
		}
		if d != to {
			end, err := dayEnd(d)
			if err != nil {
				return time.Time{}, "", err
			}
			if !t.Before(end) {
				continue // 日が明けてから取れている
			}
		}
		if first || t.Before(at) {
			at, day, first = t, d, false
		}
	}
	return at, day, nil
}
