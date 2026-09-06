// 使い捨て: 既存戦略の「新規シグナル」を信用倍率の分位で層別し、先行リターンを測る。
//
//	go run ./scratch/signal-study -config-dir /tmp/mf/large-trio-off -from 2017-09-01
//
// バックテスト 1 経路は枠 10 の採用の入れ替わりで数十ポイント揺れる（経路依存）。
// ここでは経路を捨て、戦略が「建てたい」と言った (銘柄, 日) をすべて標本にして、
// 翌日寄付 → k 日後終値の超過リターン（同日のユニバース等金額平均を引く）を
// 信用倍率の横断分位ごとに並べる。フィルタが効くなら下位 20% の標本が悪いはず。
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/pelletier/go-toml/v2"
	"github.com/shopspring/decimal"
)

var horizons = []int{5, 10, 20}

type sample struct {
	symbol, date string
	direction    float64
	rank         float64 // 信用倍率の分位（無ければ NaN）
	fwd          map[int]float64
}

type series struct {
	open, close []float64
	index       map[string]int
}

func main() {
	var (
		configDir = flag.String("config-dir", "config/jp-levels", "")
		from      = flag.String("from", "2017-09-01", "")
		lag       = flag.Int("margin-lag-days", strategy.MarginPublicationLag, "")
	)
	flag.Parse()

	setCfg, err := wbjpcfg.LoadSettingsFile(*configDir)
	must(err)
	stratCfg, err := wbjpcfg.LoadStrategiesConfig(*configDir)
	must(err)
	strats, weights := buildStrategies(*configDir, stratCfg)
	combine := strategy.GetCombinerByName(stratCfg.Combiner)
	for _, s := range strats {
		fmt.Fprintln(os.Stderr, s.Describe())
	}

	app := settings.LoadAppSettings()
	store := data.NewBarStore(app.BarsDir())
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
			s.open = append(s.open, f(b.Open))
			s.close = append(s.close, f(b.Close))
			s.index[b.Date] = i
			dateSet[b.Date] = struct{}{}
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

	book := loadMargin(app.JQuantsArchiveDir(), setCfg.Universe.Symbols, *lag)
	fmt.Fprintf(os.Stderr, "信用残 %d 銘柄、遅れ %d 日\n", book.Len(), *lag)
	universe := strategy.NewUniverse(allBars).SetMargin(book)

	// 日ごとのユニバース平均（翌日寄付 → k 日後終値）
	marketFwd := map[string]map[int]float64{}
	for _, d := range dates {
		acc := map[int][]float64{}
		for _, s := range ser {
			if i, ok := s.index[d]; ok {
				for _, k := range horizons {
					if r, ok := fwdReturn(s, i, k); ok {
						acc[k] = append(acc[k], r)
					}
				}
			}
		}
		marketFwd[d] = map[int]float64{}
		for k, xs := range acc {
			marketFwd[d][k] = mean(xs)
		}
	}

	var samples []sample
	prevOn := map[string]bool{}
	equity := decimal.NewFromInt(30_000_000)
	for _, d := range dates {
		ctx := universe.At(d, nil, equity)
		bySymbol := map[string][]domain.Signal{}
		for _, s := range strats {
			sigs, err := s.OnBars(ctx)
			if err != nil {
				continue
			}
			for _, sig := range sigs {
				bySymbol[sig.Symbol] = append(bySymbol[sig.Symbol], sig)
			}
		}
		on := map[string]bool{}
		for _, sym := range ctx.Symbols() {
			cs := combine(sym, bySymbol[sym], weights)
			if cs.Direction < stratCfg.EntryThreshold {
				continue
			}
			on[sym] = true
			if prevOn[sym] {
				continue // 継続。新規に建てたい日だけ数える
			}
			s := ser[sym]
			i := s.index[d]
			smp := sample{symbol: sym, date: d, direction: cs.Direction, rank: math.NaN(), fwd: map[int]float64{}}
			complete := true
			for _, k := range horizons {
				r, ok := fwdReturn(s, i, k)
				if !ok {
					complete = false
					break
				}
				smp.fwd[k] = r - marketFwd[d][k]
			}
			if !complete {
				continue
			}
			if r, ok := ctx.MarginRatioRank(sym); ok {
				smp.rank = r
			}
			samples = append(samples, smp)
		}
		prevOn = on
	}
	fmt.Fprintf(os.Stderr, "新規シグナル %d 件（%d 営業日）\n", len(samples), len(dates))

	fmt.Printf("## %s: 新規シグナルの先行リターン（超過、翌日寄付 → k 日後終値）\n\n", filepath.Base(*configDir))
	fmt.Println("| 信用倍率の分位 | n | 超過 5d | 超過 10d | 超過 20d | 勝率(10d) |")
	fmt.Println("|---|---:|---:|---:|---:|---:|")
	printRow("全体", samples)
	printRow("信用残なし", filter(samples, func(s sample) bool { return math.IsNaN(s.rank) }))
	for q := 0; q < 5; q++ {
		lo, hi := float64(q)/5, float64(q+1)/5
		printRow(fmt.Sprintf("Q%d (%.0f〜%.0f%%)", q+1, lo*100, hi*100),
			filter(samples, func(s sample) bool { return !math.IsNaN(s.rank) && s.rank >= lo && s.rank < hi }))
	}
	printRow("下位 20% を除外", filter(samples, func(s sample) bool { return math.IsNaN(s.rank) || s.rank >= 0.2 }))
	printRow("上位 20% だけ", filter(samples, func(s sample) bool { return !math.IsNaN(s.rank) && s.rank >= 0.8 }))

	fmt.Println()
	fmt.Println("| 年 | n | 超過 20d 全体 | 下位 20% | 下位除外 | 差 |")
	fmt.Println("|---|---:|---:|---:|---:|---:|")
	byYear := map[string][]sample{}
	for _, s := range samples {
		byYear[s.date[:4]] = append(byYear[s.date[:4]], s)
	}
	var years []string
	for y := range byYear {
		years = append(years, y)
	}
	sort.Strings(years)
	for _, y := range years {
		xs := byYear[y]
		q1 := filter(xs, func(s sample) bool { return !math.IsNaN(s.rank) && s.rank < 0.2 })
		rest := filter(xs, func(s sample) bool { return math.IsNaN(s.rank) || s.rank >= 0.2 })
		a, b, c := meanOf(xs, 20), meanOf(q1, 20), meanOf(rest, 20)
		fmt.Printf("| %s | %d | %s | %s (n=%d) | %s | %s |\n", y, len(xs), pct(a), pct(b), len(q1), pct(c), pct(c-b))
	}
}

func printRow(name string, xs []sample) {
	if len(xs) == 0 {
		return
	}
	fmt.Printf("| %s | %d | %s | %s | %s | %s |\n", name, len(xs), pct(meanOf(xs, 5)), pct(meanOf(xs, 10)), pct(meanOf(xs, 20)), pct(winRate(xs, 10)))
}

func filter(xs []sample, keep func(sample) bool) []sample {
	var out []sample
	for _, x := range xs {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

// buildStrategies は cmd/wbjp/root.go と同じ手順で strategies.toml から戦略を作る。
func buildStrategies(configDir string, stratCfg *wbjpcfg.StrategiesConfig) ([]strategy.Strategy, map[string]float64) {
	raw, err := os.ReadFile(filepath.Join(configDir, "strategies.toml"))
	must(err)
	var doc struct {
		Strategies []map[string]any `toml:"strategies"`
	}
	must(toml.Unmarshal(raw, &doc))
	var strats []strategy.Strategy
	weights := map[string]float64{}
	for i, se := range stratCfg.Strategies {
		if !se.IsEnabled() {
			continue
		}
		entry := map[string]any{}
		if i < len(doc.Strategies) {
			entry = doc.Strategies[i]
		}
		weight := se.Weight
		if _, ok := entry["weight"]; !ok {
			weight = 1.0
		}
		weights[se.Name] = weight
		params := map[string]any{}
		for k, v := range entry {
			if k != "name" && k != "enabled" && k != "weight" {
				params[k] = v
			}
		}
		s, err := strategy.Create(se.Name, params)
		must(err)
		strats = append(strats, s)
	}
	return strats, weights
}

// loadMargin は cmd/wbjp/margin.go と同じ手順で信用残を読む。
func loadMargin(archiveDir string, symbols []string, lagDays int) *strategy.MarginBook {
	arch := archive.NewArchive(archiveDir)
	ep := archive.MustEndpoint("markets_margin_interest")
	want := map[string]string{}
	for _, sym := range symbols {
		code, isIndex, err := data.ToJQuantsCode(sym)
		if err != nil || isIndex {
			continue
		}
		if len(code) == 4 {
			code += "0"
		}
		want[code] = sym
	}
	frame, err := arch.ReadWhere(ep, archive.ReadOptions{
		Columns: []string{"Code", "LongVol", "ShrtVol"},
		Keep:    func(row archive.RowView) bool { _, ok := want[row.Text("Code")]; return ok },
	})
	must(err)
	records := map[string][]strategy.MarginRecord{}
	for i := 0; i < frame.Height(); i++ {
		sym := want[text(frame.Get(i, "Code"))]
		long, err1 := strconv.ParseFloat(text(frame.Get(i, "LongVol")), 64)
		short, err2 := strconv.ParseFloat(text(frame.Get(i, "ShrtVol")), 64)
		date := text(frame.Get(i, ep.DateColumn))
		if sym == "" || date == "" || err1 != nil || err2 != nil {
			continue
		}
		records[sym] = append(records[sym], strategy.MarginRecord{Date: date, Long: long, Short: short})
	}
	return strategy.NewMarginBookWithLag(records, lagDays)
}

// fwdReturn は i の翌日寄付で建て、k 日後（翌日を 1 日目）の終値で決済したリターン。
func fwdReturn(s *series, i, k int) (float64, bool) {
	entry, exit := i+1, i+k
	if exit >= len(s.close) || s.open[entry] <= 0 {
		return 0, false
	}
	return s.close[exit]/s.open[entry] - 1, true
}

func f(d decimal.Decimal) float64 { v, _ := d.Float64(); return v }

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

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

func meanOf(xs []sample, k int) float64 {
	vals := make([]float64, 0, len(xs))
	for _, x := range xs {
		vals = append(vals, x.fwd[k])
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
