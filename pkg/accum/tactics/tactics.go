package tactics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/window"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
)

type Tactic interface {
	Name() string
	Describe() string
	WarmupBars() int
	Multipliers(bars []domain.Bar) []float64
	// Window はこの戦略の発注時間帯。
	Window() window.TradingWindow
	// AllowsOrder はその時刻に発注してよいか。
	AllowsOrder(moment time.Time) bool
}

// Base は全戦略が持つ発注時間帯。埋め込んで使う。
//
// ゼロ値は「未設定」であり既定の 14:00〜15:00 として扱う。ここを
// window.TradingWindow の値型にすると、ゼロ値の Enabled=false が
// 「制限なし」を意味してしまい、設定し忘れが時間帯ガードの無効化として
// 現れる。安全側に倒すためポインタで持つ。
type Base struct {
	w *window.TradingWindow
}

// Window はこの戦略の発注時間帯。未設定なら既定を返す。
func (b *Base) Window() window.TradingWindow {
	if b.w == nil {
		return window.Default()
	}
	return *b.w
}

// SetWindow は発注時間帯を差し替える。
func (b *Base) SetWindow(w window.TradingWindow) {
	b.w = &w
}

// AllowsOrder はその時刻に発注してよいか。
func (b *Base) AllowsOrder(moment time.Time) bool {
	return b.Window().Allows(moment)
}

// Constant は定額（ドル平均法）。常に 1.0 倍。
type Constant struct{ Base }

func (c *Constant) Name() string {
	return "constant"
}

func (c *Constant) Describe() string {
	return "constant"
}

func (c *Constant) WarmupBars() int {
	return 1
}

func (c *Constant) Multipliers(bars []domain.Bar) []float64 {
	res := make([]float64, len(bars))
	for i := range res {
		res[i] = 1.0
	}
	return res
}

// BearStack は完全下降配列（終値 < MA20 < MA50 < MA200）で増額する。
type BearStack struct {
	Base
	Value float64
	Fast  int
	Mid   int
	Slow  int
}

func NewBearStack(multiplier float64, fast, mid, slow int) (*BearStack, error) {
	if multiplier <= 0 {
		multiplier = 4.0
	}
	fast, mid, slow = defaultPeriods(fast, mid, slow)
	if err := checkMultiplier(multiplier); err != nil {
		return nil, err
	}
	if err := checkPeriods(fast, mid, slow); err != nil {
		return nil, err
	}
	return &BearStack{Value: multiplier, Fast: fast, Mid: mid, Slow: slow}, nil
}

func (b *BearStack) Name() string {
	return "bear_stack"
}

func (b *BearStack) Describe() string {
	return fmt.Sprintf("bear_stack(×%g, %d/%d/%d)", b.Value, b.Fast, b.Mid, b.Slow)
}

func (b *BearStack) WarmupBars() int {
	return b.Slow
}

func (b *BearStack) Multipliers(bars []domain.Bar) []float64 {
	n := len(bars)
	res := make([]float64, n)
	for i := range res {
		res[i] = 1.0
	}
	if n < b.Slow {
		return res
	}

	closes := make([]float64, n)
	for i, bar := range bars {
		c, _ := bar.Close.Float64()
		closes[i] = c
	}

	smaFast, _ := indicators.SMA(closes, b.Fast)
	smaMid, _ := indicators.SMA(closes, b.Mid)
	smaSlow, _ := indicators.SMA(closes, b.Slow)

	for i := b.Slow - 1; i < n; i++ {
		p := closes[i]
		f := smaFast[i]
		m := smaMid[i]
		s := smaSlow[i]

		if p < f && f < m && m < s {
			res[i] = b.Value
		}
	}

	return res
}

// StackLadder は弱気スコア（0〜6）に応じて段階的に増額する。
type StackLadder struct {
	Base
	Table map[int]float64
	Fast  int
	Mid   int
	Slow  int
}

func NewStackLadder(table map[int]float64, fast, mid, slow int) (*StackLadder, error) {
	if len(table) == 0 {
		table = map[int]float64{3: 1.5, 5: 2.0, 6: 4.0}
	}
	fast, mid, slow = defaultPeriods(fast, mid, slow)
	if err := checkPeriods(fast, mid, slow); err != nil {
		return nil, err
	}

	scores := make([]int, 0, len(table))
	for score, value := range table {
		if score < 0 || score > 6 {
			return nil, fmt.Errorf("弱気スコアは 0〜6: %d", score)
		}
		if err := checkMultiplier(value); err != nil {
			return nil, err
		}
		scores = append(scores, score)
	}
	sort.Ints(scores)
	ordered := make([]float64, len(scores))
	for i, sc := range scores {
		ordered[i] = table[sc]
	}
	if err := checkMonotone(ordered, "弱気スコアが大きいほど倍率も大きく"); err != nil {
		return nil, err
	}

	return &StackLadder{Table: table, Fast: fast, Mid: mid, Slow: slow}, nil
}

func (s *StackLadder) Name() string {
	return "stack_ladder"
}

func (s *StackLadder) Describe() string {
	scores := make([]int, 0, len(s.Table))
	for score := range s.Table {
		scores = append(scores, score)
	}
	sort.Ints(scores)
	rungs := make([]string, 0, len(scores))
	for _, score := range scores {
		rungs = append(rungs, fmt.Sprintf("%d→×%s", score, trimFloat(s.Table[score])))
	}
	return fmt.Sprintf("stack_ladder(%s)", strings.Join(rungs, ", "))
}

func (s *StackLadder) WarmupBars() int {
	return s.Slow
}

func (s *StackLadder) Multipliers(bars []domain.Bar) []float64 {
	n := len(bars)
	res := make([]float64, n)
	for i := range res {
		res[i] = 1.0
	}
	if n < s.Slow {
		return res
	}

	closes := make([]float64, n)
	for i, bar := range bars {
		c, _ := bar.Close.Float64()
		closes[i] = c
	}

	smaFast, _ := indicators.SMA(closes, s.Fast)
	smaMid, _ := indicators.SMA(closes, s.Mid)
	smaSlow, _ := indicators.SMA(closes, s.Slow)

	// ソートされた閾値
	var thresholds []int
	for th := range s.Table {
		thresholds = append(thresholds, th)
	}
	sort.Ints(thresholds)

	for i := s.Slow - 1; i < n; i++ {
		p := closes[i]
		f := smaFast[i]
		m := smaMid[i]
		sl := smaSlow[i]

		score := 0
		if p < f {
			score++
		}
		if p < m {
			score++
		}
		if p < sl {
			score++
		}
		if f < m {
			score++
		}
		if f < sl {
			score++
		}
		if m < sl {
			score++
		}

		mult := 1.0
		for _, th := range thresholds {
			if score >= th {
				mult = s.Table[th]
			}
		}
		res[i] = mult
	}

	return res
}

// DrawdownLadder は過去最高値からの下落率に応じて段階的に増額する。
type DrawdownLadder struct {
	Base
	Levels           []float64
	Values           []float64
	RequireDowntrend bool
	Slow             int
}

func NewDrawdownLadder(levels, values []float64, requireDowntrend bool, slow int) (*DrawdownLadder, error) {
	if len(levels) == 0 && len(values) == 0 {
		levels = []float64{0.10, 0.20, 0.30}
		values = []float64{2.0, 3.0, 4.0}
	}
	if slow <= 0 {
		slow = 200
	}
	if len(levels) != len(values) {
		return nil, fmt.Errorf("levels と multipliers の長さが違います: %d, %d", len(levels), len(values))
	}
	if len(levels) == 0 {
		return nil, fmt.Errorf("levels が空です")
	}
	if !sort.Float64sAreSorted(levels) {
		return nil, fmt.Errorf("levels は浅い順に並べてください: %v", levels)
	}
	for _, level := range levels {
		if level <= 0 || level >= 1 {
			return nil, fmt.Errorf("下落率は 0〜1 の割合で指定します: %g", level)
		}
	}
	for _, value := range values {
		if err := checkMultiplier(value); err != nil {
			return nil, err
		}
	}
	if err := checkMonotone(values, "下落が深いほど倍率も大きく"); err != nil {
		return nil, err
	}

	return &DrawdownLadder{
		Levels:           levels,
		Values:           values,
		RequireDowntrend: requireDowntrend,
		Slow:             slow,
	}, nil
}

func (d *DrawdownLadder) Name() string {
	return "drawdown_ladder"
}

func (d *DrawdownLadder) Describe() string {
	rungs := make([]string, 0, len(d.Levels))
	for i, level := range d.Levels {
		rungs = append(rungs, fmt.Sprintf("-%.0f%%→×%s", level*100, trimFloat(d.Values[i])))
	}
	gate := ""
	if d.RequireDowntrend {
		gate = "・200日線割れ時のみ"
	}
	return fmt.Sprintf("drawdown_ladder(%s%s)", strings.Join(rungs, ", "), gate)
}

func (d *DrawdownLadder) WarmupBars() int {
	if d.RequireDowntrend {
		return d.Slow
	}
	return 1
}

func (d *DrawdownLadder) Multipliers(bars []domain.Bar) []float64 {
	n := len(bars)
	res := make([]float64, n)
	for i := range res {
		res[i] = 1.0
	}
	if n == 0 {
		return res
	}

	closes := make([]float64, n)
	for i, bar := range bars {
		c, _ := bar.Close.Float64()
		closes[i] = c
	}

	var smaSlow []float64
	if d.RequireDowntrend {
		smaSlow, _ = indicators.SMA(closes, d.Slow)
	}

	cumMax := closes[0]
	for i := 0; i < n; i++ {
		p := closes[i]
		if p > cumMax {
			cumMax = p
		}
		dd := (p - cumMax) / cumMax // <= 0

		gate := true
		if d.RequireDowntrend {
			if i < d.Slow-1 || math.IsNaN(smaSlow[i]) || p >= smaSlow[i] {
				gate = false
			}
		}

		if !gate {
			continue
		}

		mult := 1.0
		for idx, level := range d.Levels {
			if dd <= -level {
				if idx < len(d.Values) && d.Values[idx] > mult {
					mult = d.Values[idx]
				}
			}
		}
		res[i] = mult
	}

	return res
}

// defaultPeriods は未指定（0以下）の移動平均期間に既定値を入れる。
func defaultPeriods(fast, mid, slow int) (int, int, int) {
	if fast <= 0 {
		fast = 20
	}
	if mid <= 0 {
		mid = 50
	}
	if slow <= 0 {
		slow = 200
	}
	return fast, mid, slow
}

// checkPeriods は移動平均が短期 < 中期 < 長期の順であることを確かめる。
//
// 順序が崩れると「完全下降配列」の判定が意味を失い、常時発動や無発動に
// なる。設定の書き間違いを発注前に弾く。
func checkPeriods(fast, mid, slow int) error {
	if !(fast < mid && mid < slow) {
		return fmt.Errorf("移動平均は短期 < 中期 < 長期の順に指定してください: %d/%d/%d", fast, mid, slow)
	}
	return nil
}

// checkMultiplier は倍率が 1.0 以上であることを確かめる。
//
// 積立は「増額する」機構であり減額はしない。1.0 未満は設定の誤りとみなす。
func checkMultiplier(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("倍率が数値ではありません: %v", value)
	}
	if value < 1.0 {
		return fmt.Errorf("倍率は 1.0 以上で指定してください（減額はしない）: %g", value)
	}
	return nil
}

// checkMonotone は倍率が単調非減少であることを確かめる。
func checkMonotone(values []float64, label string) error {
	for i := 1; i < len(values); i++ {
		if values[i] < values[i-1] {
			return fmt.Errorf("%s指定してください: %v", label, values)
		}
	}
	return nil
}

// trimFloat は 4 を "4"、1.5 を "1.5" のように余分な 0 を落として書く。
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
