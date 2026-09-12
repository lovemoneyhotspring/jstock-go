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
	// Sector は 33 業種コード（equities/master の S33）。同じ業種に建玉を偏らせない
	// 判定（signal.max_per_sector）に使う。取れなければ空。
	Sector string
	// MarginRatio は信用倍率（買残 ÷ 売残。週末残高の最新）。**記録だけで選定には使わない**
	// （研究ノート 2026-09-jp-gap-minute の発見 6）。残高の報告が無い銘柄は nil。
	MarginRatio *float64
	// ShortInterest は空売り残高（markets/short-sale-report の ShrtPosToSO を銘柄ごとに
	// 合計した比）。報告の無い銘柄は nil（＝重い残高が無い。報告義務は 0.5% 以上）。
	// ショートの母集団の条件（config.Margin.MaxShortInterest）に使う。
	ShortInterest *float64
	// EarnYield は益回り（直近の本決算の当期純利益 ÷ 前日の時価総額）。本決算が
	// 見つからない銘柄は nil。**選定の 2 段階目**（config.Signal.ValuePool）で使う。
	EarnYield *float64
	// Loss は直近の本決算が赤字（当期純利益 ≤ 0）。EarnYield が nil なら偽
	// （判定できない銘柄を赤字扱いにして落とさない）。
	Loss bool
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
func Eligible(c Candidate, cfg config.Universe) bool { return NewFilter(cfg).Match(c) }

// ShortEligible はショート（信用新規売り）の母集団。Eligible と同じ列に加えて
// 貸借銘柄と売り禁を見る。ロングとは区分・分位・規制の扱いが違う（[margin] の各項目）。
func ShortEligible(c Candidate, m config.Margin) bool { return NewShortFilter(m).Match(c) }

// Filter はロングの母集団の条件を 1 度だけ展開したもの。
//
// 条件そのものは Match にしかない（Eligible もこれを呼ぶ）。展開して持つのは、
// バックテストが同じ条件を 500 万行に当てるため——decimal から float への変換を
// 行ごとに払うと 1 本で数秒になる。
type Filter struct {
	segments             []string
	minTurnover          float64
	excludeCapTerciles   int
	excludeEarningsPrev  bool
	excludeEarningsToday bool
	excludeMarginAlert   bool
	excludeLoss          bool
}

// NewFilter はロングの母集団の判定器。
func NewFilter(cfg config.Universe) Filter {
	minTurnover, _ := cfg.MinTurnover.Float64()
	return Filter{
		segments: cfg.Segments, minTurnover: minTurnover,
		excludeCapTerciles:   cfg.ExcludeCapTerciles,
		excludeEarningsPrev:  cfg.ExcludeEarningsPrev,
		excludeEarningsToday: cfg.ExcludeEarningsToday,
		excludeMarginAlert:   cfg.ExcludeMarginAlert,
		excludeLoss:          cfg.ExcludeLoss,
	}
}

// Match は候補がロングの母集団に入るか。
func (f Filter) Match(c Candidate) bool {
	if !contains(f.segments, c.Segment) {
		return false
	}
	if c.TurnoverMed < f.minTurnover {
		return false
	}
	if f.excludeCapTerciles > 0 && c.CapTercile <= f.excludeCapTerciles {
		return false
	}
	if f.excludeEarningsPrev && c.EarnPrev {
		return false
	}
	if f.excludeEarningsToday && c.DiscToday {
		return false
	}
	if f.excludeMarginAlert && c.Alert {
		return false
	}
	if f.excludeLoss && c.Loss {
		return false
	}
	return true
}

// ShortFilter はショートの母集団の条件を 1 度だけ展開したもの（Filter の鏡像）。
type ShortFilter struct {
	enabled              bool
	segments             []string
	minTurnover          float64
	excludeCapTerciles   int
	excludeEarningsPrev  bool
	excludeEarningsToday bool
	excludeMarginAlert   bool
	excludeJsfStop       bool
	// maxShortInterest は空売り残高の上限（0 なら上限なし）。
	maxShortInterest float64
}

// NewShortFilter はショートの母集団の判定器。
func NewShortFilter(m config.Margin) ShortFilter {
	minTurnover, _ := m.MinTurnover.Float64()
	maxSI := 0.0
	if m.MaxShortInterest.IsPositive() {
		maxSI, _ = m.MaxShortInterest.Float64()
	}
	return ShortFilter{
		enabled: m.Enabled, segments: m.Segments, minTurnover: minTurnover,
		excludeCapTerciles:   m.ExcludeCapTerciles,
		excludeEarningsPrev:  m.ExcludeEarningsPrev,
		excludeEarningsToday: m.ExcludeEarningsToday,
		excludeMarginAlert:   m.ExcludeMarginAlert,
		excludeJsfStop:       m.ExcludeJsfStop,
		maxShortInterest:     maxSI,
	}
}

// Match は候補がショートの母集団に入るか。
func (f ShortFilter) Match(c Candidate) bool {
	if !f.enabled || !c.Shortable {
		return false
	}
	if !contains(f.segments, c.Segment) {
		return false
	}
	if c.TurnoverMed < f.minTurnover {
		return false
	}
	if f.excludeCapTerciles > 0 && c.CapTercile <= f.excludeCapTerciles {
		return false
	}
	if f.excludeEarningsPrev && c.EarnPrev {
		return false
	}
	if f.excludeEarningsToday && c.DiscToday {
		return false
	}
	if f.excludeMarginAlert && c.Alert {
		return false
	}
	if f.excludeJsfStop && c.JsfStop {
		return false
	}
	// 空売り残高が重い銘柄は踏み上げの燃料を抱えている（張り付き率が 2 倍・寄→引も不利）。
	// 報告の無い銘柄（nil）は 0 として通す——報告義務は 0.5% 以上なので、無い＝軽い。
	if f.maxShortInterest > 0 && c.ShortInterest != nil && *c.ShortInterest > f.maxShortInterest {
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
