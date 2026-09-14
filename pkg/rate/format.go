package rate

import (
	"fmt"
	"math"
	"strings"
)

// HHMM は掲載日 0 時からの経過時間を HH:MM で見せる（日またぎは 24 時超えのまま出す）。
// 分に丸める。負（掲載日より前に見えた）は先頭に - を付ける。
func HHMM(hours float64) string {
	total := int(math.Round(hours * 60))
	sign := ""
	if total < 0 {
		sign, total = "-", -total
	}
	return fmt.Sprintf("%s%02d:%02d", sign, total/60, total%60)
}

// TrimTS は RFC3339 の時刻を "YYYY-MM-DD HH:MM" に縮める。短ければそのまま。
func TrimTS(ts string) string {
	if len(ts) >= 16 {
		return strings.Replace(ts[:16], "T", " ", 1)
	}
	return ts
}
