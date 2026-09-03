package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// TrendPullback は上昇トレンド銘柄の「押し目からのブレイクアウト」戦略。
//
// 平均回帰の逆張りではなく、「上昇トレンド銘柄が健全な押し目をつけ、出来高が
// 枯れた後に反発を始めた瞬間」を拾う。押し目そのもの（下落中）では買わない。
// 出来高が細り、下値が固まってから、直近高値を上抜けた日にだけ意見を出す。
//
// 手仕舞いは損切り・建値ストップ・時間切れ・利確をすべて risk/stops に委ね、
// この戦略が direction=-1 を出すのは決算ブラックアウトに入ったときだけ。
type TrendPullback struct {
	SMALong             int
	SMAMid              int
	SMAShort            int
	SlopeLookback       int
	RSLookback          int
	HighLookback        int
	MaxDrawdownFromHigh float64
	BreakoutLookback    int
	VolumeLookback      int
	VolumeDryupMax      float64
	ATRPeriod           int
	MinATRRatio         float64
	MaxATRRatio         float64
	MinDollarVolume     float64
	Benchmark           string
	BenchmarkSMA        int
	BlackoutDaysBefore  int
	ExitBeforeEarnings  bool
	Blackout            Blackout

	warmup int
}

// TrendPullbackOptions は TrendPullback の設定。ゼロ値は既定値に置き換わる。
type TrendPullbackOptions struct {
	SMALong             int
	SMAMid              int
	SMAShort            int
	SlopeLookback       int
	RSLookback          int
	HighLookback        int
	MaxDrawdownFromHigh float64
	BreakoutLookback    int
	VolumeLookback      int
	VolumeDryupMax      float64
	ATRPeriod           int
	MinATRRatio         float64
	MaxATRRatio         float64
	MinDollarVolume     float64
	Benchmark           string
	BenchmarkSMA        int
	BlackoutFile        string
	BlackoutDaysBefore  int
	ExitBeforeEarnings  bool
}

// DefaultTrendPullbackOptions は Python 版と同じ既定値。
func DefaultTrendPullbackOptions() TrendPullbackOptions {
	return TrendPullbackOptions{
		SMALong: 200, SMAMid: 50, SMAShort: 20,
		SlopeLookback: 10, RSLookback: 63, HighLookback: 60,
		MaxDrawdownFromHigh: 0.15, BreakoutLookback: 1,
		VolumeLookback: 20, VolumeDryupMax: 0.7,
		ATRPeriod: 14, MinATRRatio: 0.015, MaxATRRatio: 0.05,
		MinDollarVolume: 5_000_000, Benchmark: "SPY", BenchmarkSMA: 50,
		BlackoutDaysBefore: 3, ExitBeforeEarnings: true,
	}
}

// NewTrendPullback は押し目ブレイクアウト戦略を作る。
func NewTrendPullback(o TrendPullbackOptions) (*TrendPullback, error) {
	if !(o.SMAShort < o.SMAMid && o.SMAMid < o.SMALong) {
		return nil, fmt.Errorf("sma_short < sma_mid < sma_long を満たすこと: %d/%d/%d", o.SMAShort, o.SMAMid, o.SMALong)
	}
	if !(0 < o.MinATRRatio && o.MinATRRatio < o.MaxATRRatio) {
		return nil, fmt.Errorf("0 < min_atr_ratio < max_atr_ratio を満たすこと: %g, %g", o.MinATRRatio, o.MaxATRRatio)
	}
	if !(0 < o.VolumeDryupMax && o.VolumeDryupMax < 1) {
		return nil, fmt.Errorf("volume_dryup_max は 0 より大きく 1 未満: %g", o.VolumeDryupMax)
	}

	t := &TrendPullback{
		SMALong: o.SMALong, SMAMid: o.SMAMid, SMAShort: o.SMAShort,
		SlopeLookback: o.SlopeLookback, RSLookback: o.RSLookback, HighLookback: o.HighLookback,
		MaxDrawdownFromHigh: o.MaxDrawdownFromHigh, BreakoutLookback: o.BreakoutLookback,
		VolumeLookback: o.VolumeLookback, VolumeDryupMax: o.VolumeDryupMax,
		ATRPeriod: o.ATRPeriod, MinATRRatio: o.MinATRRatio, MaxATRRatio: o.MaxATRRatio,
		MinDollarVolume: o.MinDollarVolume, Benchmark: o.Benchmark, BenchmarkSMA: o.BenchmarkSMA,
		BlackoutDaysBefore: o.BlackoutDaysBefore, ExitBeforeEarnings: o.ExitBeforeEarnings,
	}
	if o.BlackoutFile != "" {
		b, err := LoadBlackout(o.BlackoutFile)
		if err != nil {
			return nil, err
		}
		t.Blackout = b
	}
	t.warmup = maxInt(maxInt(o.SMALong, o.HighLookback), maxInt(o.BenchmarkSMA, o.RSLookback)) + o.SlopeLookback + 1
	return t, nil
}

func (t *TrendPullback) Name() string    { return "trend_pullback" }
func (t *TrendPullback) WarmupBars() int { return t.warmup }
func (t *TrendPullback) Describe() string {
	return fmt.Sprintf("%s(sma=%d/%d/%d, breakout=%d日高値, benchmark=%s)",
		t.Name(), t.SMAShort, t.SMAMid, t.SMALong, t.BreakoutLookback, t.Benchmark)
}

func (t *TrendPullback) OnBars(ctx *Context) ([]domain.Signal, error) {
	return eachSymbol(ctx, t.warmup, func(symbol string, v View) *domain.Signal {
		if symbol == t.Benchmark {
			return nil // ベンチマークは売買対象にしない
		}
		inBlackout := t.Blackout.InBlackout(symbol, ctx.AsOf, t.BlackoutDaysBefore)

		if ctx.HasPosition(symbol) {
			return t.evaluateExit(symbol, inBlackout)
		}
		if inBlackout {
			return nil
		}
		return t.evaluateEntry(ctx, symbol, v)
	}), nil
}

func (t *TrendPullback) evaluateEntry(ctx *Context, symbol string, v View) *domain.Signal {
	checks := t.screen(ctx, symbol, v)
	if !checks.Passed() {
		return nil
	}
	return signal(t.Name(), symbol, scoreToDirection(checks.Score), 1.0,
		fmt.Sprintf("押し目からのブレイク: 出来高比 %.0f%%, 高値から %.1f%%, RS +%.1fpt, スコア %.2f",
			checks.Values["dryup_ratio"]*100, checks.Values["drawdown"]*100,
			checks.Values["rs_margin"], checks.Score),
		checks.Meta())
}

// evaluateExit は保有中の判断。損切り・利確・時間切れは risk/stops に委ねる。
//
// ここで判断するのは、日足の指標だけでは分からない決算リスクだけ。
func (t *TrendPullback) evaluateExit(symbol string, inBlackout bool) *domain.Signal {
	if inBlackout && t.ExitBeforeEarnings {
		return signal(t.Name(), symbol, -1.0, 1.0, "決算前のため手仕舞い", nil)
	}
	// 意見が無いと sizer が「シグナル消滅」で手仕舞うため、明示的に弱い買いを返す。
	return signal(t.Name(), symbol, 0.5, 1.0, "保有継続（損切り・利確はストップ管理に委ねる）", nil)
}

// Screen は 1 銘柄のエントリー条件を1つずつ評価する（screen --show-failed 用）。
func (t *TrendPullback) Screen(ctx *Context, symbol string) ScreenResult {
	v, ok := ctx.Bars(symbol)
	if !ok || v.Len() < t.warmup {
		return ScreenResult{Symbol: symbol, Failed: []string{"足の本数がウォームアップに足りない"}}
	}
	return t.screen(ctx, symbol, v)
}

func (t *TrendPullback) screen(ctx *Context, symbol string, v View) ScreenResult {
	res := ScreenResult{Symbol: symbol}

	closes := v.Closes()
	smaLongSeries := v.SMA(t.SMALong)
	smaMidSeries := v.SMA(t.SMAMid)

	closeValue := last(closes)
	smaLong := last(smaLongSeries)
	smaMid := last(smaMidSeries)
	atrValue := last(v.ATR(t.ATRPeriod))
	high := last(v.HighestHigh(t.HighLookback))
	dollarVolume := last(v.DollarVolume(t.VolumeLookback))
	dryupRatio := last(v.VolumeDryup(t.VolumeLookback))
	breakoutHigh := last(v.DonchianHigh(t.BreakoutLookback))
	rsValue := last(v.ROC(t.RSLookback))

	res.Close = closeValue
	if anyNaN(closeValue, smaLong, smaMid, atrValue, high, dollarVolume, dryupRatio, breakoutHigh) {
		res.fail("指標が未計算")
		return res
	}

	olderMid := ago(smaMidSeries, t.SlopeLookback)
	olderLong := ago(smaLongSeries, t.SlopeLookback)

	drawdown := 1.0
	if high > 0 {
		drawdown = 1.0 - closeValue/high
	}
	atrRatio := 0.0
	if closeValue != 0 {
		atrRatio = atrValue / closeValue
	}

	// 地合いとベンチマーク騰落率は銘柄横断の材料。Context から都度引く。
	marketOK := benchmarkOK(ctx, t.Benchmark, t.BenchmarkSMA)
	benchReturn := benchmarkROC(ctx, t.Benchmark, t.RSLookback)
	rsMargin := math.NaN()
	if !math.IsNaN(rsValue) && !math.IsNaN(benchReturn) {
		rsMargin = rsValue - benchReturn
	}

	if !(closeValue > smaLong && smaMid > smaLong) {
		res.fail(fmt.Sprintf("トレンド: 終値 > SMA%d > … を満たさない", t.SMALong))
	}
	if !math.IsNaN(olderLong) && smaLong <= olderLong {
		res.fail(fmt.Sprintf("SMA%d が下向き", t.SMALong))
	}
	if !math.IsNaN(olderMid) && smaMid <= olderMid {
		res.fail(fmt.Sprintf("SMA%d が下向き", t.SMAMid))
	}
	if t.Benchmark != "" && (math.IsNaN(rsMargin) || rsMargin <= 0) {
		res.fail(fmt.Sprintf("RS: %s に対する超過リターンが無い/マイナス", t.Benchmark))
	}
	if drawdown > t.MaxDrawdownFromHigh {
		res.fail(fmt.Sprintf("高値から %.1f%% 下落（上限 %.0f%%）", drawdown*100, t.MaxDrawdownFromHigh*100))
	}
	if dryupRatio > t.VolumeDryupMax {
		res.fail(fmt.Sprintf("出来高比 %.0f%% が枯れていない（上限 %.0f%%）", dryupRatio*100, t.VolumeDryupMax*100))
	}
	if closeValue <= breakoutHigh {
		res.fail(fmt.Sprintf("直近%d日高値 %.2f を未達", t.BreakoutLookback, breakoutHigh))
	}
	if !(t.MinATRRatio <= atrRatio && atrRatio <= t.MaxATRRatio) {
		res.fail(fmt.Sprintf("ATR比 %.1f%% が範囲外", atrRatio*100))
	}
	if dollarVolume < t.MinDollarVolume {
		res.fail(fmt.Sprintf("売買代金 %.0f が下限未満", dollarVolume))
	}
	if !marketOK {
		res.fail(fmt.Sprintf("地合い: %s が SMA%d 割れ", t.Benchmark, t.BenchmarkSMA))
	}

	trendDistance := closeValue/smaLong - 1.0
	if math.IsNaN(rsMargin) {
		rsMargin = 0
	}
	res.set("dryup_ratio", dryupRatio)
	res.set("drawdown", drawdown)
	res.set("atr_ratio", atrRatio)
	res.set("dollar_volume", dollarVolume)
	res.set("trend_distance", trendDistance)
	res.set("rs_margin", rsMargin)

	// スコアは押し目の質（dryup）と資金の向かい先（rs）を重く見る。
	dryup := clamp01((t.VolumeDryupMax - dryupRatio) / t.VolumeDryupMax)
	rs := clamp01(rsMargin / 20.0)
	trend := clamp01(trendDistance / 0.20)
	liquid := clamp01((math.Log10(math.Max(dollarVolume, 1.0)) - math.Log10(t.MinDollarVolume)) / 2.0)
	res.set("dryup", dryup)
	res.set("rs", rs)
	res.set("trend", trend)
	res.set("liquid", liquid)
	res.Score = math.Round((0.35*dryup+0.30*rs+0.20*trend+0.15*liquid)*10000) / 10000
	return res
}
