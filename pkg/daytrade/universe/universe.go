// Package universe は母集団（前夜に確定する条件）。J-Quants のアーカイブから
// 「明日買ってよい銘柄」を絞る。
//
// ここにあるのは条件の式と純粋関数だけ。同じ条件を前夜の plan（1 日ぶん）と
// backtest（10 年ぶんのパネル）で使う——検証と実運用で条件がずれないために。
//
// 入力の列名は J-Quants V2 の生の列名（Code / C / Va / MktCap / MktNm …）。
package universe

import (
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
)

// CloseChangedOn は東証の引け時刻が 15:00 → 15:30 に変わった日。
// 決算開示が「引け後」かの判定に使う。
var CloseChangedOn = time.Date(2024, 11, 5, 0, 0, 0, 0, time.UTC)

// StockProduct は J-Quants の ProdCat。011 が株式（ETF・REIT・優先株等を除く）。
const StockProduct = "011"

// VolDays は銘柄ごとのボラティリティ（日次リターンの標準偏差）を取る日数。配分の重みに使う。
const VolDays = 20

// Candidate は母集団の 1 銘柄。条件に合わない行も残す（なぜ外れたかを見せるため）。
type Candidate struct {
	Code        string
	Symbol      string
	Name        string
	Segment     string
	PrevClose   float64
	TurnoverMed float64
	MktCap      float64
	// Vol20 は 20 日の日次ボラ（取れなければ nil）。
	Vol20 *float64
	// CapTercile は時価総額の 3 分位（1=下位、3=上位）。母集団の外は 0。
	CapTercile int
	// EarnPrev は前日引け後に決算短信を開示した。
	EarnPrev bool
	// DiscToday は当日に決算発表の予定がある。
	DiscToday bool
	// Alert は前日に日々公表・注意喚起・増担保の対象だった。
	Alert bool
	// JsfStop は日証金の申込停止（売り禁）。
	JsfStop bool
	// Shortable は貸借銘柄（制度信用で新規売りができる）。
	Shortable bool
	// Eligible が真ならロングの対象、ShortEligible が真ならショートの対象。
	Eligible      bool
	ShortEligible bool
}

// ToBrokerSymbol は J-Quants の 5 桁コード（72030 / 130A0）を
// 発注用の表記（7203 / 130A）にする。
func ToBrokerSymbol(code string) string {
	code = strings.TrimSpace(code)
	if len(code) == 5 && strings.HasSuffix(code, "0") {
		return code[:4]
	}
	return code
}

// SegmentOf は市場区分名を prime / standard / growth / other に畳む。
//
// 2022-04 の再編前（東証一部・二部・マザーズ・JASDAQ）も同じ呼び方に寄せる。
// 「JASDAQ グロース」はグロース扱い（先に判定する）。
func SegmentOf(name string) string {
	switch {
	case strings.Contains(name, "プライム"), strings.Contains(name, "一部"):
		return "prime"
	case strings.Contains(name, "グロース"), strings.Contains(name, "マザーズ"):
		return "growth"
	case strings.Contains(name, "スタンダード"), strings.Contains(name, "二部"),
		strings.Contains(name, "JASDAQ"):
		return "standard"
	default:
		return "other"
	}
}

// IsShortable は equities/master の Mrgn（1=信用 2=貸借 3=その他）が貸借か。
func IsShortable(mrgn string) bool { return strings.TrimSpace(mrgn) == "2" }

// IsJsfStop は markets/margin-alert の PubReason（JSON 文字列）が
// 日証金の申込停止（売り禁）を立てているか。新規売りが出せない。
func IsJsfStop(pubReason string) bool {
	return strings.Contains(pubReason, `"RestrictedByJSF": "1"`)
}

// IsPostClose は決算開示が引け後か。引け時刻は 2024-11-05 から 15:30。
func IsPostClose(discDate time.Time, discTime string) bool {
	closeAt := "15:30"
	if discDate.Before(CloseChangedOn) {
		closeAt = "15:00"
	}
	if len(discTime) < 5 {
		return false
	}
	return discTime[:5] >= closeAt
}

// Eligible はロングの母集団の条件。
func Eligible(c Candidate, cfg config.Universe) bool {
	if !contains(cfg.Segments, c.Segment) {
		return false
	}
	minTurnover, _ := cfg.MinTurnover.Float64()
	if c.TurnoverMed < minTurnover {
		return false
	}
	if cfg.ExcludeCapTerciles > 0 && c.CapTercile <= cfg.ExcludeCapTerciles {
		return false
	}
	if cfg.ExcludeEarningsPrev && c.EarnPrev {
		return false
	}
	if cfg.ExcludeEarningsToday && c.DiscToday {
		return false
	}
	if cfg.ExcludeMarginAlert && c.Alert {
		return false
	}
	return true
}

// ShortEligible はショート（信用新規売り）の母集団。Eligible と同じ列に加えて
// 貸借銘柄と売り禁を見る。ロングとは区分・分位・規制の扱いが違う（[margin] の各項目）。
func ShortEligible(c Candidate, m config.Margin) bool {
	if !m.Enabled {
		return false
	}
	if !c.Shortable {
		return false
	}
	if !contains(m.Segments, c.Segment) {
		return false
	}
	minTurnover, _ := m.MinTurnover.Float64()
	if c.TurnoverMed < minTurnover {
		return false
	}
	if m.ExcludeCapTerciles > 0 && c.CapTercile <= m.ExcludeCapTerciles {
		return false
	}
	if m.ExcludeEarningsPrev && c.EarnPrev {
		return false
	}
	if m.ExcludeEarningsToday && c.DiscToday {
		return false
	}
	if m.ExcludeMarginAlert && c.Alert {
		return false
	}
	if m.ExcludeJsfStop && c.JsfStop {
		return false
	}
	return true
}

// CapTerciles は時価総額の 3 分位（1=下位、3=上位）を割り当てる。
//
// mask が真の行だけを母集団にして順位を付ける（偽の行は 0）。順位も件数も母集団の中で
// 数えないと、流動性の無い小型株が下位を埋めて分位が上に偏る。
func CapTerciles(values []float64, mask []bool) []int {
	type entry struct {
		index int
		value float64
	}
	var pool []entry
	for i, ok := range mask {
		if ok {
			pool = append(pool, entry{i, values[i]})
		}
	}
	out := make([]int, len(values))
	n := len(pool)
	if n == 0 {
		return out
	}
	// 順位は昇順の ordinal（同値は入力順）。polars の rank("ordinal") と同じ。
	sortStable(pool, func(a, b entry) bool { return a.value < b.value })
	for rank, e := range pool {
		tercile := ceilDiv((rank+1)*3, n)
		out[e.index] = clamp(tercile, 1, 3)
	}
	return out
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func ceilDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
