// Package window は発注を許す時間帯を扱う。
//
// # これは投下額を変えない
//
// 積立の計画は日足で決まる（pkg/accum/plan）。この時間帯は「その日ぶんの
// 注文を、いつ出してよいか」だけを制御する。したがって simulate の結果は
// 時間帯を変えても変わらない。日足では時間内の値動きを再現できないため、
// 変わったように見せる方が嘘になる。
//
// # なぜ既定を 14:00〜15:00 にしているか
//
// 60分足3年ぶんの実測では、時間帯による平均取得単価の差は ±0.03% しか
// なく、どの時間帯が安いという傾向は無かった（大引けより安く買えた日の
// 割合はどの時間帯も50〜54%）。一方で大引けに対するぶれは時間とともに
// 単調に小さくなり、寄り付き直後は14時台の約3倍ある。
//
//	09:00 の標準偏差 0.687% ／ 平均値幅 0.778%
//	14:00 の標準偏差 0.236% ／ 平均値幅 0.329%
//
// つまり14時台を選ぶ理由は「安く買えるから」ではなく「想定と大きく違う
// 値段を掴みにくいから」。期待値の改善は無い。
package window

import (
	"fmt"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// TimeOfDay は時刻（時・分）。東証の立会時間に対する規則なので秒は持たない。
type TimeOfDay struct {
	Hour   int
	Minute int
}

func (t TimeOfDay) minutes() int { return t.Hour*60 + t.Minute }

func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// Before は t が other より前かどうか。
func (t TimeOfDay) Before(other TimeOfDay) bool { return t.minutes() < other.minutes() }

// session は立会時間の一区切り。
type session struct {
	start TimeOfDay
	end   TimeOfDay
}

// sessions は東証の立会時間（2024年11月5日の延長後。大引けは15:30）。
var sessions = []session{
	{TimeOfDay{9, 0}, TimeOfDay{11, 30}},
	{TimeOfDay{12, 30}, TimeOfDay{15, 30}},
}

// 既定の発注時間帯。
var (
	DefaultStart = TimeOfDay{14, 0}
	DefaultEnd   = TimeOfDay{15, 0}
)

// TradingWindow は発注を許す時間帯（日本時間 JST）。
//
// Start は含み、End は含まない。Enabled が false なら時間帯を制限しない。
type TradingWindow struct {
	Start   TimeOfDay
	End     TimeOfDay
	Enabled bool
}

// Default は既定の発注時間帯（14:00〜15:00 JST）を返す。
func Default() TradingWindow {
	return TradingWindow{Start: DefaultStart, End: DefaultEnd, Enabled: true}
}

// Unrestricted は制限なしの時間帯を返す。
func Unrestricted() TradingWindow {
	return TradingWindow{Start: DefaultStart, End: DefaultEnd, Enabled: false}
}

// Validate は時間帯が立会時間に収まっているかを確かめる。
func (w TradingWindow) Validate() error {
	if !w.Enabled {
		return nil
	}
	if !w.Start.Before(w.End) {
		return fmt.Errorf("開始が終了以降になっています: %s〜%s", w.Start, w.End)
	}
	inStart := false
	for _, s := range sessions {
		if !w.Start.Before(s.start) && w.Start.Before(s.end) {
			inStart = true
			break
		}
	}
	if !inStart {
		return fmt.Errorf("開始 %s が立会時間外です（前場 09:00〜11:30 / 後場 12:30〜15:30）", w.Start)
	}
	inEnd := false
	for _, s := range sessions {
		if s.start.Before(w.End) && !s.end.Before(w.End) {
			inEnd = true
			break
		}
	}
	if !inEnd {
		return fmt.Errorf("終了 %s が立会時間外です（前場 09:00〜11:30 / 後場 12:30〜15:30）", w.End)
	}
	return nil
}

// Allows はその時刻に発注してよいか。時間帯を制限しているときは土日も不可。
//
// 祝日は時計だけでは分からないので判定しない。祝日に送った注文はブローカーが
// 拒否し（REJECTED）、翌営業日に同じ判断で出し直される。Enabled=false は
// 文字どおり制限なし（土日も含む。検証・手動用）。
func (w TradingWindow) Allows(moment time.Time) bool {
	if !w.Enabled {
		return true
	}
	jst := clock.ToZone(moment, clock.Tokyo)
	if wd := jst.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	cur := TimeOfDay{jst.Hour(), jst.Minute()}
	return !cur.Before(w.Start) && cur.Before(w.End)
}

// NextOpen は次に発注できるようになる日時（JST）。今が時間内ならその時刻自身。
//
// 土日と祝日は考慮しない。休場日かどうかは足データが持っている情報であり、
// 時計だけでは判断できないため。
func (w TradingWindow) NextOpen(moment time.Time) time.Time {
	jst := clock.ToZone(moment, clock.Tokyo)
	if !w.Enabled || w.Allows(moment) {
		return jst
	}
	today := time.Date(jst.Year(), jst.Month(), jst.Day(), w.Start.Hour, w.Start.Minute, 0, 0, jst.Location())
	if jst.Before(today) {
		return today
	}
	return today.AddDate(0, 0, 1)
}

// Describe は人が読む形の説明を返す。
func (w TradingWindow) Describe() string {
	if !w.Enabled {
		return "制限なし"
	}
	return fmt.Sprintf("%s〜%s JST", w.Start, w.End)
}

// Parse は設定ファイルの記述から組み立てる。
//
// 受け付ける形:
//
//	（省略 / nil）                          → 既定の 14:00〜15:00
//	window = false                          → 制限なし
//	window = { start = "14:00", end = "15:00" }
func Parse(value any) (TradingWindow, error) {
	switch v := value.(type) {
	case nil:
		return Default(), nil
	case TradingWindow:
		return v, nil
	case bool:
		w := Default()
		w.Enabled = v
		return w, nil
	case map[string]any:
		return parseMap(v)
	default:
		return TradingWindow{}, fmt.Errorf(
			`window は false か { start = "14:00", end = "15:00" } の形で指定してください: %v`, value)
	}
}

func parseMap(m map[string]any) (TradingWindow, error) {
	var unknown []string
	for k := range m {
		if k != "start" && k != "end" && k != "enabled" {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return TradingWindow{}, fmt.Errorf("window に未知のキーがあります: %v", unknown)
	}

	w := Default()
	if raw, ok := m["start"]; ok {
		t, err := parseTime(raw, "start")
		if err != nil {
			return TradingWindow{}, err
		}
		w.Start = t
	}
	if raw, ok := m["end"]; ok {
		t, err := parseTime(raw, "end")
		if err != nil {
			return TradingWindow{}, err
		}
		w.End = t
	}
	if raw, ok := m["enabled"]; ok {
		b, ok := raw.(bool)
		if !ok {
			return TradingWindow{}, fmt.Errorf("window.enabled は真偽値で指定してください: %v", raw)
		}
		w.Enabled = b
	}
	if err := w.Validate(); err != nil {
		return TradingWindow{}, err
	}
	return w, nil
}

func parseTime(value any, field string) (TimeOfDay, error) {
	s, ok := value.(string)
	if !ok {
		return TimeOfDay{}, fmt.Errorf("window.%s は文字列で指定してください: %v", field, value)
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return TimeOfDay{}, fmt.Errorf("window.%s は HH:MM 形式で指定してください: %q", field, s)
	}
	return TimeOfDay{t.Hour(), t.Minute()}, nil
}
