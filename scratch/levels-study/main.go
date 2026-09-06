// 使い捨て: level_bounce の入口シグナルに優位性があるかを、出口・サイジング抜きで測る。
//
//	go run ./scratch/levels-study -config-dir config/jp-levels -from 2017-09-01
//
// 各営業日に全銘柄を Screen し、通った (銘柄, 日) の「翌日寄付 → k 日後終値」の
// リターンを、同じ日のユニバース平均（等金額）と比べる（超過リターン）。
// 対照として「トレンド条件（終値 > SMA200）だけ満たす全日」も同じ指標で出す。
// 節目の種類・戻し比率・出来高・年ごとの内訳も並べる。
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
)

var horizons = []int{1, 3, 5, 10, 20}

type sample struct {
	symbol, date, setup string
	year                string
	depth, rvol, rr     float64
	fwd                 map[int]float64 // 翌日寄付 → k 日後終値（超過、比率）
	raw                 map[int]float64 // 同・生
	hitTarget, hitStop  bool            // 20 日以内に目標／損切りのどちらに先に触れたか
	stopFirst           bool
}

type series struct {
	dates  []string
	open   []float64
	high   []float64
	low    []float64
	close  []float64
	index  map[string]int
	smaOK  []bool
}

func main() {
	var (
		configDir = flag.String("config-dir", "config/jp-levels", "")
		from      = flag.String("from", "2017-09-01", "")
		params    = flag.String("params", "", "戦略パラメータ (key=value,...)")
	)
	flag.Parse()

	setCfg, err := wbjpcfg.LoadSettingsFile(*configDir)
	must(err)
	raw := map[string]any{}
	for _, kv := range strings.Split(*params, ",") {
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		var v float64
		if _, err := fmt.Sscanf(parts[1], "%g", &v); err == nil {
			raw[parts[0]] = v
		} else if parts[1] == "true" || parts[1] == "false" {
			raw[parts[0]] = parts[1] == "true"
		} else {
			raw[parts[0]] = parts[1]
		}
	}
	strat, err := strategy.Create("level_bounce", raw)
	must(err)
	screener := strat.(strategy.Screener)
	lb := strat.(*strategy.LevelBounce)
	fmt.Fprintln(os.Stderr, strat.Describe())

	store := data.NewBarStore(settings.LoadAppSettings().BarsDir())
	if topix, err := store.Read("^TOPIX", *from, ""); err == nil && len(topix) > 1 {
		first, lastBar := topix[0], topix[len(topix)-1]
		fmt.Fprintf(os.Stderr, "TOPIX 買い持ち %s %s → %s %s: %+.1f%%\n", first.Date, first.Close.StringFixed(2),
			lastBar.Date, lastBar.Close.StringFixed(2), (f(lastBar.Close)/f(first.Close)-1)*100)
	}
	allBars := map[string][]domain.Bar{}
	ser := map[string]*series{}
	dateSet := map[string]struct{}{}
	for _, sym := range setCfg.Universe.Symbols {
		bars, err := store.Read(sym, "", "")
		if err != nil || len(bars) == 0 {
			continue
		}
		allBars[sym] = bars
		s := &series{index: map[string]int{}}
		for i, b := range bars {
			s.dates = append(s.dates, b.Date)
			s.open = append(s.open, f(b.Open))
			s.high = append(s.high, f(b.High))
			s.low = append(s.low, f(b.Low))
			s.close = append(s.close, f(b.Close))
			s.index[b.Date] = i
			dateSet[b.Date] = struct{}{}
		}
		s.smaOK = make([]bool, len(bars))
		sum := 0.0
		for i := range bars {
			sum += s.close[i]
			if i >= 200 {
				sum -= s.close[i-200]
				s.smaOK[i] = s.close[i] > sum/200
			}
		}
		ser[sym] = s
	}
	var dates []string
	for d := range dateSet {
		if d >= *from {
			dates = append(dates, d)
		}
	}
	sort.Strings(dates)
	universe := strategy.NewUniverse(allBars)

	// 日ごとのユニバース平均リターン（翌日寄付 → k 日後終値、等金額）
	marketFwd := map[string]map[int]float64{}
	for _, d := range dates {
		acc := map[int][]float64{}
		for _, s := range ser {
			i, ok := s.index[d]
			if !ok {
				continue
			}
			for _, k := range horizons {
				if r, ok := fwdReturn(s, i, k); ok {
					acc[k] = append(acc[k], r)
				}
			}
		}
		marketFwd[d] = map[int]float64{}
		for k, xs := range acc {
			marketFwd[d][k] = mean(xs)
		}
	}

	var signals, baseline []sample
	for _, d := range dates {
		ctx := universe.At(d, nil, decimal.Zero)
		for _, sym := range ctx.Symbols() {
			s := ser[sym]
			i, ok := s.index[d]
			if !ok || !s.smaOK[i] {
				continue
			}
			base := makeSample(s, sym, d, i, marketFwd[d])
			if base == nil {
				continue
			}
			baseline = append(baseline, *base)
			res := screener.Screen(ctx, sym)
			if !res.Passed() {
				continue
			}
			sig := *base
			sig.setup = res.Setup
			sig.depth = res.Values["depth"]
			sig.rvol = res.Values["rvol"]
			sig.rr = res.Values["reward_risk"]
			target := res.Values["swing_high"]
			stop := res.Values["pullback_low"] - lb.ToleranceATR*res.Values["atr_ratio"]*res.Close
			sig.hitTarget, sig.hitStop, sig.stopFirst = race(s, i, 20, target, stop)
			signals = append(signals, sig)
		}
	}
	fmt.Fprintf(os.Stderr, "シグナル %d 件、対照 %d 件、%d 営業日\n", len(signals), len(baseline), len(dates))

	fmt.Println("## 先行リターン（翌日寄付 → k 日後終値）。超過＝同日のユニバース等金額平均を引いたもの")
	fmt.Println()
	fmt.Println("| 群 | n | 生 5d | 生 10d | 生 20d | 超過 1d | 超過 3d | 超過 5d | 超過 10d | 超過 20d | 勝率(超過 10d) | 目標先着 | 損切先着 |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	printRow("対照: 終値>SMA200 の全日", baseline)
	printRow("節目反発（全体）", signals)
	for _, g := range groupBy(signals, func(s sample) string { return "節目 " + s.setup }) {
		printRow(g.name, g.items)
	}
	for _, g := range groupBy(signals, func(s sample) string {
		switch {
		case s.depth < 0.45:
			return "戻し <45%"
		case s.depth < 0.55:
			return "戻し 45〜55%"
		default:
			return "戻し ≥55%"
		}
	}) {
		printRow(g.name, g.items)
	}
	for _, g := range groupBy(signals, func(s sample) string {
		switch {
		case s.rvol < 1.5:
			return "RVOL 1.2〜1.5"
		case s.rvol < 2.5:
			return "RVOL 1.5〜2.5"
		default:
			return "RVOL ≥2.5"
		}
	}) {
		printRow(g.name, g.items)
	}
	for _, g := range groupBy(signals, func(s sample) string {
		switch {
		case s.rr < 2:
			return "損益比 1.5〜2"
		case s.rr < 3:
			return "損益比 2〜3"
		default:
			return "損益比 ≥3"
		}
	}) {
		printRow(g.name, g.items)
	}
	fmt.Println()
	fmt.Println("| 年 | n | 超過 5d | 超過 10d | 超過 20d | 勝率(超過 10d) | 対照 n | 対照 超過 10d |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|---:|")
	baseByYear := map[string][]sample{}
	for _, b := range baseline {
		baseByYear[b.year] = append(baseByYear[b.year], b)
	}
	for _, g := range groupBy(signals, func(s sample) string { return s.year }) {
		bs := baseByYear[g.name]
		fmt.Printf("| %s | %d | %s | %s | %s | %s | %d | %s |\n", g.name, len(g.items),
			pct(meanOf(g.items, func(s sample) float64 { return s.fwd[5] })),
			pct(meanOf(g.items, func(s sample) float64 { return s.fwd[10] })),
			pct(meanOf(g.items, func(s sample) float64 { return s.fwd[20] })),
			pct(winRate(g.items, 10)), len(bs),
			pct(meanOf(bs, func(s sample) float64 { return s.fwd[10] })))
	}
}

func printRow(name string, xs []sample) {
	if len(xs) == 0 {
		return
	}
	target, stop := 0, 0
	for _, s := range xs {
		if s.hitTarget && !s.stopFirst {
			target++
		}
		if s.hitStop && s.stopFirst {
			stop++
		}
	}
	fmt.Printf("| %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", name, len(xs),
		pct(meanOf(xs, func(s sample) float64 { return s.raw[5] })),
		pct(meanOf(xs, func(s sample) float64 { return s.raw[10] })),
		pct(meanOf(xs, func(s sample) float64 { return s.raw[20] })),
		pct(meanOf(xs, func(s sample) float64 { return s.fwd[1] })),
		pct(meanOf(xs, func(s sample) float64 { return s.fwd[3] })),
		pct(meanOf(xs, func(s sample) float64 { return s.fwd[5] })),
		pct(meanOf(xs, func(s sample) float64 { return s.fwd[10] })),
		pct(meanOf(xs, func(s sample) float64 { return s.fwd[20] })),
		pct(winRate(xs, 10)),
		pct(float64(target)/float64(len(xs))), pct(float64(stop)/float64(len(xs))))
}

type group struct {
	name  string
	items []sample
}

func groupBy(xs []sample, key func(sample) string) []group {
	m := map[string][]sample{}
	for _, x := range xs {
		m[key(x)] = append(m[key(x)], x)
	}
	var out []group
	for k, v := range m {
		out = append(out, group{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func makeSample(s *series, sym, d string, i int, market map[int]float64) *sample {
	out := &sample{symbol: sym, date: d, year: d[:4], fwd: map[int]float64{}, raw: map[int]float64{}}
	for _, k := range horizons {
		r, ok := fwdReturn(s, i, k)
		if !ok {
			return nil
		}
		out.raw[k] = r
		out.fwd[k] = r - market[k]
	}
	return out
}

// fwdReturn は i の翌日寄付で建て、その k 日後（翌日を 1 日目）の終値で決済したリターン。
func fwdReturn(s *series, i, k int) (float64, bool) {
	entry, exit := i+1, i+k
	if exit >= len(s.close) || s.open[entry] <= 0 {
		return 0, false
	}
	return s.close[exit]/s.open[entry] - 1, true
}

// race は翌日から days 日の間に、目標（高値が target 以上）と損切り（安値が stop 以下）の
// どちらに先に触れたか。同じ日に両方なら損切りとみなす（保守的）。
func race(s *series, i, days int, target, stop float64) (hitTarget, hitStop, stopFirst bool) {
	for j := i + 1; j <= i+days && j < len(s.close); j++ {
		t := s.high[j] >= target
		st := s.low[j] <= stop
		if st {
			return t, true, true
		}
		if t {
			return true, false, false
		}
	}
	return false, false, false
}

func f(d decimal.Decimal) float64 { v, _ := d.Float64(); return v }

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func meanOf(xs []sample, get func(sample) float64) float64 {
	vals := make([]float64, 0, len(xs))
	for _, x := range xs {
		vals = append(vals, get(x))
	}
	return mean(vals)
}

func winRate(xs []sample, k int) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	w := 0
	for _, x := range xs {
		if x.fwd[k] > 0 {
			w++
		}
	}
	return float64(w) / float64(len(xs))
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%+.2f%%", v*100)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
