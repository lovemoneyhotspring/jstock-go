package history

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/shopspring/decimal"
)

// ColumnType は履歴に書ける値の型。
//
// Python 版は polars の DataFrame をそのまま渡していた。Go には表の標準型が
// 無いので、Parquet に書ける最小限の型だけを持つ Frame をここで定義する。
// 型を絞るのは、追記した履歴を後から DuckDB で横断して読むため——列ごとに
// 型が揺れると union_by_name が破綻する。
type ColumnType int

const (
	// TypeString は文字列。値は string。
	TypeString ColumnType = iota
	// TypeInt64 は整数。値は int64。
	TypeInt64
	// TypeFloat64 は浮動小数。値は float64。
	TypeFloat64
	// TypeBool は真偽値。値は bool。
	TypeBool
	// TypeDate は日付。値は time.Time（UTC の 0 時）。
	TypeDate
	// TypeTimestamp は時刻。値は time.Time（UTC、マイクロ秒精度）。
	TypeTimestamp
)

func (t ColumnType) String() string {
	switch t {
	case TypeString:
		return "string"
	case TypeInt64:
		return "int64"
	case TypeFloat64:
		return "float64"
	case TypeBool:
		return "bool"
	case TypeDate:
		return "date"
	case TypeTimestamp:
		return "timestamp"
	default:
		return "unknown"
	}
}

// Column は列の名前と型。
type Column struct {
	Name string
	Type ColumnType
}

// Frame は履歴に書き出す（あるいは読み出した）表。
//
// 行は列名 → 値のマップ。値が nil なら欠損（Parquet では null）。
// 列の並びは Columns が持つ（マップは順序を持たないため）。
type Frame struct {
	Columns []Column
	Rows    []map[string]any
}

// NewFrame は列と行から表を作る。
func NewFrame(columns []Column, rows []map[string]any) Frame {
	if rows == nil {
		rows = []map[string]any{}
	}
	return Frame{Columns: columns, Rows: rows}
}

// Height は行数。
func (f Frame) Height() int { return len(f.Rows) }

// Width は列数。
func (f Frame) Width() int { return len(f.Columns) }

// IsEmpty は行が無いか。
func (f Frame) IsEmpty() bool { return len(f.Rows) == 0 }

// Names は列名の並び。
func (f Frame) Names() []string {
	names := make([]string, len(f.Columns))
	for i, c := range f.Columns {
		names[i] = c.Name
	}
	return names
}

// Has は列があるか。
func (f Frame) Has(name string) bool {
	for _, c := range f.Columns {
		if c.Name == name {
			return true
		}
	}
	return false
}

// ColumnOf は名前から列定義を引く。
func (f Frame) ColumnOf(name string) (Column, bool) {
	for _, c := range f.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// Drop は指定した列を落とした表を返す（元の表は変えない）。
func (f Frame) Drop(names ...string) Frame {
	remove := make(map[string]struct{}, len(names))
	for _, n := range names {
		remove[n] = struct{}{}
	}
	columns := make([]Column, 0, len(f.Columns))
	for _, c := range f.Columns {
		if _, drop := remove[c.Name]; !drop {
			columns = append(columns, c)
		}
	}
	rows := make([]map[string]any, 0, len(f.Rows))
	for _, row := range f.Rows {
		copied := make(map[string]any, len(columns))
		for _, c := range columns {
			copied[c.Name] = row[c.Name]
		}
		rows = append(rows, copied)
	}
	return Frame{Columns: columns, Rows: rows}
}

// Filter は述語に合う行だけの表を返す。
func (f Frame) Filter(keep func(row map[string]any) bool) Frame {
	rows := make([]map[string]any, 0, len(f.Rows))
	for _, row := range f.Rows {
		if keep(row) {
			rows = append(rows, row)
		}
	}
	return Frame{Columns: f.Columns, Rows: rows}
}

// SortBy は列の値で並べ替えた表を返す。descending が真なら降順。
// 比較できない値（型違い）は文字列表現で比べる。
func (f Frame) SortBy(names []string, descending []bool) Frame {
	rows := make([]map[string]any, len(f.Rows))
	copy(rows, f.Rows)
	sort.SliceStable(rows, func(i, j int) bool {
		for k, name := range names {
			cmp := compareValues(rows[i][name], rows[j][name])
			if cmp == 0 {
				continue
			}
			if k < len(descending) && descending[k] {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
	return Frame{Columns: f.Columns, Rows: rows}
}

// Head は先頭 n 行。
func (f Frame) Head(n int) Frame {
	if n < 0 || n >= len(f.Rows) {
		return f
	}
	return Frame{Columns: f.Columns, Rows: f.Rows[:n]}
}

// ToMaps は行の並びを返す。output.RowsOf（JSON 出力）が使う。
func (f Frame) ToMaps() []map[string]any {
	out := make([]map[string]any, 0, len(f.Rows))
	for _, row := range f.Rows {
		copied := make(map[string]any, len(row))
		for key, value := range row {
			copied[key] = value
		}
		out = append(out, copied)
	}
	return out
}

// Get は行から値を取り出す（無ければ nil）。
func Get(row map[string]any, name string) any {
	if row == nil {
		return nil
	}
	return row[name]
}

// compareValues は履歴に載る型どうしを比べる。
func compareValues(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	// 欠損は常に最小（並べたときに末尾に寄らないので、降順でも先頭に来ない）
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	switch av := a.(type) {
	case time.Time:
		if bv, ok := b.(time.Time); ok {
			switch {
			case av.Before(bv):
				return -1
			case av.After(bv):
				return 1
			default:
				return 0
			}
		}
	case int64:
		if bv, ok := b.(int64); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			default:
				return 0
			}
		}
	case float64:
		if bv, ok := b.(float64); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			default:
				return 0
			}
		}
	case bool:
		if bv, ok := b.(bool); ok {
			switch {
			case av == bv:
				return 0
			case !av:
				return -1
			default:
				return 1
			}
		}
	case string:
		if bv, ok := b.(string); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			default:
				return 0
			}
		}
	}
	as, bs := fmt.Sprint(a), fmt.Sprint(b)
	switch {
	case as < bs:
		return -1
	case as > bs:
		return 1
	default:
		return 0
	}
}

// ToFloat は Decimal / 文字列 / 整数を float64 にする。空や変換不能なら nil。
// 履歴は集計のための表なので、金額もここでは浮動小数にする
// （正確な残高の記録は台帳が持つ）。
func ToFloat(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case decimal.Decimal:
		f, _ := v.Float64()
		return f
	case *decimal.Decimal:
		if v == nil {
			return nil
		}
		f, _ := v.Float64()
		return f
	case string:
		if v == "" {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		return f
	default:
		return nil
	}
}

// ToInt は ToFloat と同じ入力を int64 にする。
func ToInt(value any) any {
	f := ToFloat(value)
	if f == nil {
		return nil
	}
	return int64(f.(float64))
}
