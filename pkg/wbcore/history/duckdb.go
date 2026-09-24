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

	return QueryFrame(db, "SELECT * FROM "+parquetSource(paths, ""))
}

// parquetSource は read_parquet(...) の句を作る。extra は足すオプション（", filename=true" など）。
func parquetSource(paths []string, extra string) string {
	return "read_parquet([" + quoteAll(paths) + "], union_by_name=true" + extra + ")"
}

func quoteAll(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, "'"+strings.ReplaceAll(v, "'", "''")+"'")
	}
	return strings.Join(quoted, ", ")
}

// readParquetHead は新しい順（day・recorded_at の降順）の先頭 n 行と、全体の行数を返す。
//
// 全行を Frame（行ごとの map）に載せてから並べて切ると、板の記録（週に数十 MB 増える）では
// 表の 50 行のために全部を展開することになる。並べ替えと切り出しは DuckDB に任せ、Go には
// n 行だけ持ってくる。同じ (day, recorded_at) の行は、全部読んでから安定ソートしたときと同じく
// ファイル名の順・ファイルの中の順に並べる（filename・file_row_number で決める）。
// 列は全ファイルの和（union_by_name）なので、全部読んだときと同じ列が同じ順で並ぶ。
func readParquetHead(paths []string, n int) (Frame, int, error) {
	db, err := storage.OpenDuckDB()
	if err != nil {
		return Frame{}, 0, fmt.Errorf("履歴の読み出しに DuckDB を開けません: %w", err)
	}
	defer db.Close()

	var total int64
	if err := db.QueryRow("SELECT count(*) FROM " + parquetSource(paths, "")).Scan(&total); err != nil {
		return Frame{}, 0, fmt.Errorf("問い合わせに失敗しました: %w", err)
	}
	query := fmt.Sprintf("SELECT * EXCLUDE (filename, file_row_number) FROM %s "+
		"ORDER BY day DESC, recorded_at DESC, filename, file_row_number LIMIT %d",
		parquetSource(paths, ", filename=true, file_row_number=true"), n)
	frame, err := QueryFrame(db, query)
	if err != nil {
		return Frame{}, 0, err
	}
	return frame, int(total), nil
}

// readParquetLatest は recorded_at が最大のファイルの行だけを読む（Store.Latest の中身）。
//
// まず recorded_at の列だけを読んでファイルごとの最大を取り、その値を持つファイルだけを
// 本読みする。1 ファイル = 1 回の実行なので、その日の他の実行ぶんを Go に展開しない。
// 本読みも全ファイルを並べた read_parquet から filename で絞るので、列は全ファイルの和のまま
// （全部読んでから絞ったときと同じ列・同じ順）。
// recorded_at が時刻として読めない（列が無い・全部 null）ときは ok=false を返し、呼ぶ側が全部読む。
func readParquetLatest(paths []string) (frame Frame, last time.Time, ok bool, err error) {
	db, err := storage.OpenDuckDB()
	if err != nil {
		return Frame{}, time.Time{}, false, fmt.Errorf("履歴の読み出しに DuckDB を開けません: %w", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT filename, max(recorded_at) FROM " +
		parquetSource(paths, ", filename=true") + " GROUP BY filename")
	if err != nil {
		// recorded_at の無い置き場など。全部読んで絞る側に任せる
		return Frame{}, time.Time{}, false, nil
	}
	latestFiles := map[string]time.Time{}
	for rows.Next() {
		var name string
		var value any
		if err := rows.Scan(&name, &value); err != nil {
			rows.Close()
			return Frame{}, time.Time{}, false, err
		}
		if t, isTime := value.(time.Time); isTime {
			latestFiles[name] = t.UTC()
			if t.After(last) {
				last = t.UTC()
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Frame{}, time.Time{}, false, err
	}
	rows.Close()
	if last.IsZero() {
		return Frame{}, time.Time{}, false, nil
	}
	picked := []string{}
	for name, t := range latestFiles {
		if t.Equal(last) {
			picked = append(picked, name)
		}
	}
	query := "SELECT * EXCLUDE (filename) FROM " + parquetSource(paths, ", filename=true") +
		" WHERE filename IN (" + quoteAll(picked) + ")"
	frame, err = QueryFrame(db, query)
	return frame, last, true, err
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
