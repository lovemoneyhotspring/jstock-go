// Package history は追記専用の履歴（Parquet）。選定の候補と結果を、実行のたびに
// 1 ファイルずつ積む。
//
// 台帳（SQLite）は「今日もう買ったか」に答えるための現在の状態で、dry-run の記録は
// 確認のたびに消す。ログ（JSONL）は順位表の上位数件しか持たず、90 日で消える。
// どちらも「先週の 9:00 に何が候補で、何を選び、何を選ばなかったか」を後から
// 並べて分析する用途には向かない。ここはその用途だけのための置き場:
//
//   - 1 回の実行 = 1 ファイル。<root>/<kind>/<day>T<HHMMSS>Z-<run_id>.parquet。
//     同じ名前があれば枝番を付け、決して上書きしない
//   - 全ファイルに day（判定日）・run_id・recorded_at（UTC）の列が先頭に付く。
//     同じ日の複数回の実行（cron の再試行、dry-run の確認）はすべて残り、run_id で
//     区別できる。「その日の最終判断」は recorded_at が最大の行
//   - 読むときは期間で絞って縦に結合する。列が増えても古いファイルはそのまま読める
//     （DuckDB の union_by_name）
//
// ファイル名の先頭が判定日なので、期間の絞り込みはファイルを開かずに済む。
package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

// KeyColumns は全ファイルに付く鍵の列（この順で先頭に置く）。
var KeyColumns = []string{"day", "run_id", "recorded_at"}

// KindSummary は種類ごとの蓄積状況。
type KindSummary struct {
	Kind     string
	Files    int
	FirstDay *time.Time
	LastDay  *time.Time
}

// Store は 1 つの置き場（root）の下に種類（kind）ごとのディレクトリを持つ。
type Store struct {
	Root string
}

// NewStore は置き場を指す履歴ストアを作る。
func NewStore(root string) *Store { return &Store{Root: root} }

// Directory は種類ごとのディレクトリ。
func (s *Store) Directory(kind string) string {
	return filepath.Join(s.Root, kind)
}

// ---- 書く ----------------------------------------------------------------

// AppendOptions は Append の任意指定。
type AppendOptions struct {
	// RunID を省くと、いま束ねている CLI 実行の ID（ログと同じ）を使う。
	RunID string
	// At は記録時刻。ゼロ値なら現在（UTC）。
	At time.Time
	// Ctx は run_id を引くための実行コンテキスト（省略可）。
	Ctx context.Context
}

// Append は 1 回の実行の結果を 1 ファイルとして足す。既存のファイルには触れない。
//
// 0 行でも書く——「その日は条件に合う銘柄が無かった」も記録のうち。
func (s *Store) Append(kind string, frame Frame, day time.Time, opts AppendOptions) (string, error) {
	moment := opts.At
	if moment.IsZero() {
		moment = clock.NowUTC()
	}
	moment = moment.UTC().Truncate(time.Microsecond)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)

	runID := opts.RunID
	if runID == "" {
		runID = logging.CurrentRunID(opts.Ctx)
	}
	if runID == "" {
		runID = "manual"
	}

	keyed := withKeyColumns(frame, day, runID, moment)

	directory := s.Directory(kind)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("履歴の置き場を作れません %s: %w", directory, err)
	}
	path := freshPath(directory, day, moment, runID)

	// 途中で落ちた書きかけを読ませないため、別名で書いてから rename する
	tmp := path + ".tmp"
	if err := writeParquet(tmp, keyed); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("履歴の書き出しに失敗しました %s: %w", path, err)
	}
	return path, nil
}

// withKeyColumns は本体の列の前に day / run_id / recorded_at を差し込む。
// 呼び出し側が同名の列を持っていたら、こちらで上書きする（鍵の意味が揺れると
// 後から突き合わせられなくなるため）。
func withKeyColumns(frame Frame, day time.Time, runID string, moment time.Time) Frame {
	body := frame.Drop(KeyColumns...)
	columns := []Column{
		{Name: "day", Type: TypeDate},
		{Name: "run_id", Type: TypeString},
		{Name: "recorded_at", Type: TypeTimestamp},
	}
	columns = append(columns, body.Columns...)

	rows := make([]map[string]any, 0, len(body.Rows))
	for _, row := range body.Rows {
		copied := make(map[string]any, len(columns))
		for key, value := range row {
			copied[key] = value
		}
		copied["day"] = day
		copied["run_id"] = runID
		copied["recorded_at"] = moment
		rows = append(rows, copied)
	}
	return Frame{Columns: columns, Rows: rows}
}

// freshPath は既存と衝突しないファイル名を選ぶ。
func freshPath(directory string, day, moment time.Time, runID string) string {
	stem := fmt.Sprintf("%sT%sZ-%s", day.Format("2006-01-02"), moment.Format("150405"), runID)
	path := filepath.Join(directory, stem+".parquet")
	for n := 2; exists(path); n++ {
		// 枝番は "_" で繋ぐ。"-" だと名前順で本体（".parquet"）より前に並び、読む順が狂う
		path = filepath.Join(directory, fmt.Sprintf("%s_%d.parquet", stem, n))
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---- 読む ----------------------------------------------------------------

// Kinds は置き場にある種類の一覧。
func (s *Store) Kinds() []string {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return []string{}
	}
	kinds := []string{}
	for _, e := range entries {
		if e.IsDir() {
			kinds = append(kinds, e.Name())
		}
	}
	sort.Strings(kinds)
	return kinds
}

// Range は判定日の期間（両端を含む）。ゼロ値は「制限なし」。
type Range struct {
	Start time.Time
	End   time.Time
}

// Files は期間に入るファイルを古い順に返す。
func (s *Store) Files(kind string, window Range) []string {
	entries, err := os.ReadDir(s.Directory(kind))
	if err != nil {
		return []string{}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".parquet") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	found := []string{}
	for _, name := range names {
		day, ok := DayOf(name)
		if !ok {
			continue
		}
		if !window.Start.IsZero() && day.Before(truncateDay(window.Start)) {
			continue
		}
		if !window.End.IsZero() && day.After(truncateDay(window.End)) {
			continue
		}
		found = append(found, filepath.Join(s.Directory(kind), name))
	}
	return found
}

// DayOf はファイル名の先頭 10 文字（判定日）を読む。形式が違えば ok=false。
func DayOf(name string) (time.Time, bool) {
	base := filepath.Base(name)
	if len(base) < 10 {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", base[:10], time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

func truncateDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Days は記録のある判定日の一覧（昇順）。
func (s *Store) Days(kind string) []time.Time {
	seen := map[string]struct{}{}
	days := []time.Time{}
	for _, path := range s.Files(kind, Range{}) {
		day, ok := DayOf(path)
		if !ok {
			continue
		}
		key := day.Format("2006-01-02")
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		days = append(days, day)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	return days
}

// Read は期間のファイルを縦に結合する。無ければ列も行も無い空の表。
//
// 列が増えても古いファイルはそのまま読める（DuckDB の union_by_name=true が
// 名前で揃え、足りない列は null で埋める）。
func (s *Store) Read(kind string, window Range) (Frame, error) {
	paths := s.Files(kind, window)
	if len(paths) == 0 {
		return Frame{Columns: []Column{}, Rows: []map[string]any{}}, nil
	}
	frame, err := readParquetFiles(paths)
	if err != nil {
		return Frame{}, err
	}
	return reorderKeyFirst(frame), nil
}

// Latest はその日の最後の実行ぶんだけ（recorded_at が最大の行）を返す。
func (s *Store) Latest(kind string, day time.Time) (Frame, error) {
	frame, err := s.Read(kind, Range{Start: day, End: day})
	if err != nil || frame.Height() == 0 {
		return frame, err
	}
	var last time.Time
	for _, row := range frame.Rows {
		if t, ok := row["recorded_at"].(time.Time); ok && t.After(last) {
			last = t
		}
	}
	if last.IsZero() {
		return frame, nil
	}
	return frame.Filter(func(row map[string]any) bool {
		t, ok := row["recorded_at"].(time.Time)
		return ok && t.Equal(last)
	}), nil
}

// Summary は種類ごとのファイル数と期間。
func (s *Store) Summary() []KindSummary {
	out := []KindSummary{}
	for _, kind := range s.Kinds() {
		days := s.Days(kind)
		item := KindSummary{Kind: kind, Files: len(s.Files(kind, Range{}))}
		if len(days) > 0 {
			first, last := days[0], days[len(days)-1]
			item.FirstDay = &first
			item.LastDay = &last
		}
		out = append(out, item)
	}
	return out
}

// reorderKeyFirst は鍵の列を先頭に寄せる。
// Parquet の列は名前順に並ぶ（書き出しに使う parquet.Group がマップのため）ので、
// 読んだ直後は day / run_id / recorded_at が散らばっている。
func reorderKeyFirst(frame Frame) Frame {
	columns := make([]Column, 0, len(frame.Columns))
	for _, key := range KeyColumns {
		if c, ok := frame.ColumnOf(key); ok {
			columns = append(columns, c)
		}
	}
	for _, c := range frame.Columns {
		isKey := false
		for _, key := range KeyColumns {
			if c.Name == key {
				isKey = true
				break
			}
		}
		if !isKey {
			columns = append(columns, c)
		}
	}
	return Frame{Columns: columns, Rows: frame.Rows}
}
