package history

import (
	"database/sql"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

// readParquetFiles は複数の Parquet を縦に結合して読む。
//
// 読みと集計を DuckDB に任せるのは、列の揃わないファイル群（列は後から増える）を
// union_by_name=true が名前で揃えてくれるため。自前で読むとファイルごとの
// スキーマ突き合わせを全部書くことになる。
func readParquetFiles(paths []string) (Frame, error) {
	db, err := storage.OpenDuckDB()
	if err != nil {
		return Frame{}, fmt.Errorf("履歴の読み出しに DuckDB を開けません: %w", err)
	}
	defer db.Close()

	quoted := make([]string, 0, len(paths))
	for _, path := range paths {
		quoted = append(quoted, "'"+strings.ReplaceAll(path, "'", "''")+"'")
	}
	query := "SELECT * FROM read_parquet([" + strings.Join(quoted, ", ") + "], union_by_name=true)"
	return QueryFrame(db, query)
}

// QueryFrame は SQL の結果を Frame にする。DuckDB を使う他の集計
// （BarStore.Query など）からも使えるように公開している。
func QueryFrame(db *sql.DB, query string) (Frame, error) {
	rows, err := db.Query(query)
	if err != nil {
		return Frame{}, fmt.Errorf("問い合わせに失敗しました: %w", err)
	}
	defer rows.Close()

	types, err := rows.ColumnTypes()
	if err != nil {
		return Frame{}, err
	}
	columns := make([]Column, len(types))
	for i, t := range types {
		columns[i] = Column{Name: t.Name(), Type: columnTypeOf(t.DatabaseTypeName())}
	}

	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return Frame{}, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column.Name] = normalizeScanned(values[i], column.Type)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return Frame{}, err
	}
	return Frame{Columns: columns, Rows: out}, nil
}

// columnTypeOf は DuckDB の型名を Frame の列型に対応させる。
func columnTypeOf(name string) ColumnType {
	upper := strings.ToUpper(name)
	switch {
	case strings.HasPrefix(upper, "DATE"):
		return TypeDate
	case strings.HasPrefix(upper, "TIMESTAMP"):
		return TypeTimestamp
	case upper == "BOOLEAN":
		return TypeBool
	case strings.HasPrefix(upper, "DECIMAL"), upper == "DOUBLE", upper == "FLOAT", upper == "REAL":
		return TypeFloat64
	case strings.HasSuffix(upper, "INT"), strings.HasPrefix(upper, "UINT"), upper == "HUGEINT":
		return TypeInt64
	default:
		return TypeString
	}
}

// normalizeScanned はドライバが返す値を、列型が約束する Go の型に揃える。
// 揃えておかないと、比較（Latest の recorded_at）や JSON 出力で型ごとの
// 場合分けが呼び出し側に漏れる。
func normalizeScanned(value any, columnType ColumnType) any {
	if value == nil {
		return nil
	}
	switch columnType {
	case TypeDate, TypeTimestamp:
		if t, ok := value.(time.Time); ok {
			return t.UTC()
		}
	case TypeInt64:
		switch v := value.(type) {
		case int64:
			return v
		case int32:
			return int64(v)
		case int:
			return int64(v)
		case uint64:
			return int64(v)
		case *big.Int:
			return v.Int64()
		}
	case TypeFloat64:
		// DuckDB の DECIMAL はドライバ固有の型で返る。Float64() を持つので
		// インターフェースで受ける（型を直に参照すると driver に依存する）
		if d, ok := value.(interface{ Float64() float64 }); ok {
			return d.Float64()
		}
		if f := ToFloat(value); f != nil {
			return f
		}
		return nil
	case TypeBool:
		if b, ok := value.(bool); ok {
			return b
		}
	case TypeString:
		switch v := value.(type) {
		case string:
			return v
		case []byte:
			return string(v)
		default:
			return fmt.Sprint(v)
		}
	}
	return value
}
