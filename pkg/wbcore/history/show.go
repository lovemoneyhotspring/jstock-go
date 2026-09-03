package history

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
)

// ShowOptions は Show の表示条件。
type ShowOptions struct {
	// Window は判定日の期間（両端を含む）。
	Window Range
	// LatestOnly なら、その日の最後の実行ぶんだけを見せる。
	LatestOnly bool
	// Limit は表として見せる行数の上限（0 なら DefaultLimit）。
	Limit int
	// CSVPath が空でなければ、絞り込んだ全行を CSV に書き出す。
	CSVPath string
	// AsJSON なら表を出さず、JSON を 1 個だけ書く。
	AsJSON bool
}

// DefaultLimit は表として見せる既定の行数。
const DefaultLimit = 50

// Show は kind を空にすれば種類の一覧、指定すれば期間の行を見せる（CSV 書き出し可）。
//
// AsJSON のときは表を出さず、JSON を 1 個だけ書く。読み手が AI のときは罫線と
// 桁区切りが邪魔になるだけなので、Limit の間引きもしない（全行を返す。件数を
// 絞るのは呼ぶ側の期間指定の仕事）。
func Show(w io.Writer, store *Store, kind string, opts ShowOptions) error {
	if kind == "" {
		return showKinds(w, store, opts)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	var frame Frame
	var err error
	if opts.LatestOnly {
		end := opts.Window.End
		if end.IsZero() {
			days := store.Days(kind)
			if len(days) == 0 {
				if opts.AsJSON {
					return output.EmitJSONTo(w, map[string]any{"ok": true, "kind": kind, "rows": []any{}})
				}
				_, err := fmt.Fprintf(w, "%s: 履歴はまだありません\n", kind)
				return err
			}
			end = days[len(days)-1]
		}
		frame, err = store.Latest(kind, end)
	} else {
		frame, err = store.Read(kind, opts.Window)
	}
	if err != nil {
		return err
	}

	if opts.AsJSON {
		return output.EmitJSONTo(w, map[string]any{
			"ok":    true,
			"kind":  kind,
			"count": frame.Height(),
			"rows":  output.RowsOf(frame),
		})
	}
	if frame.Height() == 0 {
		_, err := fmt.Fprintf(w, "%s: 該当する行はありません\n", kind)
		return err
	}
	if opts.CSVPath != "" {
		// 分析用なので値はそのまま（桁区切りや丸めは表示だけ）
		if err := WriteCSV(opts.CSVPath, frame); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%d 行を %s に書き出しました\n", frame.Height(), opts.CSVPath); err != nil {
			return err
		}
	}
	shown := frame.SortBy([]string{"day", "recorded_at"}, []bool{true, true}).Head(limit)
	fmt.Fprintf(w, "%s  %d 行（表示 %d）\n", kind, frame.Height(), shown.Height())
	return writeTable(w, shown)
}

func showKinds(w io.Writer, store *Store, opts ShowOptions) error {
	rows := store.Summary()
	if opts.AsJSON {
		kinds := make([]any, 0, len(rows))
		for _, s := range rows {
			kinds = append(kinds, map[string]any{
				"kind":      s.Kind,
				"files":     s.Files,
				"first_day": dayValue(s.FirstDay),
				"last_day":  dayValue(s.LastDay),
			})
		}
		return output.EmitJSONTo(w, map[string]any{
			"ok":    true,
			"root":  store.Root,
			"kinds": kinds,
		})
	}
	fmt.Fprintf(w, "履歴 %s\n", store.Root)
	fmt.Fprintf(w, "%-20s %10s %12s %12s\n", "種類", "ファイル数", "最初の日", "最後の日")
	for _, s := range rows {
		fmt.Fprintf(w, "%-20s %10d %12s %12s\n", s.Kind, s.Files, dayText(s.FirstDay), dayText(s.LastDay))
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "履歴はまだありません")
	}
	return nil
}

func dayValue(day *time.Time) any {
	if day == nil {
		return nil
	}
	return day.Format("2006-01-02")
}

func dayText(day *time.Time) string {
	if day == nil {
		return ""
	}
	return day.Format("2006-01-02")
}

// Cell は表に出す 1 セルの文字列。
func Cell(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case time.Time:
		if v.Hour() == 0 && v.Minute() == 0 && v.Second() == 0 && v.Nanosecond() == 0 {
			return v.Format("2006-01-02")
		}
		return v.Format("2006-01-02T15:04:05")
	case float64:
		// 末尾の 0 を落として読みやすくする（表示だけの話で、CSV には効かせない）
		text := strconv.FormatFloat(v, 'f', 4, 64)
		text = strings.TrimRight(text, "0")
		return strings.TrimSuffix(text, ".")
	case bool:
		return strconv.FormatBool(v)
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func writeTable(w io.Writer, frame Frame) error {
	names := frame.Names()
	if _, err := fmt.Fprintln(w, strings.Join(names, "\t")); err != nil {
		return err
	}
	for _, row := range frame.Rows {
		cells := make([]string, len(names))
		for i, name := range names {
			cells[i] = Cell(row[name])
		}
		if _, err := fmt.Fprintln(w, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return nil
}

// WriteCSV は表を CSV に書き出す。分析用なので値は丸めない。
func WriteCSV(path string, frame Frame) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("CSV を作れません %s: %w", path, err)
	}
	defer f.Close()

	writer := csv.NewWriter(f)
	names := frame.Names()
	if err := writer.Write(names); err != nil {
		return err
	}
	for _, row := range frame.Rows {
		cells := make([]string, len(names))
		for i, name := range names {
			cells[i] = csvCell(row[name])
		}
		if err := writer.Write(cells); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func csvCell(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case time.Time:
		if v.Hour() == 0 && v.Minute() == 0 && v.Second() == 0 && v.Nanosecond() == 0 {
			return v.Format("2006-01-02")
		}
		return v.Format(time.RFC3339Nano)
	default:
		return Cell(v)
	}
}
