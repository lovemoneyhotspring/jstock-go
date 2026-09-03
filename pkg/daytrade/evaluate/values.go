package evaluate

import "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"

// history から読み出した値の取り出し。DuckDB の型は列によって揺れる（DECIMAL / DOUBLE /
// HUGEINT）ので、ここで 1 箇所に寄せる。

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func floatOf(v any) float64 {
	if f, ok := history.ToFloat(v).(float64); ok {
		return f
	}
	return 0
}

func floatPtrOf(v any) *float64 {
	if v == nil {
		return nil
	}
	f, ok := history.ToFloat(v).(float64)
	if !ok {
		return nil
	}
	return &f
}

func intOf(v any) int64 {
	if i, ok := history.ToInt(v).(int64); ok {
		return i
	}
	return 0
}

func boolOf(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
