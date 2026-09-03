package history

import (
	"fmt"
	"os"
	"time"

	"github.com/parquet-go/parquet-go"
)

// epochDate は Parquet の DATE 論理型（エポックからの日数）の基準日。
var epochDate = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// schemaOf は Frame の列定義から Parquet のスキーマを組む。
//
// すべての列を optional にするのは、履歴が「後から列が増える」前提の追記ストア
// だから。required にすると欠損のある行が書けず、記録を落とすことになる。
func schemaOf(frame Frame) *parquet.Schema {
	group := parquet.Group{}
	for _, column := range frame.Columns {
		group[column.Name] = parquet.Optional(leafOf(column.Type))
	}
	return parquet.NewSchema("history", group)
}

func leafOf(t ColumnType) parquet.Node {
	switch t {
	case TypeInt64:
		return parquet.Int(64)
	case TypeFloat64:
		return parquet.Leaf(parquet.DoubleType)
	case TypeBool:
		return parquet.Leaf(parquet.BooleanType)
	case TypeDate:
		return parquet.Date()
	case TypeTimestamp:
		// マイクロ秒。ミリ秒だと同一秒内の複数実行を並べたときに順序が壊れうる
		return parquet.Timestamp(parquet.Microsecond)
	default:
		return parquet.String()
	}
}

// writeParquet は Frame を 1 ファイルとして書き出す。
//
// parquet-go の型付きライタは Go の構造体を前提にしているが、履歴の列は
// 呼び出し側が実行時に決める。そこでスキーマを動的に組み、行を
// parquet.Row（列ごとの値の並び）として直接書く。
func writeParquet(path string, frame Frame) error {
	schema := schemaOf(frame)
	types := make(map[string]ColumnType, len(frame.Columns))
	for _, column := range frame.Columns {
		types[column.Name] = column.Type
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("履歴ファイルを作れません %s: %w", path, err)
	}
	defer f.Close()

	writer := parquet.NewGenericWriter[any](f, schema, parquet.Compression(&parquet.Zstd))

	// スキーマの列順は名前順（parquet.Group がマップのため）。行を組むときは
	// この順に合わせる必要がある
	paths := schema.Columns()
	rows := make([]parquet.Row, 0, len(frame.Rows))
	for _, source := range frame.Rows {
		row := make(parquet.Row, 0, len(paths))
		for index, columnPath := range paths {
			name := columnPath[len(columnPath)-1]
			value, err := parquetValue(source[name], types[name])
			if err != nil {
				return fmt.Errorf("列 %q: %w", name, err)
			}
			definition := 1
			if value.IsNull() {
				definition = 0
			}
			row = append(row, value.Level(0, definition, index))
		}
		rows = append(rows, row)
	}

	if len(rows) > 0 {
		if _, err := writer.WriteRows(rows); err != nil {
			return fmt.Errorf("履歴の書き込みに失敗しました %s: %w", path, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("履歴の確定に失敗しました %s: %w", path, err)
	}
	return nil
}

// parquetValue は Go の値を宣言した列型の Parquet 値に変換する。
// 型が合わない値は黙って落とさずエラーにする——履歴は後から直せないため。
func parquetValue(value any, columnType ColumnType) (parquet.Value, error) {
	null := parquet.ValueOf(nil)
	if value == nil {
		return null, nil
	}
	switch columnType {
	case TypeString:
		switch v := value.(type) {
		case string:
			return parquet.ValueOf(v), nil
		case fmt.Stringer:
			return parquet.ValueOf(v.String()), nil
		default:
			return parquet.ValueOf(fmt.Sprint(v)), nil
		}
	case TypeInt64:
		switch v := value.(type) {
		case int64:
			return parquet.ValueOf(v), nil
		case int:
			return parquet.ValueOf(int64(v)), nil
		case int32:
			return parquet.ValueOf(int64(v)), nil
		case float64:
			return parquet.ValueOf(int64(v)), nil
		}
	case TypeFloat64:
		switch v := value.(type) {
		case float64:
			return parquet.ValueOf(v), nil
		case float32:
			return parquet.ValueOf(float64(v)), nil
		case int64:
			return parquet.ValueOf(float64(v)), nil
		case int:
			return parquet.ValueOf(float64(v)), nil
		}
	case TypeBool:
		if v, ok := value.(bool); ok {
			return parquet.ValueOf(v), nil
		}
	case TypeDate:
		if v, ok := value.(time.Time); ok {
			days := int32(v.UTC().Truncate(24*time.Hour).Sub(epochDate) / (24 * time.Hour))
			return parquet.ValueOf(days), nil
		}
	case TypeTimestamp:
		if v, ok := value.(time.Time); ok {
			return parquet.ValueOf(v.UTC().UnixMicro()), nil
		}
	}
	return null, fmt.Errorf("%s 型の列に %T は書けません", columnType, value)
}
