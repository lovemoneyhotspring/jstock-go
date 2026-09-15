package usmarket

import "time"

// newYork は NYSE の時間帯。tzdata が無い環境では固定の −05:00 にする（夏時間の間は引けの
// 判定が 1 時間遅れるだけで、途中の値を終値と読むことはない）。
var newYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.FixedZone("EST", -5*3600)
	}
	return loc
}()

// ExpectedSession は判定日 day（東証の日付）の寄付前に確定しているはずの米国セッション
// ——day−1 以前で最新の NYSE の取引日（UTC の 0 時）。祝日明けに前々夜の値を「古い」と
// 誤らないよう、休場日を飛ばす。
func ExpectedSession(day time.Time) time.Time {
	y, m, d := day.Date()
	cur := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	for !NYSEOpen(cur) {
		cur = cur.AddDate(0, 0, -1)
	}
	return cur
}

// IsFresh は s が判定日 day の前夜のセッション（ExpectedSession）か。nil なら偽。
func IsFresh(s *Session, day time.Time) bool {
	return s != nil && s.Date.Format(dateLayout) == ExpectedSession(day).Format(dateLayout)
}

// NYSEOpen は date（年月日だけを見る）が NYSE の取引日か。土日と定例の休場日を除く。
// 臨時の休場（国葬など）は知らない——その日は「古い」扱いになり、待つ時刻を過ぎてから判定する。
func NYSEOpen(date time.Time) bool {
	y, m, d := date.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	for _, h := range nyseHolidays(y) {
		if h.Equal(day) {
			return false
		}
	}
	return true
}

// nyseHolidays は year の定例の休場日（振替後）。
func nyseHolidays(year int) []time.Time {
	date := func(m time.Month, d int) time.Time { return time.Date(year, m, d, 0, 0, 0, 0, time.UTC) }
	out := []time.Time{
		nthWeekday(year, time.January, time.Monday, 3),    // キング牧師記念日
		nthWeekday(year, time.February, time.Monday, 3),   // 大統領の日
		easter(year).AddDate(0, 0, -2),                    // 聖金曜日
		lastWeekday(year, time.May, time.Monday),          // 戦没者追悼記念日
		observed(date(time.July, 4)),                      // 独立記念日
		nthWeekday(year, time.September, time.Monday, 1),  // 労働者の日
		nthWeekday(year, time.November, time.Thursday, 4), // 感謝祭
		observed(date(time.December, 25)),                 // クリスマス
	}
	// 元日が土曜なら前年の 12/31 は休まない（NYSE の規則）。日曜なら 1/2 に振り替える
	if newYear := date(time.January, 1); newYear.Weekday() != time.Saturday {
		out = append(out, observed(newYear))
	}
	if year >= 2022 {
		out = append(out, observed(date(time.June, 19))) // ジューンティーンス
	}
	return out
}

// observed は土曜の祝日を金曜に、日曜の祝日を月曜に振り替える。
func observed(d time.Time) time.Time {
	switch d.Weekday() {
	case time.Saturday:
		return d.AddDate(0, 0, -1)
	case time.Sunday:
		return d.AddDate(0, 0, 1)
	}
	return d
}

// nthWeekday は year 年 month 月の第 n の wd 曜日。
func nthWeekday(year int, month time.Month, wd time.Weekday, n int) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	offset := (int(wd) - int(first.Weekday()) + 7) % 7
	return first.AddDate(0, 0, offset+7*(n-1))
}

// lastWeekday は year 年 month 月の最後の wd 曜日。
func lastWeekday(year int, month time.Month, wd time.Weekday) time.Time {
	last := time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	offset := (int(last.Weekday()) - int(wd) + 7) % 7
	return last.AddDate(0, 0, -offset)
}

// easter はグレゴリオ暦の復活祭（Anonymous Gregorian algorithm）。
func easter(year int) time.Time {
	a := year % 19
	b, c := year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}
