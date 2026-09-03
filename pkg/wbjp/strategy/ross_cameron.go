package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// RossCameron はロス・キャメロン流モメンタム（Gap & Go / マイクロプルバック）の日足版。
//
// 本家は分足のデイトレードなので、そのままは動かない。翻訳の対応表:
//
//	本家（分足・小型株）              この戦略（日足・スイング）
//	寄付前ギャップ ≧ 10%              始値が前日終値比 ≧ gap_min
//	相対出来高（RVOL）≧ 2〜5 倍       当日出来高 ÷ 20日平均出来高 ≧ rvol_min
//	寄付前高値のブレイク              終値が直近 high_lookback 日高値を上抜け
//	9EMA / 20EMA の上で推移           終値 > EMA9 かつ EMA9 > EMA20
//	1〜3本の押し目のあと直前高値抜け   材料日の後 1〜3 日の押し目 → 前日高値抜け
//	9EMA を割ったら降りる             終値 < EMA9 で手仕舞い（exit_on_ema）
//
// 本家の「勝率 60〜70%・損益比 2:1 以上」という骨格はそのまま残す。出口の
// 非対称性が主眼で、入口の条件は「動意づいた瞬間に乗る」ためのものに過ぎない。
// 浮動株数は日足データに無いため未実装。
type RossCameron struct {
	GapMin             float64
	RVOLMin            float64
	CloseStrength      float64
	HighLookback       int
	EMAFast            int
	EMASlow            int
	VolumeLookback     int
	AllowGapEntry      bool
	AllowPullbackEntry bool
	CatalystLookback   int
	MaxPullbackDays    int
	ATRPeriod          int
	MinATRRatio        float64
	MaxATRRatio        float64
	MinDollarVolume    float64
	MinPrice           *float64
	MaxPrice           *float64
	Benchmark          string
	BenchmarkSMA       int
	BlackoutDaysBefore int
	ExitBeforeEarnings bool
	ExitOnEMA          bool
	ExitOnPrevLow      bool
	Blackout           Blackout

	warmup int
}

// RossCameronOptions は RossCameron の設定。
type RossCameronOptions struct {
	GapMin             float64
	RVOLMin            float64
	CloseStrength      float64
	HighLookback       int
	EMAFast            int
	EMASlow            int
	VolumeLookback     int
	AllowGapEntry      bool
	AllowPullbackEntry bool
	CatalystLookback   int
	MaxPullbackDays    int
	ATRPeriod          int
	MinATRRatio        float64
	MaxATRRatio        float64
	MinDollarVolume    float64
	MinPrice           *float64
	MaxPrice           *float64
	Benchmark          string
	BenchmarkSMA       int
	BlackoutFile       string
	BlackoutDaysBefore int
	ExitBeforeEarnings bool
	ExitOnEMA          bool
	ExitOnPrevLow      bool
}

// DefaultRossCameronOptions は Python 版と同じ既定値。
//
// 決算ブラックアウトは既定で使わない。本家は決算ギャップこそを材料として
// 取りに行くため。
func DefaultRossCameronOptions() RossCameronOptions {
	return RossCameronOptions{
		GapMin: 0.03, RVOLMin: 2.0, CloseStrength: 0.7,
		HighLookback: 20, EMAFast: 9, EMASlow: 20, VolumeLookback: 20,
		AllowGapEntry: true, AllowPullbackEntry: true,
		CatalystLookback: 5, MaxPullbackDays: 3,
		ATRPeriod: 14, MinATRRatio: 0.015, MaxATRRatio: 0.10,
		MinDollarVolume: 5_000_000, Benchmark: "SPY", BenchmarkSMA: 50,
		BlackoutDaysBefore: 3, ExitBeforeEarnings: true,
		ExitOnEMA: true, ExitOnPrevLow: false,
	}
}

// NewRossCameron は Gap & Go 戦略を作る。
func NewRossCameron(o RossCameronOptions) (*RossCameron, error) {
	if o.GapMin <= 0 {
		return nil, fmt.Errorf("gap_min は正の比率（例: 0.03）: %g", o.GapMin)
	}
	if o.RVOLMin <= 0 {
		return nil, fmt.Errorf("rvol_min は正の倍率（例: 2.0）: %g", o.RVOLMin)
	}
	if !(0.0 <= o.CloseStrength && o.CloseStrength <= 1.0) {
		return nil, fmt.Errorf("close_strength は 0〜1: %g", o.CloseStrength)
	}
	if !(o.EMAFast < o.EMASlow) {
		return nil, fmt.Errorf("ema_fast < ema_slow を満たすこと: %d, %d", o.EMAFast, o.EMASlow)
	}
	if !(0 < o.MinATRRatio && o.MinATRRatio < o.MaxATRRatio) {
		return nil, fmt.Errorf("0 < min_atr_ratio < max_atr_ratio を満たすこと: %g, %g", o.MinATRRatio, o.MaxATRRatio)
	}
	if o.MinPrice != nil && o.MaxPrice != nil && !(*o.MinPrice < *o.MaxPrice) {
		return nil, fmt.Errorf("min_price < max_price を満たすこと: %g, %g", *o.MinPrice, *o.MaxPrice)
	}
	if !o.AllowGapEntry && !o.AllowPullbackEntry {
		return nil, fmt.Errorf("allow_gap_entry / allow_pullback_entry のどちらかは有効にすること")
	}
	if o.MaxPullbackDays < 1 || o.CatalystLookback <= o.MaxPullbackDays {
		return nil, fmt.Errorf("1 ≦ max_pullback_days < catalyst_lookback を満たすこと: %d, %d", o.MaxPullbackDays, o.CatalystLookback)
	}

	r := &RossCameron{
		GapMin: o.GapMin, RVOLMin: o.RVOLMin, CloseStrength: o.CloseStrength,
		HighLookback: o.HighLookback, EMAFast: o.EMAFast, EMASlow: o.EMASlow,
		VolumeLookback: o.VolumeLookback,
		AllowGapEntry:  o.AllowGapEntry, AllowPullbackEntry: o.AllowPullbackEntry,
		CatalystLookback: o.CatalystLookback, MaxPullbackDays: o.MaxPullbackDays,
		ATRPeriod: o.ATRPeriod, MinATRRatio: o.MinATRRatio, MaxATRRatio: o.MaxATRRatio,
		MinDollarVolume: o.MinDollarVolume, MinPrice: o.MinPrice, MaxPrice: o.MaxPrice,
		Benchmark: o.Benchmark, BenchmarkSMA: o.BenchmarkSMA,
		BlackoutDaysBefore: o.BlackoutDaysBefore, ExitBeforeEarnings: o.ExitBeforeEarnings,
		ExitOnEMA: o.ExitOnEMA, ExitOnPrevLow: o.ExitOnPrevLow,
	}
	if o.BlackoutFile != "" {
		b, err := LoadBlackout(o.BlackoutFile)
		if err != nil {
			return nil, err
		}
		r.Blackout = b
	}
	// EMA は種の平均から収束するまで数周期かかるため、遅い方の 3 倍を見込む
	r.warmup = maxInt(maxInt(3*o.EMASlow, o.HighLookback), maxInt(maxInt(o.VolumeLookback, o.ATRPeriod), o.BenchmarkSMA)) + o.CatalystLookback + 2
	return r, nil
}

func (r *RossCameron) Name() string    { return "ross_cameron" }
func (r *RossCameron) WarmupBars() int { return r.warmup }
func (r *RossCameron) Describe() string {
	return fmt.Sprintf("%s(gap≥%.0f%%, rvol≥%gx, ema=%d/%d, benchmark=%s)",
		r.Name(), r.GapMin*100, r.RVOLMin, r.EMAFast, r.EMASlow, r.Benchmark)
}

func (r *RossCameron) OnBars(ctx *Context) ([]domain.Signal, error) {
	return eachSymbol(ctx, r.warmup, func(symbol string, v View) *domain.Signal {
		if symbol == r.Benchmark {
			return nil
		}
		closeValue := last(v.Closes())
		emaFast := last(v.EMA(r.EMAFast))
		prevLow := last(v.PrevLow())
		if anyNaN(closeValue, emaFast, prevLow) {
			return nil
		}

		inBlackout := r.Blackout.InBlackout(symbol, ctx.AsOf, r.BlackoutDaysBefore)
		if ctx.HasPosition(symbol) {
			return r.evaluateExit(symbol, closeValue, emaFast, prevLow, inBlackout)
		}
		if inBlackout {
			return nil
		}

		checks := r.screen(ctx, symbol, v)
		if !checks.Passed() {
			return nil
		}
		return signal(r.Name(), symbol, scoreToDirection(checks.Score), 1.0,
			fmt.Sprintf("%s: ギャップ %+.1f%%, RVOL %.1fx, 引け強度 %.0f%%, スコア %.2f",
				checks.Setup, checks.Values["gap_pct"]*100, checks.Values["rvol_x"],
				checks.Values["close_strength"]*100, checks.Score),
			checks.Meta())
	}), nil
}

func (r *RossCameron) evaluateExit(symbol string, closeValue, emaFast, prevLow float64, inBlackout bool) *domain.Signal {
	reason := ""
	switch {
	case inBlackout && r.ExitBeforeEarnings:
		reason = "決算前のため手仕舞い"
	case r.ExitOnEMA && closeValue < emaFast:
		reason = fmt.Sprintf("終値 %.2f が EMA%d %.2f を割った", closeValue, r.EMAFast, emaFast)
	case r.ExitOnPrevLow && closeValue < prevLow:
		reason = fmt.Sprintf("終値 %.2f が前日安値 %.2f を割った", closeValue, prevLow)
	}
	if reason == "" {
		// 意見が無いと sizer が「シグナル消滅」で手仕舞うため、明示的に弱い買いを返す。
		return signal(r.Name(), symbol, 0.5, 1.0, fmt.Sprintf("保有継続（EMA%d の上）", r.EMAFast), nil)
	}
	return signal(r.Name(), symbol, -1.0, 1.0, reason, map[string]any{"ema_fast": emaFast})
}

// Screen は 1 銘柄のエントリー条件を1つずつ評価する（screen --show-failed 用）。
//
// Gap & Go とマイクロプルバックの両方を試し、通った方を Setup に残す。
func (r *RossCameron) Screen(ctx *Context, symbol string) ScreenResult {
	v, ok := ctx.Bars(symbol)
	if !ok || v.Len() < r.warmup {
		return ScreenResult{Symbol: symbol, Failed: []string{"足の本数がウォームアップに足りない"}}
	}
	return r.screen(ctx, symbol, v)
}

func (r *RossCameron) screen(ctx *Context, symbol string, v View) ScreenResult {
	res := ScreenResult{Symbol: symbol}
	i := v.Len() - 1

	closes, opens, highs, lows := v.Closes(), v.Opens(), v.Highs(), v.Lows()
	emaFastSeries := v.EMA(r.EMAFast)
	gaps, rvols := v.Gap(), v.RVOL(r.VolumeLookback)
	priorHigh := v.DonchianHigh(r.HighLookback)
	dollarVolume := v.DollarVolume(r.VolumeLookback)
	atrSeries := v.ATR(r.ATRPeriod)

	closeValue := at(closes, i)
	res.Close = closeValue
	if anyNaN(closeValue, at(opens, i), at(highs, i), at(lows, i),
		at(emaFastSeries, i), at(v.EMA(r.EMASlow), i), at(atrSeries, i),
		at(gaps, i), at(rvols, i), at(priorHigh, i), at(dollarVolume, i)) {
		res.fail("指標が未計算")
		return res
	}

	atrValue := at(atrSeries, i)
	atrRatio := 0.0
	if closeValue != 0 {
		atrRatio = atrValue / closeValue
	}
	strength := closeStrength(at(opens, i), at(highs, i), at(lows, i), closeValue)
	breakout := 0.0
	if atrValue != 0 {
		breakout = (closeValue - at(priorHigh, i)) / atrValue
	}

	// 共通フィルタ（どちらのセットアップでも必須）
	if !(closeValue > at(emaFastSeries, i) && at(emaFastSeries, i) > at(v.EMA(r.EMASlow), i)) {
		res.fail(fmt.Sprintf("短期トレンド: 終値 > EMA%d > EMA%d でない", r.EMAFast, r.EMASlow))
	}
	if !(r.MinATRRatio <= atrRatio && atrRatio <= r.MaxATRRatio) {
		res.fail(fmt.Sprintf("ATR比 %.1f%% が範囲外", atrRatio*100))
	}
	if at(dollarVolume, i) < r.MinDollarVolume {
		res.fail(fmt.Sprintf("売買代金 %.0f が下限未満", at(dollarVolume, i)))
	}
	if r.MinPrice != nil && closeValue < *r.MinPrice {
		res.fail(fmt.Sprintf("株価 %.2f < %g", closeValue, *r.MinPrice))
	}
	if r.MaxPrice != nil && closeValue > *r.MaxPrice {
		res.fail(fmt.Sprintf("株価 %.2f > %g", closeValue, *r.MaxPrice))
	}
	if !benchmarkOK(ctx, r.Benchmark, r.BenchmarkSMA) {
		res.fail(fmt.Sprintf("地合い: %s が SMA%d 割れ", r.Benchmark, r.BenchmarkSMA))
	}

	// セットアップ判定。通ったものがあればそれを採用する
	gap, rvol := at(gaps, i), at(rvols, i)
	var setupFailed []string
	if r.AllowGapEntry {
		reasons := r.gapAndGoFailures(v, i, strength)
		if len(reasons) == 0 {
			res.Setup = "Gap&Go"
		} else {
			for _, s := range reasons {
				setupFailed = append(setupFailed, "Gap&Go: "+s)
			}
		}
	}
	if res.Setup == "" && r.AllowPullbackEntry {
		reasons, catalyst := r.pullbackFailures(v, i)
		if len(reasons) == 0 && catalyst >= 0 {
			res.Setup = "マイクロプルバック"
			gap, rvol = at(gaps, catalyst), at(rvols, catalyst)
		} else {
			for _, s := range reasons {
				setupFailed = append(setupFailed, "押し目: "+s)
			}
		}
	}
	if res.Setup == "" {
		res.Failed = append(res.Failed, setupFailed...)
	}

	res.set("gap_pct", gap)
	res.set("rvol_x", rvol)
	res.set("close_strength", strength)
	res.set("breakout_atr", breakout)
	res.set("atr_ratio", atrRatio)
	res.set("dollar_volume", at(dollarVolume, i))

	// 本家が最重視するのは「みんなが見ている株」かどうか。rvol と gap を重く見る。
	rvolScore := clamp01((rvol - r.RVOLMin) / (3.0 * r.RVOLMin))
	gapScore := clamp01((gap - r.GapMin) / (3.0 * r.GapMin))
	strengthScore := clamp01((strength - r.CloseStrength) / math.Max(1.0-r.CloseStrength, 1e-9))
	breakoutScore := clamp01(breakout / 2.0)
	res.set("rvol", rvolScore)
	res.set("gap", gapScore)
	res.set("strength", strengthScore)
	res.set("breakout", breakoutScore)
	res.Score = math.Round((0.35*rvolScore+0.30*gapScore+0.20*strengthScore+0.15*breakoutScore)*10000) / 10000
	return res
}

// gapAndGoFailures は Gap & Go の条件（材料日そのものの判定）。空なら合格。
func (r *RossCameron) gapAndGoFailures(v View, i int, strength float64) []string {
	var failed []string
	gap := at(v.Gap(), i)
	rvol := at(v.RVOL(r.VolumeLookback), i)
	if gap < r.GapMin {
		failed = append(failed, fmt.Sprintf("ギャップ %+.1f%% < %.1f%%", gap*100, r.GapMin*100))
	}
	if rvol < r.RVOLMin {
		failed = append(failed, fmt.Sprintf("RVOL %.1fx < %gx", rvol, r.RVOLMin))
	}
	if !(at(v.Closes(), i) > at(v.Opens(), i) && strength >= r.CloseStrength) {
		failed = append(failed, fmt.Sprintf("陽線で強く引けていない（引け強度 %.0f%%）", strength*100))
	}
	if at(v.Closes(), i) <= at(v.DonchianHigh(r.HighLookback), i) {
		failed = append(failed, fmt.Sprintf("直近 %d 日高値を抜けていない", r.HighLookback))
	}
	return failed
}

// isCatalystDay は材料日か: ギャップ ＋ RVOL ＋ 強い陽線（ブレイクは問わない）。
func (r *RossCameron) isCatalystDay(v View, i int) bool {
	gap := at(v.Gap(), i)
	rvol := at(v.RVOL(r.VolumeLookback), i)
	c, o := at(v.Closes(), i), at(v.Opens(), i)
	if anyNaN(gap, rvol, c, o) {
		return false
	}
	return gap >= r.GapMin && rvol >= r.RVOLMin && c > o &&
		closeStrength(o, at(v.Highs(), i), at(v.Lows(), i), c) >= r.CloseStrength
}

// pullbackFailures はマイクロプルバックの条件。(不合格理由, 材料日の添字) を返す。
func (r *RossCameron) pullbackFailures(v View, i int) ([]string, int) {
	// 直近 catalyst_lookback 日（当日を除く）から材料日を探す。新しい方を優先。
	start := i - r.CatalystLookback
	if start < 0 {
		start = 0
	}
	catalyst := -1
	for j := i - 1; j >= start; j-- {
		if r.isCatalystDay(v, j) {
			catalyst = j
			break
		}
	}
	if catalyst < 0 {
		return []string{fmt.Sprintf("直近 %d 日に材料日がない", r.CatalystLookback)}, -1
	}

	closes, lows := v.Closes(), v.Lows()
	prevClose := v.PrevClose()
	emaFast := v.EMA(r.EMAFast)

	between := i - catalyst - 1 // 材料日と当日の間の本数
	var pullback []int
	for j := catalyst + 1; j < i; j++ {
		if at(closes, j) < at(prevClose, j) {
			pullback = append(pullback, j)
		}
	}

	var failed []string
	switch {
	case between == 0 || len(pullback) == 0:
		failed = append(failed, "材料日のあとに押し目がない")
	case between > r.MaxPullbackDays:
		failed = append(failed, fmt.Sprintf("押し目が %d 日と長すぎる（上限 %d）", between, r.MaxPullbackDays))
	default:
		for _, j := range pullback {
			if at(lows, j) < at(emaFast, j) {
				failed = append(failed, fmt.Sprintf("押し目の安値が EMA%d を割った", r.EMAFast))
				break
			}
		}
	}
	if !(at(closes, i) > at(v.PrevHigh(), i) && at(closes, i) > at(v.Opens(), i)) {
		failed = append(failed, "前日高値を陽線で抜けていない")
	}
	if at(v.Volumes(), i) <= at(v.PrevVolume(), i) {
		failed = append(failed, "出来高が前日を上回っていない")
	}
	return failed, catalyst
}

// closeStrength は終値がその日のレンジのどこで引けたか（0 = 安値引け、1 = 高値引け）。
func closeStrength(open, high, low, closeValue float64) float64 {
	span := high - low
	if span <= 0 || math.IsNaN(span) {
		if closeValue >= open {
			return 1.0
		}
		return 0.0
	}
	return clamp01((closeValue - low) / span)
}
