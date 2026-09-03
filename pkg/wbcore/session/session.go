// Package session は取引所の引け時刻と、市場をまたぐ判定の前後関係を扱う。
//
// 東証の銘柄を米国の指数で判定するとき（積立の signal_symbol）に、同じ日付の
// 指数の足が判断時点で存在するかを ClosesAfter で決める。
package session

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// closes は通常取引の引け（現地時刻）。
var closes = map[domain.Market]struct{ hour, minute int }{
	domain.MarketJP: {15, 30},
	domain.MarketUS: {16, 0},
}

// CloseUTC はその日の引けを UTC で返す。
//
// 夏時間の有無は日付で決まるので on を受ける。ゼロ値なら本日（UTC）。
func CloseUTC(market domain.Market, on time.Time) (time.Time, error) {
	c, ok := closes[market]
	if !ok {
		return time.Time{}, fmt.Errorf("引け時刻が未定義の市場です: %s", market)
	}
	if on.IsZero() {
		on = clock.NowUTC()
	}
	loc := market.Timezone()
	local := time.Date(on.Year(), on.Month(), on.Day(), c.hour, c.minute, 0, 0, loc)
	return local.UTC(), nil
}

// ClosesAfter は market の引けが、同じ暦日の other の引けより後かを返す。
//
// 「日付が同じ足」でも、引けが後の市場の足はその日の判断時点にまだ無い。
// 東証の銘柄を NASDAQ の指数で判定するなら、同じ日付の指数の足は使えず
// 前日の足を使う（米国の引け 16:00 ET は翌日 05〜06:00 JST）。
func ClosesAfter(market, other domain.Market, on time.Time) (bool, error) {
	a, err := CloseUTC(market, on)
	if err != nil {
		return false, err
	}
	b, err := CloseUTC(other, on)
	if err != nil {
		return false, err
	}
	// 暦日の中での時刻同士を比べる（Python 版の .time() 比較と同じ）。
	return timeOfDay(a) > timeOfDay(b), nil
}

// timeOfDay は UTC の時刻部分を「その日の経過秒」にする。
func timeOfDay(t time.Time) int {
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

// ParseTime は "09:30" のような表記を時・分にする。
func ParseTime(value, label string) (hour, minute int, err error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%s は HH:MM 形式で指定します: %q", label, value)
	}
	hour, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("%s は HH:MM 形式で指定します: %q", label, value)
	}
	minute, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("%s は HH:MM 形式で指定します: %q", label, value)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%s の時刻が範囲外です: %q", label, value)
	}
	return hour, minute, nil
}
