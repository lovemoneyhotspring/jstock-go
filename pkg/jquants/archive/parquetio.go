package archive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
)

// epochDay は Parquet の DATE 論理型（1970-01-01 からの日数）に変換する。
func epochDay(iso string) (int32, bool) {
	t, err := time.Parse(dateLayout, iso)
	if err != nil {
		return 0, false
	}
	return int32(t.Unix() / 86400), true
}

// fromEpochDay は DATE 論理型を "YYYY-MM-DD" に戻す。
func fromEpochDay(days int32) string {
	return time.Unix(int64(days)*86400, 0).UTC().Format(dateLayout)
}

// buildSchema は列名から Parquet のスキーマを組み立てる。
// 値はすべて省略可能（NULL あり）で、日付列だけ DATE、他は UTF8 文字列。
// parquet-go の Group は列名の昇順に並ぶので、書き出しの列順もそれに従う。
func buildSchema(columns []string, dateColumns map[string]bool) (*parquet.Schema, []string) {
	sorted := append([]string(nil), columns...)
	sort.Strings(sorted)
	group := parquet.Group{}
	for _, name := range sorted {
		if dateColumns[name] {
			group[name] = parquet.Optional(parquet.Date())
			continue
		}
		group[name] = parquet.Optional(parquet.String())
	}
	return parquet.NewSchema("jquants", group), sorted
}

// writeParquet は表を 1 ファイルに書き出す。呼び出し側が一時ファイルに書いて
// rename する前提なので、ここでは追記や部分更新をしない。
func writeParquet(path string, f *Frame, dateColumns map[string]bool) error {
	schema, order := buildSchema(f.Columns, dateColumns)
	handle, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("Parquet を作成できません %s: %w", path, err)
	}
	writer := parquet.NewWriter(handle, schema, parquet.Compression(&parquet.Zstd))
	rows := make([]parquet.Row, 0, len(f.Rows))
	for _, row := range f.Rows {
		values := make(parquet.Row, len(order))
		for i, name := range order {
			values[i] = parquetValue(row[name], dateColumns[name], i)
		}
		rows = append(rows, values)
	}
	if len(rows) > 0 {
		if _, err := writer.WriteRows(rows); err != nil {
			handle.Close()
			return fmt.Errorf("Parquet の書き込みに失敗しました %s: %w", path, err)
		}
	}
	if err := writer.Close(); err != nil {
		handle.Close()
		return fmt.Errorf("Parquet の確定に失敗しました %s: %w", path, err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("Parquet を閉じられません %s: %w", path, err)
	}
	return nil
}

// parquetValue は 1 セルを Parquet の値にする。NULL は定義レベル 0。
func parquetValue(v *string, isDate bool, column int) parquet.Value {
	if v == nil {
		return parquet.NullValue().Level(0, 0, column)
	}
	if isDate {
		days, ok := epochDay(*v)
		if !ok {
			return parquet.NullValue().Level(0, 0, column)
		}
		return parquet.Int32Value(days).Level(0, 1, column)
	}
	return parquet.ByteArrayValue([]byte(*v)).Level(0, 1, column)
}

// readParquet は 1 ファイルを表として読む。日付列は物理型（INT32）で判別するので、
// 端点の定義が後から変わっても保存済みのファイルは読める。
func readParquet(path string) (*Frame, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Parquet を開けません %s: %w", path, err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return nil, fmt.Errorf("Parquet の大きさを取れません %s: %w", path, err)
	}
	file, err := parquet.OpenFile(handle, info.Size())
	if err != nil {
		return nil, fmt.Errorf("Parquet を解釈できません %s: %w", path, err)
	}
	columns := make([]string, 0)
	for _, leaf := range file.Schema().Columns() {
		columns = append(columns, leaf[len(leaf)-1])
	}
	out := &Frame{Columns: columns}
	buffer := make([]parquet.Row, 256)
	for _, group := range file.RowGroups() {
		reader := group.Rows()
		for {
			n, err := reader.ReadRows(buffer)
			for i := 0; i < n; i++ {
				out.Rows = append(out.Rows, decodeRow(buffer[i], columns))
			}
			if err != nil {
				reader.Close()
				if isEOF(err) {
					break
				}
				return nil, fmt.Errorf("Parquet の読み取りに失敗しました %s: %w", path, err)
			}
			if n == 0 {
				reader.Close()
				break
			}
		}
	}
	return out, nil
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

// decodeRow は Parquet の 1 行を文字列の map に戻す。
func decodeRow(row parquet.Row, columns []string) map[string]*string {
	out := make(map[string]*string, len(columns))
	for _, v := range row {
		i := v.Column()
		if i < 0 || i >= len(columns) {
			continue
		}
		name := columns[i]
		if v.IsNull() {
			out[name] = nil
			continue
		}
		switch v.Kind() {
		case parquet.Int32:
			out[name] = strptr(fromEpochDay(v.Int32()))
		default:
			out[name] = strptr(string(v.ByteArray()))
		}
	}
	return out
}
