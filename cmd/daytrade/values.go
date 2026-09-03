package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
)

// 履歴（DuckDB 経由）から読んだ値の取り出し。列の型はドライバによって揺れるので、
// 表示の直前でここに寄せる。

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func fOf(v any) float64 {
	if f, ok := history.ToFloat(v).(float64); ok {
		return f
	}
	return 0
}

func fPtrOf(v any) *float64 {
	if v == nil {
		return nil
	}
	f, ok := history.ToFloat(v).(float64)
	if !ok {
		return nil
	}
	return &f
}

func iOf(v any) int64 {
	if i, ok := history.ToInt(v).(int64); ok {
		return i
	}
	return 0
}

func bOf(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func bpText(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%+.1f", fOf(v))
}

func rateText(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%.0f%%", fOf(v)*100)
}

// pnlText は円損益。-0 は +0 に寄せる（表で紛らわしいだけなので）。
func pnlText(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%+s", yen(fOf(v)+0.0))
}

func gapText(v any) string {
	if v == nil {
		return ""
	}
	return pct(fOf(v))
}

func yenOrBlank(v any) string {
	if v == nil {
		return ""
	}
	return yen(fOf(v))
}
