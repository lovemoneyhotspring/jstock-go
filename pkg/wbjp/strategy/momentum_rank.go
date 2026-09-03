package strategy

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// MomentumRank はクロスセクショナル・モメンタム（順位付き、月次入れ替え）。
//
// 「過去 6〜12 ヶ月に強かった銘柄は、その後も数ヶ月は強い」というモメンタム
// 効果を使う。勝率は 5 割前後だが、勝ちが伸びて負けが小さい（損益比型）。
// 損切りを広く取り、手仕舞いはトレンドの崩れと順位の脱落で決める。
//
// 最大の弱点は急反転局面（モメンタムクラッシュ）。ベンチマークが長期移動平均を
// 割ったら新規建てを止め、建玉も全部降りることで軽減する。
//
// この戦略は銘柄をまたいだ順位付けが本体なので、1 銘柄ずつ評価する形では
// 書けない。OnBars が全銘柄を受け取るのはそのため。
type MomentumRank struct {
	Lookback        int
	Skip            int
	LongLookback    int
	VolLookback     int
	TrendSMA        int
	ATRPeriod       int
	MaxATRRatio     float64
	MinPrice        float64
	MinDollarVolume float64
	VolumeLookback  int
	Benchmark       string
	BenchmarkSMA    int
	TopN            int
	KeepMultiple    int
	Rebalance       string
	CoreSymbol      string

	warmup int
}

// MomentumRankOptions は MomentumRank の設定。
type MomentumRankOptions struct {
	Lookback        int
	Skip            int
	LongLookback    int
	VolLookback     int
	TrendSMA        int
	ATRPeriod       int
	MaxATRRatio     float64
	MinPrice        float64
	MinDollarVolume float64
	VolumeLookback  int
	Benchmark       string
	BenchmarkSMA    int
	TopN            int
	KeepMultiple    int
	Rebalance       string
	CoreSymbol      string
}

// DefaultMomentumRankOptions は Python 版と同じ既定値。
func DefaultMomentumRankOptions() MomentumRankOptions {
	return MomentumRankOptions{
		Lookback: 126, Skip: 21, LongLookback: 252, VolLookback: 63,
		TrendSMA: 100, ATRPeriod: 14, MaxATRRatio: 0.06,
		MinPrice: 5.0, MinDollarVolume: 20_000_000, VolumeLookback: 20,
		Benchmark: "SPY", BenchmarkSMA: 200,
		TopN: 5, KeepMultiple: 2, Rebalance: "monthly",
	}
}

// NewMomentumRank はモメンタム順位戦略を作る。
func NewMomentumRank(o MomentumRankOptions) (*MomentumRank, error) {
	if o.Lookback <= 0 || o.Skip < 0 || o.LongLookback <= o.Lookback+o.Skip {
		return nil, fmt.Errorf("long_lookback > lookback + skip > 0 を満たすこと: %d, %d, %d", o.LongLookback, o.Lookback, o.Skip)
	}
	switch o.Rebalance {
	case "monthly", "weekly", "daily":
	default:
		return nil, fmt.Errorf("rebalance は monthly / weekly / daily: %q", o.Rebalance)
	}
	if o.TopN <= 0 || o.KeepMultiple < 1 {
		return nil, fmt.Errorf("top_n は正、keep_multiple は 1 以上: %d, %d", o.TopN, o.KeepMultiple)
	}

	m := &MomentumRank{
		Lookback: o.Lookback, Skip: o.Skip, LongLookback: o.LongLookback,
		VolLookback: o.VolLookback, TrendSMA: o.TrendSMA, ATRPeriod: o.ATRPeriod,
		MaxATRRatio: o.MaxATRRatio, MinPrice: o.MinPrice,
		MinDollarVolume: o.MinDollarVolume, VolumeLookback: o.VolumeLookback,
		Benchmark: o.Benchmark, BenchmarkSMA: o.BenchmarkSMA,
		TopN: o.TopN, KeepMultiple: o.KeepMultiple, Rebalance: o.Rebalance,
		CoreSymbol: o.CoreSymbol,
	}
	m.warmup = maxInt(maxInt(o.LongLookback, o.BenchmarkSMA), o.TrendSMA) + o.Skip + 2
	return m, nil
}

func (m *MomentumRank) Name() string    { return "momentum_rank" }
func (m *MomentumRank) WarmupBars() int { return m.warmup }
func (m *MomentumRank) Describe() string {
	return fmt.Sprintf("%s(mom=%d-%d, sma=%d, rebalance=%s, top=%d, benchmark=%s, core=%s)",
		m.Name(), m.Lookback, m.Skip, m.TrendSMA, m.Rebalance, m.TopN, m.Benchmark, m.CoreSymbol)
}

// ranked は順位付けの 1 行。
type ranked struct {
	symbol string
	result ScreenResult
}

func (m *MomentumRank) OnBars(ctx *Context) ([]domain.Signal, error) {
	// ウォームアップを満たす銘柄だけを土俵に乗せる。
	eligible := make([]string, 0, len(ctx.Symbols()))
	for _, sym := range ctx.Symbols() {
		if ctx.HasBars(sym, m.warmup) {
			eligible = append(eligible, sym)
		}
	}

	marketOK, benchmarkMom := m.benchmarkState(ctx)
	rebalanceDay := m.isRebalanceDay(ctx, eligible)

	// 1) 母集団を絞り、順位付けの材料を集める
	var passed []ranked
	for _, sym := range eligible {
		if sym == m.Benchmark {
			continue
		}
		res := m.screenSymbol(ctx, sym, benchmarkMom, marketOK)
		if res.Passed() {
			passed = append(passed, ranked{symbol: sym, result: res})
		}
	}
	// スコア降順。同点は銘柄名で決めて、日ごとに順位が揺れないようにする。
	sort.SliceStable(passed, func(i, j int) bool {
		if passed[i].result.Score != passed[j].result.Score {
			return passed[i].result.Score > passed[j].result.Score
		}
		return passed[i].symbol < passed[j].symbol
	})
	rankOf := make(map[string]int, len(passed))
	for i, r := range passed {
		rankOf[r.symbol] = i + 1
	}
	keepRank := m.TopN * m.KeepMultiple

	var signals []domain.Signal
	add := func(s *domain.Signal) {
		if s != nil {
			signals = append(signals, *s)
		}
	}

	// 2) 保有中の判断（毎日）
	for _, sym := range ctx.HeldSymbols() {
		if !ctx.HasBars(sym, m.warmup) {
			continue
		}
		isCore := sym == m.CoreSymbol
		if sym == m.Benchmark && !isCore {
			continue
		}
		v, _ := ctx.Bars(sym)
		closeValue := last(v.Closes())
		trend := last(v.SMA(m.TrendSMA))

		rank, ranked := rankOf[sym]
		rankText := "圏外"
		if ranked {
			rankText = fmt.Sprintf("%d", rank)
		}

		reason := ""
		switch {
		case !marketOK:
			reason = fmt.Sprintf("地合いオフ: %s が SMA%d 割れ", m.Benchmark, m.BenchmarkSMA)
		case isCore:
			// 受け皿は本命が枠を埋められるときだけ降りる
			if rebalanceDay && len(passed) >= m.TopN {
				reason = fmt.Sprintf("受け皿を解除: 候補が %d 銘柄あり枠を埋められる", len(passed))
			}
		case !math.IsNaN(trend) && closeValue < trend:
			reason = fmt.Sprintf("トレンド崩れ: 終値が SMA%d を割った", m.TrendSMA)
		case rebalanceDay && (!ranked || rank > keepRank):
			reason = fmt.Sprintf("順位脱落: %s 位（上位 %d 位まで保持）", rankText, keepRank)
		}

		if reason != "" {
			add(signal(m.Name(), sym, -1.0, 1.0, reason, nil))
		} else {
			add(signal(m.Name(), sym, 0.5, 1.0, fmt.Sprintf("保有継続（順位 %s）", rankText), nil))
		}
	}

	// 3) 新規建ては区切りの日だけ
	if !rebalanceDay || !marketOK {
		return signals, nil
	}

	if m.CoreSymbol != "" && ctx.HasBars(m.CoreSymbol, 1) && !ctx.HasPosition(m.CoreSymbol) && len(passed) < m.TopN {
		// 候補が足りない分は受け皿で埋める。direction は下限（最下位）に置くので、
		// 本命の候補が優先して枠を取る。
		add(signal(m.Name(), m.CoreSymbol, directionFloor, 1.0,
			fmt.Sprintf("受け皿: 候補 %d 銘柄で枠 %d に満たない", len(passed), m.TopN),
			map[string]any{"rank": len(passed) + 1, "core": true}))
	}

	if len(passed) == 0 {
		return signals, nil
	}
	topScore := passed[0].result.Score
	for _, r := range passed {
		if ctx.HasPosition(r.symbol) {
			continue
		}
		// 1位を 1.0、順位が下がるほど下限に近づける
		relative := 0.0
		if topScore > 0 {
			relative = r.result.Score / topScore
		}
		meta := r.result.Meta()
		meta["rank"] = rankOf[r.symbol]
		add(signal(m.Name(), r.symbol, scoreToDirection(relative), 1.0,
			fmt.Sprintf("モメンタム %d 位: 6M %+.1f%% / 12M %+.1f%% / ボラ %.0f%%",
				rankOf[r.symbol], r.result.Values["mom_mid"]*100,
				r.result.Values["mom_long"]*100, r.result.Values["vol_ann"]*100),
			meta))
	}
	return signals, nil
}

// Screen は 1 銘柄のスクリーニング結果（screen --show-failed 用）。
func (m *MomentumRank) Screen(ctx *Context, symbol string) ScreenResult {
	if !ctx.HasBars(symbol, m.warmup) {
		return ScreenResult{Symbol: symbol, Failed: []string{"足の本数がウォームアップに足りない"}}
	}
	marketOK, benchmarkMom := m.benchmarkState(ctx)
	return m.screenSymbol(ctx, symbol, benchmarkMom, marketOK)
}

func (m *MomentumRank) screenSymbol(ctx *Context, symbol string, benchmarkMom float64, marketOK bool) ScreenResult {
	res := ScreenResult{Symbol: symbol}
	v, ok := ctx.Bars(symbol)
	if !ok {
		res.fail("足が無い")
		return res
	}

	closeValue := last(v.Closes())
	momMid := last(v.Return(m.Lookback, m.Skip))
	momLong := last(v.Return(m.LongLookback, 0))
	vol := last(v.AnnualizedVol(m.VolLookback))
	trend := last(v.SMA(m.TrendSMA))
	atrValue := last(v.ATR(m.ATRPeriod))
	dollarVolume := last(v.DollarVolume(m.VolumeLookback))

	res.Close = closeValue
	if anyNaN(closeValue, momMid, momLong, vol, trend, atrValue, dollarVolume) {
		res.fail("指標が未計算")
		return res
	}

	atrRatio := atrValue / closeValue

	if closeValue < m.MinPrice {
		res.fail(fmt.Sprintf("株価 %.2f が下限 %g 未満", closeValue, m.MinPrice))
	}
	if dollarVolume < m.MinDollarVolume {
		res.fail(fmt.Sprintf("売買代金 %.0f が下限未満", dollarVolume))
	}
	if atrRatio > m.MaxATRRatio {
		res.fail(fmt.Sprintf("ATR比 %.1f%% が上限 %.0f%% 超", atrRatio*100, m.MaxATRRatio*100))
	}
	if closeValue <= trend {
		res.fail(fmt.Sprintf("終値が SMA%d 以下", m.TrendSMA))
	}
	if momMid <= 0 {
		res.fail(fmt.Sprintf("6M リターン %+.1f%% がマイナス", momMid*100))
	}
	if momLong <= 0 {
		res.fail(fmt.Sprintf("12M リターン %+.1f%% がマイナス", momLong*100))
	}
	if m.Benchmark != "" && (math.IsNaN(benchmarkMom) || momMid <= benchmarkMom) {
		res.fail(fmt.Sprintf("6M リターンが %s を下回る", m.Benchmark))
	}
	if !marketOK {
		res.fail(fmt.Sprintf("地合い: %s が SMA%d 割れ", m.Benchmark, m.BenchmarkSMA))
	}

	// リスク調整後モメンタム。同じ上昇率なら、静かに上げた銘柄を上に置く。
	if vol > 0 {
		res.Score = momMid / vol
	}
	res.set("mom_mid", momMid)
	res.set("mom_long", momLong)
	res.set("vol_ann", vol)
	res.set("atr_ratio", atrRatio)
	res.set("dollar_volume", dollarVolume)
	return res
}

// benchmarkState は (地合いが良いか, ベンチマークの 6M リターン)。
//
// ベンチマークの足が無いなら地合いを判断できない。黙って通さず建てない。
func (m *MomentumRank) benchmarkState(ctx *Context) (bool, float64) {
	if m.Benchmark == "" {
		return true, math.NaN()
	}
	v, ok := ctx.Bars(m.Benchmark)
	if !ok {
		return false, math.NaN()
	}
	sma := last(v.SMA(m.BenchmarkSMA))
	okMarket := !math.IsNaN(sma) && last(v.Closes()) > sma
	return okMarket, last(v.Return(m.Lookback, m.Skip))
}

// isRebalanceDay は入れ替えの区切りの日か。
//
// 毎日入れ替えると売買代金がかさみ、モメンタムの「ゆっくり効く」性質と合わない。
func (m *MomentumRank) isRebalanceDay(ctx *Context, eligible []string) bool {
	if m.Rebalance == "daily" {
		return true
	}
	// 銘柄によらず同じ答えになるべきなので、辞書順で先頭の銘柄に固定する。
	// 走らせるたびに答えが変わると、入れ替え日が再現しなくなる。
	if len(eligible) == 0 {
		return false
	}
	v, ok := ctx.Bars(eligible[0])
	if !ok || v.Len() < 2 {
		return false
	}
	today, err1 := time.Parse("2006-01-02", v.Date(v.Len()-1))
	prev, err2 := time.Parse("2006-01-02", v.Date(v.Len()-2))
	if err1 != nil || err2 != nil {
		return false
	}
	if m.Rebalance == "monthly" {
		return today.Year() != prev.Year() || today.Month() != prev.Month()
	}
	ty, tw := today.ISOWeek()
	py, pw := prev.ISOWeek()
	return ty != py || tw != pw
}
