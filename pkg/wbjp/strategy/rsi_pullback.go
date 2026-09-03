package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// RSIPullback は上昇トレンド中の RSI(3) 押し目買い戦略（勝率重視のスイング）。
//
// 「長期上昇トレンドが確認済みの銘柄が、短期的に売られた所を拾い、平均回帰で
// 小さく取る」。ブレイクアウトより勝率が高く、利幅は小さい。
//
// 反転確認（当日が陽線）を使うときは、RSI は前日の値で判定する。反発した当日の
// RSI(3) は必ず跳ね上がるため、当日の値で見ると条件が両立しない。
type RSIPullback struct {
	SMALong             int
	SMAMid              int
	SMAShort            int
	SlopeLookback       int
	RSIPeriod           int
	RSIEntry            float64
	RSIExit             float64
	HighLookback        int
	MaxDrawdownFromHigh float64
	ATRPeriod           int
	MinATRRatio         float64
	MaxATRRatio         float64
	MinDollarVolume     float64
	VolumeLookback      int
	RequireReversalBar  bool
	Benchmark           string
	BenchmarkSMA        int
	BlackoutDaysBefore  int
	ExitBeforeEarnings  bool
	ExitOnRSI           bool
	ExitOnHigh          bool
	ExitOnSMARecovery   bool
	Blackout            Blackout

	warmup int
}

// RSIPullbackOptions は RSIPullback の設定。
type RSIPullbackOptions struct {
	SMALong             int
	SMAMid              int
	SMAShort            int
	SlopeLookback       int
	RSIPeriod           int
	RSIEntry            float64
	RSIExit             float64
	HighLookback        int
	MaxDrawdownFromHigh float64
	ATRPeriod           int
	MinATRRatio         float64
	MaxATRRatio         float64
	MinDollarVolume     float64
	VolumeLookback      int
	RequireReversalBar  bool
	Benchmark           string
	BenchmarkSMA        int
	BlackoutFile        string
	BlackoutDaysBefore  int
	ExitBeforeEarnings  bool
	ExitOnRSI           bool
	ExitOnHigh          bool
	ExitOnSMARecovery   bool
}

// DefaultRSIPullbackOptions は Python 版と同じ既定値。
func DefaultRSIPullbackOptions() RSIPullbackOptions {
	return RSIPullbackOptions{
		SMALong: 200, SMAMid: 50, SMAShort: 20, SlopeLookback: 10,
		RSIPeriod: 3, RSIEntry: 20, RSIExit: 80,
		HighLookback: 60, MaxDrawdownFromHigh: 0.15,
		ATRPeriod: 14, MinATRRatio: 0.015, MaxATRRatio: 0.05,
		MinDollarVolume: 5_000_000, VolumeLookback: 20,
		RequireReversalBar: true, Benchmark: "SPY", BenchmarkSMA: 50,
		BlackoutDaysBefore: 3, ExitBeforeEarnings: true,
		ExitOnRSI: true, ExitOnHigh: true, ExitOnSMARecovery: true,
	}
}

// NewRSIPullback は RSI 押し目買い戦略を作る。
func NewRSIPullback(o RSIPullbackOptions) (*RSIPullback, error) {
	if !(o.SMAShort < o.SMAMid && o.SMAMid < o.SMALong) {
		return nil, fmt.Errorf("sma_short < sma_mid < sma_long を満たすこと: %d/%d/%d", o.SMAShort, o.SMAMid, o.SMALong)
	}
	if !(0 < o.RSIEntry && o.RSIEntry < o.RSIExit && o.RSIExit < 100) {
		return nil, fmt.Errorf("0 < rsi_entry < rsi_exit < 100 を満たすこと: %g, %g", o.RSIEntry, o.RSIExit)
	}
	if !(0 < o.MinATRRatio && o.MinATRRatio < o.MaxATRRatio) {
		return nil, fmt.Errorf("0 < min_atr_ratio < max_atr_ratio を満たすこと: %g, %g", o.MinATRRatio, o.MaxATRRatio)
	}

	s := &RSIPullback{
		SMALong: o.SMALong, SMAMid: o.SMAMid, SMAShort: o.SMAShort, SlopeLookback: o.SlopeLookback,
		RSIPeriod: o.RSIPeriod, RSIEntry: o.RSIEntry, RSIExit: o.RSIExit,
		HighLookback: o.HighLookback, MaxDrawdownFromHigh: o.MaxDrawdownFromHigh,
		ATRPeriod: o.ATRPeriod, MinATRRatio: o.MinATRRatio, MaxATRRatio: o.MaxATRRatio,
		MinDollarVolume: o.MinDollarVolume, VolumeLookback: o.VolumeLookback,
		RequireReversalBar: o.RequireReversalBar, Benchmark: o.Benchmark, BenchmarkSMA: o.BenchmarkSMA,
		BlackoutDaysBefore: o.BlackoutDaysBefore, ExitBeforeEarnings: o.ExitBeforeEarnings,
		ExitOnRSI: o.ExitOnRSI, ExitOnHigh: o.ExitOnHigh, ExitOnSMARecovery: o.ExitOnSMARecovery,
	}
	if o.BlackoutFile != "" {
		b, err := LoadBlackout(o.BlackoutFile)
		if err != nil {
			return nil, err
		}
		s.Blackout = b
	}
	s.warmup = maxInt(maxInt(o.SMALong, o.HighLookback), o.BenchmarkSMA) + o.SlopeLookback + 1
	return s, nil
}

func (s *RSIPullback) Name() string    { return "rsi_pullback" }
func (s *RSIPullback) WarmupBars() int { return s.warmup }
func (s *RSIPullback) Describe() string {
	return fmt.Sprintf("%s(sma=%d/%d/%d, rsi%d<%g, benchmark=%s)",
		s.Name(), s.SMAShort, s.SMAMid, s.SMALong, s.RSIPeriod, s.RSIEntry, s.Benchmark)
}

func (s *RSIPullback) OnBars(ctx *Context) ([]domain.Signal, error) {
	return eachSymbol(ctx, s.warmup, func(symbol string, v View) *domain.Signal {
		if symbol == s.Benchmark {
			return nil // ベンチマークは売買対象にしない
		}

		closeValue := last(v.Closes())
		rsiValue := last(v.RSI(s.RSIPeriod))
		smaShort := last(v.SMA(s.SMAShort))
		smaLong := last(v.SMA(s.SMALong))
		high := last(v.HighestHigh(s.HighLookback))
		if anyNaN(closeValue, rsiValue, smaShort, smaLong, high) {
			return nil
		}

		inBlackout := s.Blackout.InBlackout(symbol, ctx.AsOf, s.BlackoutDaysBefore)

		if pos, held := ctx.Position(symbol); held && pos.Quantity.IsPositive() {
			return s.evaluateExit(symbol, closeValue, rsiValue, smaShort, high, pos, inBlackout)
		}
		if inBlackout {
			return nil
		}
		checks := s.screen(ctx, symbol, v)
		if !checks.Passed() {
			return nil
		}
		return signal(s.Name(), symbol, scoreToDirection(checks.Score), 1.0,
			fmt.Sprintf("押し目: RSI%d %.1f, 高値から %.1f%%, ATR %.1f%%, スコア %.2f",
				s.RSIPeriod, checks.Values["rsi"], checks.Values["drawdown"]*100,
				checks.Values["atr_ratio"]*100, checks.Score),
			checks.Meta())
	}), nil
}

func (s *RSIPullback) evaluateExit(symbol string, closeValue, rsiValue, smaShort, high float64, pos domain.Position, inBlackout bool) *domain.Signal {
	cost, _ := pos.CostPrice.Float64()

	reason := ""
	switch {
	case inBlackout && s.ExitBeforeEarnings:
		reason = "決算前のため手仕舞い"
	case s.ExitOnRSI && rsiValue >= s.RSIExit:
		reason = fmt.Sprintf("RSI%d %.1f が買われすぎ圏（≧%g）", s.RSIPeriod, rsiValue, s.RSIExit)
	case s.ExitOnHigh && closeValue >= high:
		reason = fmt.Sprintf("直近 %d 日高値に到達", s.HighLookback)
	case s.ExitOnSMARecovery && closeValue > smaShort && closeValue > cost:
		reason = fmt.Sprintf("含み益で SMA%d を回復（第一目標）", s.SMAShort)
	}

	if reason == "" {
		// 意見が無いと sizer が「シグナル消滅」で手仕舞うため、明示的に弱い買いを返す。
		return signal(s.Name(), symbol, 0.5, 1.0, "保有継続（押し目の回復待ち）", nil)
	}
	return signal(s.Name(), symbol, -1.0, 1.0, reason, map[string]any{"rsi": rsiValue})
}

// Screen は 1 銘柄のエントリー条件を1つずつ評価する（screen --show-failed 用）。
func (s *RSIPullback) Screen(ctx *Context, symbol string) ScreenResult {
	v, ok := ctx.Bars(symbol)
	if !ok || v.Len() < s.warmup {
		return ScreenResult{Symbol: symbol, Failed: []string{"足の本数がウォームアップに足りない"}}
	}
	return s.screen(ctx, symbol, v)
}

func (s *RSIPullback) screen(ctx *Context, symbol string, v View) ScreenResult {
	res := ScreenResult{Symbol: symbol}

	smaMidSeries := v.SMA(s.SMAMid)
	rsiSeries := v.RSI(s.RSIPeriod)

	closeValue := last(v.Closes())
	smaLong := last(v.SMA(s.SMALong))
	smaMid := last(smaMidSeries)
	smaShort := last(v.SMA(s.SMAShort))
	atrValue := last(v.ATR(s.ATRPeriod))
	high := last(v.HighestHigh(s.HighLookback))
	dollarVolume := last(v.DollarVolume(s.VolumeLookback))
	prevClose := last(v.PrevClose())

	// 反転確認を使うなら「前日に売られすぎていた」ことを見る。
	rsiValue := last(rsiSeries)
	if s.RequireReversalBar {
		rsiValue = ago(rsiSeries, 1)
	}

	res.Close = closeValue
	if anyNaN(closeValue, smaLong, smaMid, smaShort, rsiValue, atrValue, high, dollarVolume) {
		res.fail("指標が未計算")
		return res
	}

	olderMid := ago(smaMidSeries, s.SlopeLookback)

	drawdown := 1.0
	if high > 0 {
		drawdown = 1.0 - closeValue/high
	}
	atrRatio := 0.0
	if closeValue != 0 {
		atrRatio = atrValue / closeValue
	}
	marketOK := benchmarkOK(ctx, s.Benchmark, s.BenchmarkSMA)

	if !(closeValue > smaLong && smaMid > smaLong) {
		res.fail(fmt.Sprintf("トレンド: 終値 > SMA%d > … を満たさない", s.SMALong))
	}
	if !math.IsNaN(olderMid) && smaMid <= olderMid {
		res.fail(fmt.Sprintf("SMA%d が下向き", s.SMAMid))
	}
	if drawdown > s.MaxDrawdownFromHigh {
		res.fail(fmt.Sprintf("高値から %.1f%% 下落（上限 %.0f%%）", drawdown*100, s.MaxDrawdownFromHigh*100))
	}
	if rsiValue >= s.RSIEntry {
		res.fail(fmt.Sprintf("RSI%d %.1f ≧ %g", s.RSIPeriod, rsiValue, s.RSIEntry))
	}
	if s.RequireReversalBar && (math.IsNaN(prevClose) || closeValue <= prevClose) {
		res.fail("反転確認なし（当日が陽線でない）")
	}
	if !(s.MinATRRatio <= atrRatio && atrRatio <= s.MaxATRRatio) {
		res.fail(fmt.Sprintf("ATR比 %.1f%% が範囲外", atrRatio*100))
	}
	if dollarVolume < s.MinDollarVolume {
		res.fail(fmt.Sprintf("売買代金 %.0f が下限未満", dollarVolume))
	}
	if !marketOK {
		res.fail(fmt.Sprintf("地合い: %s が SMA%d 割れ", s.Benchmark, s.BenchmarkSMA))
	}

	trendDistance := closeValue/smaLong - 1.0
	stretch := 0.0
	if atrValue != 0 {
		stretch = (smaShort - closeValue) / atrValue
	}
	res.set("rsi", rsiValue)
	res.set("drawdown", drawdown)
	res.set("atr_ratio", atrRatio)
	res.set("dollar_volume", dollarVolume)
	res.set("trend_distance", trendDistance)
	res.set("stretch_atr", stretch)

	// 勝率に最も効くのは「どれだけ売られた所を拾えたか」なので dip / stretch を重く見る。
	dip := clamp01((s.RSIEntry - rsiValue) / s.RSIEntry)
	stretchScore := clamp01(stretch / 3.0)
	trend := clamp01(trendDistance / 0.20)
	liquid := clamp01((math.Log10(math.Max(dollarVolume, 1.0)) - math.Log10(s.MinDollarVolume)) / 2.0)
	res.set("dip", dip)
	res.set("stretch", stretchScore)
	res.set("trend", trend)
	res.set("liquid", liquid)
	res.Score = math.Round((0.35*dip+0.30*stretchScore+0.20*trend+0.15*liquid)*10000) / 10000
	return res
}
