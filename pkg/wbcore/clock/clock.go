package clock

import (
	"fmt"
	"strings"
	"time"
)

var (
	// UTC は常に UTC ロケーションを表す。
	UTC = time.UTC

	// Tokyo は JST (Asia/Tokyo) ロケーションを表す。
	// tzdata の無い環境でも UTC に化けないよう、読めなければ固定の +09:00 にする。
	Tokyo = loadTokyo()

	// Now は現在時刻の出所。試験で時刻を固定したいときに差し替える
	// （NowUTC / NowJST / TodayUTC / TodayJST / StampISO はすべてここを通る）。
	Now = time.Now
)

// loadTokyo は Asia/Tokyo を読み、無ければ固定の JST（+09:00）を返す。
// 日本に夏時間は無いので、固定オフセットでも時刻は変わらない。
func loadTokyo() *time.Location {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil || loc == nil {
		return time.FixedZone("JST", 9*3600)
	}
	return loc
}

// NowUTC は現在時刻を UTC で返す。
func NowUTC() time.Time {
	return Now().UTC()
}

// NowJST は現在時刻を JST で返す。
func NowJST() time.Time {
	return Now().In(Tokyo)
}

// TodayUTC は UTC で見た今日の日付（年月日のみ、時刻は 00:00:00 UTC）を返す。
func TodayUTC() time.Time {
	now := NowUTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// TodayJST は JST で見た今日の日付（年月日のみ、時刻は 00:00:00 JST）を返す。
// 日本の営業日を数えるコマンドはこちらを使う。TodayUTC は 09:00 JST までは前日になる。
func TodayJST() time.Time {
	now := NowJST()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, Tokyo)
}

// ParseDateJST は "YYYY-MM-DD" を JST の 00:00 として読む。
func ParseDateJST(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, Tokyo)
}

// EnsureUTC は時刻を UTC に揃える。タイムゾーン情報が未指定の場合は UTC とみなす。
func EnsureUTC(t time.Time) time.Time {
	return t.UTC()
}

// Zone はタイムゾーン名から time.Location を取得する。空文字や未知の場合は UTC またはエラーを返す。
func Zone(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("未知の時間帯: %q（例: UTC / Asia/Tokyo / America/New_York）: %w", name, err)
	}
	return loc, nil
}

// MustZone はタイムゾーン名から time.Location を取得し、失敗時は panic する。
func MustZone(name string) *time.Location {
	loc, err := Zone(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// ToZone は時刻を指定したタイムゾーンに変換する。
func ToZone(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc)
}

// Fmt は時間帯の略号を添えてフォーマットする（例: "2026-08-29 15:20 JST" / "2026-08-29 06:20 UTC"）。
func Fmt(t time.Time, loc *time.Location, withSeconds bool) string {
	local := ToZone(t, loc)
	layout := "2006-01-02 15:04 MST"
	if withSeconds {
		layout = "2006-01-02 15:04:05 MST"
	}
	return local.Format(layout)
}

// FmtTime は時刻のみフォーマットする（例: "15:20 JST"）。
func FmtTime(t time.Time, loc *time.Location) string {
	local := ToZone(t, loc)
	return local.Format("15:04 MST")
}

// FmtISO は DB 等に保存されている ISO 8601 文字列（UTC）を表示用の指定タイムゾーン文字列に変換する。
// 日付のみ ("2026-08-29") の場合はそのまま返す。
func FmtISO(isoStr string, loc *time.Location) string {
	if !strings.Contains(isoStr, "T") && !strings.Contains(isoStr, " ") {
		return isoStr
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.Replace(isoStr, " ", "T", 1))
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, isoStr)
		if err != nil {
			return isoStr
		}
	}
	return Fmt(parsed, loc, true)
}

// StampISO はログ用の現在時刻文字列（オフセット付き ISO 8601、マイクロ秒精度）を返す。
func StampISO(loc *time.Location) string {
	local := ToZone(NowUTC(), loc)
	return local.Format("2006-01-02T15:04:05.000000-07:00")
}
