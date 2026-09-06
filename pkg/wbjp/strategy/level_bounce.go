package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// LevelBounce は「みんなが見ている節目」での反発を拾う押し目買い戦略。
//
// 前提にしているのは価格の予知ではなく参加者の心理。直近の上昇波（安値→高値）の
// フィボナッチ比率（38.2 / 50 / 61.8%）やキリ番は、そこに指値・逆指値・「戻ったら
// 買う」の注文が集まるから機能する（自己成就）。ただし節目そのものは無数にあるので、
// 節目に「触れた」だけでは買わない。買うのは
//
//   - 上昇トレンドの中の押し目で（終値 > 長期線、上昇波の幅が十分）、
//   - 押し目の安値が節目に届き（許容幅は ATR で測る）、
//   - その節目で買い手が実際に現れた（陽線・高値圏引け・出来高増）
//
// ことを足で確認できた日。節目の下（押し目安値の少し下）に損切りを置けるので
// 損失は小さく、目標は直前の高値（そこで「やれやれ売り」の壁に当たる）と決まる。
// 入口の時点で損益比が読めるのがこの手法の本当の価値で、min_reward_risk で足切りする。
//
// 出口（戦略側）: 前回高値に到達（目標達成）／押し目安値割れ（節目が否定された）。
// 時間切れと ATR ストップはエンジン側（[stops]）に任せる。
type LevelBounce struct {
	SwingLookback   int
	MinImpulse      float64
	MinPullbackDays int
	Levels          []float64
	RoundNumbers    bool
	ToleranceATR    float64
	BounceWindow    int
	RVOLMin         float64
	VolumeLookback  int
	CloseStrength   float64
	MinRewardRisk   float64
	SMALong         int
	ATRPeriod       int
	MinATRRatio     float64
	MaxATRRatio     float64
	MinDollarVolume float64
	Benchmark       string
	BenchmarkSMA    int
	ExitOnTarget    bool
	ExitOnBreak     bool

	warmup int
}

// LevelBounceOptions は LevelBounce の設定。
type LevelBounceOptions struct {
	// SwingLookback は上昇波（安値→高値）を探す日数。当日は含めない。
	SwingLookback int
	// MinImpulse は上昇波の最小幅（安値比）。小さい波の節目は誰も見ていない。
	MinImpulse float64
	// MinPullbackDays は高値から当日までの最小日数。高値の翌日の反発は押し目ではない。
	MinPullbackDays int
	// Levels は上昇波に対するリトレースメント比率。
	Levels []float64
	// RoundNumbers はキリ番（価格の桁に応じた 1,000 円・500 円等）も節目に加えるか。
	RoundNumbers bool
	// ToleranceATR は節目に「届いた」とみなす許容幅（ATR 倍率）。
	ToleranceATR float64
	// BounceWindow は押し目安値が当日から何日以内であれば「反発したて」とみなすか。
	BounceWindow int
	// RVOLMin は反発日の相対出来高（当日 ÷ 前日までの平均）の下限。
	RVOLMin float64
	// VolumeLookback は平均出来高・売買代金の日数。
	VolumeLookback int
	// CloseStrength は反発日の終値位置 (close-low)/(high-low) の下限。
	CloseStrength float64
	// MinRewardRisk は（前回高値まで）÷（押し目安値の許容幅下まで）の下限。
	MinRewardRisk float64
	// SMALong はトレンドフィルタの長期線。終値がこれより上でだけ買う。
	SMALong     int
	ATRPeriod   int
	MinATRRatio float64
	MaxATRRatio float64
	// MinDollarVolume は平均売買代金の下限（口座通貨）。
	MinDollarVolume float64
	// Benchmark は地合いフィルタの指数（空で無効）。
	Benchmark    string
	BenchmarkSMA int
	// ExitOnTarget は前回高値到達で手仕舞うか。
	ExitOnTarget bool
	// ExitOnBreak は押し目安値（許容幅の下）を終値で割ったら手仕舞うか。
	ExitOnBreak bool
}

// DefaultLevelBounceOptions は日本株の日足を想定した既定値。
func DefaultLevelBounceOptions() LevelBounceOptions {
	return LevelBounceOptions{
		SwingLookback: 60, MinImpulse: 0.15, MinPullbackDays: 3,
		Levels: []float64{0.382, 0.5, 0.618}, RoundNumbers: true,
		ToleranceATR: 0.5, BounceWindow: 2,
		RVOLMin: 1.2, VolumeLookback: 20, CloseStrength: 0.6,
		MinRewardRisk: 1.5,
		SMALong:       200, ATRPeriod: 14, MinATRRatio: 0.01, MaxATRRatio: 0.06,
		MinDollarVolume: 300_000_000,
		Benchmark:       "", BenchmarkSMA: 50,
		ExitOnTarget: true, ExitOnBreak: true,
	}
}

// NewLevelBounce は節目反発戦略を作る。
func NewLevelBounce(o LevelBounceOptions) (*LevelBounce, error) {
	if o.SwingLookback < 10 {
		return nil, fmt.Errorf("swing_lookback は 10 以上: %d", o.SwingLookback)
	}
	if !(0 < o.MinImpulse && o.MinImpulse < 1) {
		return nil, fmt.Errorf("0 < min_impulse < 1 を満たすこと: %g", o.MinImpulse)
	}
	if len(o.Levels) == 0 {
		return nil, fmt.Errorf("levels を 1 つ以上指定すること")
	}
	for _, r := range o.Levels {
		if !(0 < r && r < 1) {
			return nil, fmt.Errorf("levels は 0〜1 の比率: %g", r)
		}
	}
	if o.ToleranceATR <= 0 {
		return nil, fmt.Errorf("tolerance_atr は正の数: %g", o.ToleranceATR)
	}
	if o.BounceWindow < 1 {
		return nil, fmt.Errorf("bounce_window は 1 以上: %d", o.BounceWindow)
	}
	if !(0 < o.MinATRRatio && o.MinATRRatio < o.MaxATRRatio) {
		return nil, fmt.Errorf("0 < min_atr_ratio < max_atr_ratio を満たすこと: %g, %g", o.MinATRRatio, o.MaxATRRatio)
	}
	if !(0 <= o.CloseStrength && o.CloseStrength <= 1) {
		return nil, fmt.Errorf("close_strength は 0〜1: %g", o.CloseStrength)
	}
	s := &LevelBounce{
		SwingLookback: o.SwingLookback, MinImpulse: o.MinImpulse, MinPullbackDays: o.MinPullbackDays,
		Levels: append([]float64(nil), o.Levels...), RoundNumbers: o.RoundNumbers,
		ToleranceATR: o.ToleranceATR, BounceWindow: o.BounceWindow,
		RVOLMin: o.RVOLMin, VolumeLookback: o.VolumeLookback, CloseStrength: o.CloseStrength,
		MinRewardRisk: o.MinRewardRisk, SMALong: o.SMALong,
		ATRPeriod: o.ATRPeriod, MinATRRatio: o.MinATRRatio, MaxATRRatio: o.MaxATRRatio,
		MinDollarVolume: o.MinDollarVolume, Benchmark: o.Benchmark, BenchmarkSMA: o.BenchmarkSMA,
		ExitOnTarget: o.ExitOnTarget, ExitOnBreak: o.ExitOnBreak,
	}
	s.warmup = maxInt(maxInt(o.SMALong, o.SwingLookback), maxInt(o.VolumeLookback, o.BenchmarkSMA)) + 2
	return s, nil
}

func (s *LevelBounce) Name() string    { return "level_bounce" }
func (s *LevelBounce) WarmupBars() int { return s.warmup }
func (s *LevelBounce) Describe() string {
	return fmt.Sprintf("%s(swing=%d, impulse≥%.0f%%, levels=%v, round=%t, tol=%.1fATR, rvol≥%.1f, rr≥%.1f, sma=%d)",
		s.Name(), s.SwingLookback, s.MinImpulse*100, s.Levels, s.RoundNumbers,
		s.ToleranceATR, s.RVOLMin, s.MinRewardRisk, s.SMALong)
}

// swing は当日を除く直近の上昇波と、その後の押し目。
type swing struct {
	high, low   float64 // 上昇波の高値・安値
	highIdx     int     // 高値の添字
	pullbackLow float64 // 高値の翌日から当日までの最安値
	pullbackIdx int     // その添字
}

// findSwing は直近 lookback 本（当日を除く）の最高値と、その前の最安値を上昇波とみなす。
// 高値が安値より前にある（下降波）なら ok=false。
//
// 押し目の最安値は入口では当日を含めて探す（当日の安値が節目に届いて反発した形を
// 拾うため）。出口では当日を除く——当日を含めると「終値が押し目安値を割る」ことが
// 起こり得ず（終値 ≧ 当日安値）、節目割れの手仕舞いが永久に発動しない。
func (s *LevelBounce) findSwing(v View, includeToday bool) (swing, bool) {
	n := v.Len()
	highs, lows := v.Highs(), v.Lows()
	pullbackEnd := n
	if !includeToday {
		pullbackEnd = n - 1
	}
	start := n - 1 - s.SwingLookback
	if start < 0 {
		start = 0
	}
	sw := swing{high: math.Inf(-1), highIdx: -1}
	for i := start; i < n-1; i++ {
		if highs[i] > sw.high {
			sw.high, sw.highIdx = highs[i], i
		}
	}
	if sw.highIdx < 0 {
		return swing{}, false
	}
	sw.low = math.Inf(1)
	lowIdx := -1
	for i := start; i <= sw.highIdx; i++ {
		if lows[i] < sw.low {
			sw.low, lowIdx = lows[i], i
		}
	}
	if lowIdx < 0 || lowIdx >= sw.highIdx || sw.low <= 0 {
		return swing{}, false
	}
	sw.pullbackLow = math.Inf(1)
	sw.pullbackIdx = -1
	for i := sw.highIdx + 1; i < pullbackEnd; i++ {
		if lows[i] < sw.pullbackLow {
			sw.pullbackLow, sw.pullbackIdx = lows[i], i
		}
	}
	if sw.pullbackIdx < 0 {
		return swing{}, false
	}
	return sw, true
}

// level は節目 1 つ。
type level struct {
	price float64
	kind  string // fib_382 / fib_500 / round
}

// candidateLevels は上昇波の中にある節目を並べる。
func (s *LevelBounce) candidateLevels(sw swing) []level {
	span := sw.high - sw.low
	var out []level
	for _, r := range s.Levels {
		out = append(out, level{price: sw.high - r*span, kind: fmt.Sprintf("fib_%03d", int(math.Round(r*1000)))})
	}
	if s.RoundNumbers {
		for _, p := range roundLevels(sw.low, sw.high) {
			out = append(out, level{price: p, kind: "round"})
		}
	}
	return out
}

// roundLevels は lo〜hi の間にあるキリ番。刻みは高値の桁で決め、その半分も入れる
// （高値 1,890 円なら 1,000 と 500 の倍数、980 円なら 100 と 50 の倍数）。
// 安値の桁で刻むと節目が密になりすぎ、「節目に届いた」条件が常に成立して
// フィルタとして働かなくなる。
func roundLevels(lo, hi float64) []float64 {
	if !(lo > 0 && hi > lo) {
		return nil
	}
	step := math.Pow(10, math.Floor(math.Log10(hi)))
	half := step / 2
	var out []float64
	for p := math.Ceil(lo/half) * half; p < hi; p += half {
		if p > lo {
			out = append(out, p)
		}
	}
	return out
}

// nearestLevel は押し目安値に最も近い節目。許容幅に入っていなければ ok=false。
func nearestLevel(levels []level, pullbackLow, tol float64) (level, bool) {
	best, bestDist := level{}, math.Inf(1)
	for _, l := range levels {
		d := math.Abs(pullbackLow - l.price)
		if d < bestDist {
			best, bestDist = l, d
		}
	}
	return best, bestDist <= tol
}

func (s *LevelBounce) OnBars(ctx *Context) ([]domain.Signal, error) {
	return eachSymbol(ctx, s.warmup, func(symbol string, v View) *domain.Signal {
		if symbol == s.Benchmark {
			return nil
		}
		if pos, held := ctx.Position(symbol); held && pos.Quantity.IsPositive() {
			return s.evaluateExit(ctx, symbol, v, pos)
		}
		checks := s.screen(ctx, symbol, v)
		if !checks.Passed() {
			return nil
		}
		return signal(s.Name(), symbol, scoreToDirection(checks.Score), 1.0,
			fmt.Sprintf("節目反発: %s %.1f に押し目 %.1f（戻し %.0f%%）、RVOL %.1f、損益比 %.1f、スコア %.2f",
				checks.Setup, checks.Values["level"], checks.Values["pullback_low"],
				checks.Values["depth"]*100, checks.Values["rvol"], checks.Values["reward_risk"], checks.Score),
			checks.Meta())
	}), nil
}

// evaluateExit は保有中の判断。目標到達か節目割れなら手仕舞い、それ以外は弱い買いで保有継続。
func (s *LevelBounce) evaluateExit(ctx *Context, symbol string, v View, pos domain.Position) *domain.Signal {
	closeValue := last(v.Closes())
	atrValue := last(v.ATR(s.ATRPeriod))
	prevHigh := last(v.DonchianHigh(s.SwingLookback))
	reason := ""
	switch {
	case s.ExitOnTarget && !math.IsNaN(prevHigh) && closeValue >= prevHigh:
		reason = fmt.Sprintf("直近 %d 日高値 %.1f に到達（目標）", s.SwingLookback, prevHigh)
	case s.ExitOnBreak:
		anchor, ok := s.entryAnchor(ctx, symbol, v)
		if ok && !math.IsNaN(atrValue) && closeValue < anchor-s.ToleranceATR*atrValue {
			reason = fmt.Sprintf("押し目安値 %.1f を終値で割った（節目が否定された）", anchor)
		}
	}
	if reason == "" {
		// 意見が無いと sizer が「シグナル消滅」で手仕舞うため、明示的に弱い買いを返す。
		return signal(s.Name(), symbol, 0.5, 1.0, "保有継続（前回高値への戻り待ち）", nil)
	}
	return signal(s.Name(), symbol, -1.0, 1.0, reason, map[string]any{"close": closeValue})
}

// entryAnchor は保有中の損切り基準（建てたときの押し目安値）を探す。
//
// 戦略は建玉の建て日を知らない（無状態）ので、直近の窓の中で入口条件が最後に
// 成立した日（＝反発日）を入口と同じ関数で探し直し、その日の押し目安値を返す。
// 「当日までの最安値」を基準にすると、値下がりのたびに基準が一緒にずり下がり、
// 終値が基準を割ることが起こらず、節目割れの手仕舞いが永久に発動しない。
func (s *LevelBounce) entryAnchor(ctx *Context, symbol string, v View) (float64, bool) {
	n := v.Len()
	floor := maxInt(s.warmup, n-1-s.SwingLookback)
	for k := n - 1; k >= floor; k-- {
		res := s.screen(ctx, symbol, View{series: v.series, n: k})
		if res.Passed() {
			return res.Values["pullback_low"], true
		}
	}
	return 0, false
}

// Screen は 1 銘柄のエントリー条件を 1 つずつ評価する（screen --show-failed 用）。
func (s *LevelBounce) Screen(ctx *Context, symbol string) ScreenResult {
	v, ok := ctx.Bars(symbol)
	if !ok || v.Len() < s.warmup {
		return ScreenResult{Symbol: symbol, Failed: []string{"足の本数がウォームアップに足りない"}}
	}
	return s.screen(ctx, symbol, v)
}

func (s *LevelBounce) screen(ctx *Context, symbol string, v View) ScreenResult {
	res := ScreenResult{Symbol: symbol}
	n := v.Len()
	opens, highs, lows, closes := v.Opens(), v.Highs(), v.Lows(), v.Closes()

	closeValue := closes[n-1]
	smaLong := last(v.SMA(s.SMALong))
	atrValue := last(v.ATR(s.ATRPeriod))
	rvol := last(v.RVOL(s.VolumeLookback))
	dollarVolume := last(v.DollarVolume(s.VolumeLookback))
	prevClose := last(v.PrevClose())

	res.Close = closeValue
	if anyNaN(closeValue, smaLong, atrValue, rvol, dollarVolume, prevClose) || atrValue <= 0 {
		res.fail("指標が未計算")
		return res
	}

	atrRatio := atrValue / closeValue
	if !(s.MinATRRatio <= atrRatio && atrRatio <= s.MaxATRRatio) {
		res.fail(fmt.Sprintf("ATR比 %.1f%% が範囲外", atrRatio*100))
	}
	if dollarVolume < s.MinDollarVolume {
		res.fail(fmt.Sprintf("売買代金 %.0f が下限未満", dollarVolume))
	}
	if closeValue <= smaLong {
		res.fail(fmt.Sprintf("トレンド: 終値 %.1f ≦ SMA%d %.1f", closeValue, s.SMALong, smaLong))
	}
	if !benchmarkOK(ctx, s.Benchmark, s.BenchmarkSMA) {
		res.fail(fmt.Sprintf("地合い: %s が SMA%d 割れ", s.Benchmark, s.BenchmarkSMA))
	}
	res.set("atr_ratio", atrRatio)
	res.set("dollar_volume", dollarVolume)
	res.set("rvol", rvol)

	// 上昇波と押し目
	sw, ok := s.findSwing(v, true)
	if !ok {
		res.fail("直近に上昇波（安値→高値）が無い")
		return res
	}
	impulse := sw.high/sw.low - 1
	depth := (sw.high - sw.pullbackLow) / (sw.high - sw.low)
	res.set("swing_high", sw.high)
	res.set("swing_low", sw.low)
	res.set("pullback_low", sw.pullbackLow)
	res.set("impulse", impulse)
	res.set("depth", depth)
	if impulse < s.MinImpulse {
		res.fail(fmt.Sprintf("上昇波 %.1f%% が小さい（下限 %.0f%%）", impulse*100, s.MinImpulse*100))
	}
	if n-1-sw.highIdx < s.MinPullbackDays {
		res.fail(fmt.Sprintf("高値から %d 日しか経っていない", n-1-sw.highIdx))
	}
	if n-1-sw.pullbackIdx >= s.BounceWindow {
		res.fail(fmt.Sprintf("押し目安値が %d 日前で反発したてではない", n-1-sw.pullbackIdx))
	}
	if sw.pullbackLow <= sw.low {
		res.fail("押し目が上昇波の起点を割った（波が否定された）")
	}

	// 節目に届いたか
	tol := s.ToleranceATR * atrValue
	lv, touched := nearestLevel(s.candidateLevels(sw), sw.pullbackLow, tol)
	res.set("level", lv.price)
	res.set("level_distance_atr", math.Abs(sw.pullbackLow-lv.price)/atrValue)
	if !touched {
		res.fail(fmt.Sprintf("押し目安値 %.1f が節目（最寄り %s %.1f）に届いていない", sw.pullbackLow, lv.kind, lv.price))
	} else {
		res.Setup = lv.kind
	}
	if closeValue < lv.price {
		res.fail(fmt.Sprintf("終値 %.1f が節目 %.1f の下（支持されていない）", closeValue, lv.price))
	}

	// 買い手が現れたか（当日の足）
	openValue, highValue, lowValue := opens[n-1], highs[n-1], lows[n-1]
	strength := 0.0
	if highValue > lowValue {
		strength = (closeValue - lowValue) / (highValue - lowValue)
	}
	res.set("close_strength", strength)
	if !(closeValue > openValue && closeValue > prevClose) {
		res.fail("反転確認なし（当日が陽線で前日終値を上回っていない）")
	}
	if strength < s.CloseStrength {
		res.fail(fmt.Sprintf("終値位置 %.2f が弱い（下限 %.2f）", strength, s.CloseStrength))
	}
	if rvol < s.RVOLMin {
		res.fail(fmt.Sprintf("RVOL %.2f が下限 %.1f 未満", rvol, s.RVOLMin))
	}

	// 損益比: 目標は前回高値、損切りは押し目安値の許容幅下
	risk := closeValue - (sw.pullbackLow - tol)
	reward := sw.high - closeValue
	rr := 0.0
	if risk > 0 {
		rr = reward / risk
	}
	res.set("reward_risk", rr)
	if rr < s.MinRewardRisk {
		res.fail(fmt.Sprintf("損益比 %.2f が下限 %.1f 未満", rr, s.MinRewardRisk))
	}

	// 深い押し目ほど恐怖を吸収していて、出来高が伴うほど「その節目で買った人」が多い。
	depthScore := clamp01((depth - 0.3) / 0.4)
	volumeScore := clamp01((rvol - 1.0) / 2.0)
	impulseScore := clamp01(impulse / 0.5)
	rrScore := clamp01((rr - 1.0) / 3.0)
	res.set("depth_score", depthScore)
	res.set("volume_score", volumeScore)
	res.set("impulse_score", impulseScore)
	res.set("rr_score", rrScore)
	res.Score = math.Round((0.30*depthScore+0.30*volumeScore+0.15*impulseScore+0.25*rrScore)*10000) / 10000
	return res
}
