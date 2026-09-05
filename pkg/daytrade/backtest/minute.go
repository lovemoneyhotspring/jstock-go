package backtest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/archsql"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// MinuteBars は分足から引いた「その時刻に成行を出したらいくらで約定したか」と
// 「その日いつ寄ったか」。
//
// 日足の検証（寄付で建てて引けで手仕舞う）と実運用（9:01 の成行・15:20 の成行）の
// ずれを埋める。**「T に成行を出した」は T 以降の最初の足の始値**で近似する——板が
// 無いのでスプレッドの半分は見ていない（研究ノート 2026-09-jp-gap-minute の「割り引くべき点」）。
//
// 分足の履歴は 2 年しか無い。**分足の無い日は日足の寄付・引けにそのまま落とす**ので、
// 10 年の検証はこれを渡しても止まらない。どこからが分足かは Describe が返す。
type MinuteBars struct {
	// entryAt / exitAt は約定値を取る時刻（"09:01"）。空なら日足の寄付・引け。
	entryAt, exitAt string
	// days は分足のある日（"2006-01-02"）。
	days map[string]bool
	// rows は "日付|銘柄" → その日の値。
	rows map[string]minuteRow
	// first / last は分足のある期間。
	first, last string
}

type minuteRow struct {
	// entry / exit は entryAt / exitAt 以降の最初の約定値（無ければ 0）。
	entry, exit float64
	// firstAt はその日の最初の約定の時刻（"09:00" なら板寄せで寄った）。
	firstAt string
}

// LoadMinuteBars は期間の分足を読む。entryAt / exitAt は "HH:MM"（JST）で、
// 空なら日足の寄付・引けを使う（Opened の判定だけに使うときは両方空でよい）。
//
// codes を渡すと読み出しをその銘柄に絞る（パネルの候補だけを引くため。nil なら全銘柄）。
func LoadMinuteBars(arch *archive.Archive, start, end time.Time, entryAt, exitAt string, codes []string) (*MinuteBars, error) {
	if err := validTime(entryAt, "建値の時刻"); err != nil {
		return nil, err
	}
	if err := validTime(exitAt, "手仕舞いの時刻"); err != nil {
		return nil, err
	}
	source, ok := archsql.Source(arch, universe.EPMinute, start, end)
	if !ok {
		return nil, fmt.Errorf("分足がありません（%s〜%s）。jquants backfill --only %s を先に",
			start.Format(archsql.DateLayout), end.Format(archsql.DateLayout), universe.EPMinute.Name())
	}
	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	entryExpr, exitExpr := "NULL::DOUBLE", "NULL::DOUBLE"
	if entryAt != "" {
		entryExpr = fmt.Sprintf(`arg_min("O", "Time") FILTER (WHERE "Time" >= '%s')`, entryAt)
	}
	if exitAt != "" {
		exitExpr = fmt.Sprintf(`arg_min("O", "Time") FILTER (WHERE "Time" >= '%s')`, exitAt)
	}
	where := fmt.Sprintf(`"Date" >= %s AND "Date" <= %s`, archsql.Lit(start), archsql.Lit(end))
	if len(codes) > 0 {
		where += fmt.Sprintf(` AND CAST("Code" AS VARCHAR) IN (%s)`, quoteCodes(codes))
	}
	query := fmt.Sprintf(`
SELECT "Date" AS d, CAST("Code" AS VARCHAR) AS code,
       min("Time") AS first_at, %s AS entry, %s AS exit
FROM %s
WHERE %s
GROUP BY 1, 2`, entryExpr, exitExpr, source, where)

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("分足の読み出しに失敗しました: %w", err)
	}
	defer rows.Close()

	out := &MinuteBars{
		entryAt: entryAt, exitAt: exitAt,
		days: map[string]bool{}, rows: map[string]minuteRow{},
	}
	for rows.Next() {
		var (
			day         time.Time
			code        string
			firstAt     string
			entry, exit sqlFloat
		)
		if err := rows.Scan(&day, &code, &firstAt, &entry, &exit); err != nil {
			return nil, err
		}
		key := day.UTC().Format(dayLayout)
		out.days[key] = true
		out.rows[key+"|"+code] = minuteRow{entry: entry.value(), exit: exit.value(), firstAt: firstAt}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	days := make([]string, 0, len(out.days))
	for day := range out.days {
		days = append(days, day)
	}
	sort.Strings(days)
	if len(days) > 0 {
		out.first, out.last = days[0], days[len(days)-1]
	}
	return out, nil
}

// Fill は建値と手仕舞い値（FillModel）。
//
//   - 分足の無い日は日足の寄付・引け（10 年の検証を止めないため）
//   - 分足のある日にその銘柄の足が 1 本も無ければ建てない（その日 1 度も約定していない）
//   - 手仕舞いの時刻以降に約定が無ければ引け（クロージング・オークションで約定した扱い）
func (m *MinuteBars) Fill(r Row) (float64, float64, bool) {
	entry, exit := r.Open, r.Close
	if m.covers(r.Date) {
		row, ok := m.rows[m.key(r.Date, r.Code)]
		if !ok {
			return 0, 0, false
		}
		if m.entryAt != "" {
			if row.entry <= 0 {
				return 0, 0, false // その時刻以降に約定が無い＝建てられない
			}
			entry = row.entry
		}
		if m.exitAt != "" && row.exit > 0 {
			exit = row.exit
		}
	}
	return entry, exit, entry > 0 && exit > 0
}

// Opened は 09:00 の板寄せで寄ったか。known が偽なら分足が無く判定できない。
//
// 実運用の signal.skip_opened（9:01 の時点で既に寄っている銘柄を候補から外す）を
// 検証で再現するための材料。特別気配で寄りが遅れる銘柄が利益源なので、
// ここで分かれる群の差は大きい（研究ノート 2026-09-jp-gap-minute の発見 1）。
func (m *MinuteBars) Opened(day time.Time, code string) (opened, known bool) {
	if !m.covers(day) {
		return false, false
	}
	row, ok := m.rows[m.key(day, code)]
	if !ok {
		return false, true // 1 度も約定していない＝寄っていない
	}
	return row.firstAt <= openingAuction, true
}

// Days は分足のある日数。
func (m *MinuteBars) Days() int { return len(m.days) }

// Describe は約定モデルと分足の期間を 1 行で。検証の出力に載せる
// （どの期間が分足で、どこからが日足の近似かを混同しないため）。
func (m *MinuteBars) Describe() string {
	entry, exit := "日足の寄付", "日足の引け"
	if m.entryAt != "" {
		entry = m.entryAt + " 以降の最初の約定"
	}
	if m.exitAt != "" {
		exit = m.exitAt + " 以降の最初の約定"
	}
	if len(m.days) == 0 {
		return fmt.Sprintf("分足: 該当なし（建値 %s / 手仕舞い %s）", entry, exit)
	}
	return fmt.Sprintf("分足: %s〜%s の %d 日（建値 %s / 手仕舞い %s。無い日は日足の寄付・引け）",
		m.first, m.last, len(m.days), entry, exit)
}

func (m *MinuteBars) covers(day time.Time) bool { return m.days[day.Format(dayLayout)] }

func (m *MinuteBars) key(day time.Time, code string) string {
	return day.Format(dayLayout) + "|" + code
}

// openingAuction は寄付の板寄せの足（この時刻に約定していれば 09:00 に寄った）。
const openingAuction = "09:00"

// PanelCodes はパネルに出てくる銘柄コード（分足の読み出しを絞るのに使う）。
func PanelCodes(panel *Panel) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4096)
	for _, r := range panel.Rows {
		if !seen[r.Code] {
			seen[r.Code] = true
			out = append(out, r.Code)
		}
	}
	sort.Strings(out)
	return out
}

func quoteCodes(codes []string) string {
	quoted := make([]string, 0, len(codes))
	for _, c := range codes {
		quoted = append(quoted, "'"+strings.ReplaceAll(c, "'", "''")+"'")
	}
	return strings.Join(quoted, ", ")
}

// validTime は "HH:MM"（空は「日足を使う」）。SQL に埋めるので書式を確かめてから通す。
func validTime(value, label string) error {
	if value == "" {
		return nil
	}
	if _, err := time.Parse("15:04", value); err != nil {
		return fmt.Errorf("%s の書式が違います: %q（例 09:01）", label, value)
	}
	return nil
}

// sqlFloat は NULL を 0 として読む（分足に該当の時刻が無い銘柄日）。
type sqlFloat struct{ v *float64 }

func (f *sqlFloat) Scan(src any) error {
	f.v = nil
	switch x := src.(type) {
	case nil:
		return nil
	case float64:
		f.v = &x
	case float32:
		v := float64(x)
		f.v = &v
	case int64:
		v := float64(x)
		f.v = &v
	default:
		return fmt.Errorf("分足の値を読めません: %T", src)
	}
	return nil
}

func (f sqlFloat) value() float64 {
	if f.v == nil {
		return 0
	}
	return *f.v
}
