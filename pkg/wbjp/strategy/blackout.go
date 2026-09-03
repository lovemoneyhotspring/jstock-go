package strategy

import (
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Blackout は銘柄ごとの決算発表日の一覧。
//
// 日足では発表翌日のギャップを避けられないため、発表日の手前で建てるのを
// 止め、保有中なら降りるのに使う。
type Blackout map[string][]time.Time

// LoadBlackout は決算日のブラックアウト表を読む。
//
// 形式（TOML）:
//
//	[earnings]
//	AAPL = ["2026-10-29", "2027-01-28"]
//	MSFT = ["2026-10-27"]
//
// 日付は決算発表日。発表が引け後でも、当日は「翌日ギャップの前日」なので
// ブラックアウトに含める。
func LoadBlackout(path string) (Blackout, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ブラックアウト表が見つかりません: %s: %w", path, err)
	}

	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("ブラックアウト表を読めません: %s: %w", path, err)
	}
	// [earnings] セクションが無ければトップレベルをそのまま表とみなす。
	table := raw
	if section, ok := raw["earnings"].(map[string]any); ok {
		table = section
	}

	out := make(Blackout, len(table))
	for symbol, value := range table {
		list, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: 日付のリストを指定してください", symbol)
		}
		for _, item := range list {
			d, err := parseBlackoutDate(item)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", symbol, err)
			}
			out[symbol] = append(out[symbol], d)
		}
	}
	return out, nil
}

func parseBlackoutDate(item any) (time.Time, error) {
	switch v := item.(type) {
	case time.Time:
		return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC), nil
	case toml.LocalDate:
		return time.Date(v.Year, time.Month(v.Month), v.Day, 0, 0, 0, 0, time.UTC), nil
	case string:
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			return time.Time{}, fmt.Errorf("日付として読めません: %q", v)
		}
		return d, nil
	default:
		return time.Time{}, fmt.Errorf("日付として読めません: %v", item)
	}
}

// InBlackout は asOf（YYYY-MM-DD）が symbol のブラックアウト期間に入っているか。
//
// 決算発表日の daysBefore 日前から当日までを対象にする。
func (b Blackout) InBlackout(symbol, asOf string, daysBefore int) bool {
	if len(b) == 0 || asOf == "" {
		return false
	}
	day, err := time.Parse("2006-01-02", asOf)
	if err != nil {
		return false
	}
	for _, earnings := range b[symbol] {
		start := earnings.AddDate(0, 0, -daysBefore)
		if !day.Before(start) && !day.After(earnings) {
			return true
		}
	}
	return false
}
