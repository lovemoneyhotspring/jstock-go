// 使い捨て: binance/BTC の 3MA レジーム積立(internal/accum)を日本株の日足に当て、
// jstock-go の積立戦略(pkg/accum/tactics)と同じ土俵で比べる。
//
//	go run ./scratch/accum-3ma -symbols "^TOPIX" -years 5
//	go run ./scratch/accum-3ma -universe -years 5
//
// 土俵の揃え方:
//   - どの戦略も「毎営業日、月額予算/その月の営業日数 × 倍率」を投下し、翌日寄付で約定
//     （jstock-go の週まとめリリースは投下日をずらすだけで総額を変えないので外す）
//   - 平均取得単価は拠出額のスケールに依存しないので、拠出額が違っても単価比較は成立する
//   - 資金の重さは invested(単純DCA比の総拠出額)と IRR(金額加重)で見る
//   - MA200 の助走 200 本は全戦略から除外する（3MA は助走中の底を取り返せない）
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

const warmupBars = 200

// barsPerYear は 1 年あたりの足の本数。株は年 245 営業日、暗号資産は 365 日。
var barsPerYear = 245.0

// ---------------------------------------------------------------------------
// binance/BTC internal/accum の移植（3MA レジーム）
// ---------------------------------------------------------------------------

const (
	stBullExtended = "bull_extended"
	stBullPullback = "bull_pullback"
	stBullDeep     = "bull_deep"
	stRegimeBreak  = "regime_break"
	stBearRebound  = "bear_rebound"
	stBearDeep     = "bear_deep"
	stUnknown      = ""
)

var mult3MAMap = map[string]float64{
	stBullExtended: 0.8,
	stBullPullback: 1.2,
	stBullDeep:     1.8,
	stRegimeBreak:  1.5,
	stBearRebound:  2.0,
}

// classify は 20/50/200 日線から 6 状態に分類する（末尾 i 本目の時点）。
func classify(close []float64, i int) (string, float64) {
	if i+1 < 200 {
		return stUnknown, math.NaN()
	}
	p := close[i]
	f := mean(close[i-19 : i+1])
	m := mean(close[i-49 : i+1])
	s := mean(close[i-199 : i+1])
	dev := (p - s) / s
	switch {
	case p > s && m > s:
		switch {
		case p > f && f > m:
			return stBullExtended, dev
		case p > m:
			return stBullPullback, dev
		default:
			return stBullDeep, dev
		}
	case p < s && m > s:
		return stRegimeBreak, dev
	case m < s:
		if p > f {
			return stBearRebound, dev
		}
		return stBearDeep, dev
	default:
		return stBullPullback, dev
	}
}

// multipliers3MA は状態別倍率。bear_deep だけ MA200 乖離に比例させる。
func multipliers3MA(bars []domain.Bar) []float64 {
	close := closes(bars)
	out := make([]float64, len(close))
	for i := range out {
		state, dev := classify(close, i)
		switch {
		case state == stUnknown:
			out[i] = fallbackMult(close, i)
		case state == stBearDeep:
			out[i] = math.Min(2.5+math.Max(0, -dev-0.10)*8.0, 4.0)
		default:
			out[i] = mult3MAMap[state]
		}
	}
	return out
}

// fallbackMult は MA200 が確定しない期間のつなぎ（MA50 乖離のみ）。
func fallbackMult(close []float64, i int) float64 {
	if i+1 < 50 {
		return 1.0
	}
	ma := mean(close[i-49 : i+1])
	dev := (close[i] - ma) / ma
	return clamp(1-dev*2.0, 0.6, 4.0)
}

// multipliersMADev は accum_va_ma の 50日MA 乖離倍率（バリュー平均法の部分は外す）。
func multipliersMADev(bars []domain.Bar) []float64 {
	close := closes(bars)
	out := make([]float64, len(close))
	for i := range out {
		if i+1 < 30 {
			out[i] = 1.0
			continue
		}
		n := 50
		if i+1 < n {
			n = i + 1
		}
		ma := mean(close[i-n+1 : i+1])
		dev := (close[i] - ma) / ma
		out[i] = clamp(1-dev*1.5, 0.4, 2.5)
	}
	return out
}

// ---------------------------------------------------------------------------
// シミュレーション
// ---------------------------------------------------------------------------

type result struct {
	Invested float64
	Units    float64
	AvgCost  float64
	Terminal float64
	IRR      float64
	BoostDay float64 // 倍率 > 1 だった日の割合

	// Monthly は月ごとの拠出額（基準 100,000 円/月）。前後の欠けた月は含めない。
	Monthly []float64
}

// simulate は毎営業日 base*mult を投下し、翌日寄付で約定させる。
func simulate(bars []domain.Bar, mults []float64, start int) result {
	// 月ごとの営業日数（評価区間の中で数える）
	perMonth := map[string]int{}
	for _, b := range bars[start:] {
		perMonth[b.Date[:7]]++
	}
	const monthly = 100000.0

	var r result
	var flowDays, flows []float64
	paid := map[string]float64{}
	boost := 0
	for i := start; i < len(bars); i++ {
		base := monthly / float64(perMonth[bars[i].Date[:7]])
		amt := base * mults[i]
		fill := fillPrice(bars, i)
		if fill <= 0 || amt <= 0 {
			continue
		}
		r.Invested += amt
		r.Units += amt / fill
		paid[bars[i].Date[:7]] += amt
		if mults[i] > 1 {
			boost++
		}
		flowDays = append(flowDays, float64(i-start))
		flows = append(flows, -amt)
	}
	if r.Units <= 0 {
		return r
	}
	last, _ := bars[len(bars)-1].Close.Float64()
	r.AvgCost = r.Invested / r.Units
	r.Terminal = r.Units * last
	r.BoostDay = float64(boost) / float64(len(bars)-start) * 100

	// 本数を暦日に直す（株は 1 本 = 約 1.49 暦日、暗号資産は 1 本 = 1 暦日）
	span := float64(len(bars)-start) * 365.0 / barsPerYear
	for i := range flowDays {
		flowDays[i] = flowDays[i] * 365.0 / barsPerYear
	}
	flowDays = append(flowDays, span)
	flows = append(flows, r.Terminal)
	r.IRR = irr(flowDays, flows)

	// 月ごとの拠出額。窓の端で欠けている月は除く。
	months := make([]string, 0, len(paid))
	for m := range paid {
		months = append(months, m)
	}
	sort.Strings(months)
	if len(months) > 2 {
		for _, m := range months[1 : len(months)-1] {
			r.Monthly = append(r.Monthly, paid[m])
		}
	}
	return r
}

func fillPrice(bars []domain.Bar, i int) float64 {
	if i < len(bars)-1 {
		if o, _ := bars[i+1].Open.Float64(); o > 0 {
			return o
		}
		c, _ := bars[i+1].Close.Float64()
		return c
	}
	c, _ := bars[i].Close.Float64()
	return c
}

// irr は年率の内部収益率を二分法で解く。
func irr(days, flows []float64) float64 {
	f := func(rate float64) float64 {
		sum := 0.0
		for i := range flows {
			sum += flows[i] / math.Pow(1+rate, days[i]/365.0)
		}
		return sum
	}
	lo, hi := -0.95, 5.0
	if f(lo)*f(hi) > 0 {
		return math.NaN()
	}
	for range 200 {
		mid := (lo + hi) / 2
		if f(lo)*f(mid) <= 0 {
			hi = mid
		} else {
			lo = mid
		}
	}
	return (lo + hi) / 2
}

// ---------------------------------------------------------------------------
// 戦略の一覧
// ---------------------------------------------------------------------------

type rule struct {
	name  string
	mults func([]domain.Bar) []float64
}

func rules() []rule {
	bs4, _ := tactics.NewBearStack(4, 0, 0, 0)
	bs2, _ := tactics.NewBearStack(2, 0, 0, 0)
	sl, _ := tactics.NewStackLadder(map[int]float64{3: 1.5, 5: 2.0, 6: 4.0}, 0, 0, 0)
	dd, _ := tactics.NewDrawdownLadder([]float64{0.10, 0.20, 0.30}, []float64{2, 3, 4}, true, 200)
	ddNo, _ := tactics.NewDrawdownLadder([]float64{0.10, 0.20, 0.30}, []float64{2, 3, 4}, false, 200)
	return []rule{
		{"DCA(基準)", func(b []domain.Bar) []float64 { return ones(len(b)) }},
		{"bear_stack x4", bs4.Multipliers},
		{"bear_stack x2", bs2.Multipliers},
		{"stack_ladder", sl.Multipliers},
		{"drawdown_ladder", dd.Multipliers},
		{"drawdown(素)", ddNo.Multipliers},
		{"BTC 3MA", multipliers3MA},
		{"BTC MA50乖離", multipliersMADev},
	}
}

// ---------------------------------------------------------------------------

type stat struct {
	imp      []float64 // DCA 比の平均取得単価改善率(%)
	invested []float64 // DCA 比の総拠出額
	irrGap   []float64 // DCA 比の IRR 差(%pt)
	boost    []float64

	monMean []float64 // 月あたりの拠出額（基準 100,000 円/月）
	monP95  []float64 // 上位 5% の月
	monMax  []float64 // 最も重かった月
	mon12   []float64 // 最も重かった 12 ヶ月の月平均
}

// evaluate は各系列を窓に切り、全戦略を同じ窓で回す。
func evaluate(seriesList [][]domain.Bar, years float64, nWindows int) []stat {
	rs := rules()
	stats := make([]stat, len(rs))

	for _, bars := range seriesList {
		// 窓の切り出し。年数 0 なら全履歴。
		type win struct{ lo, hi int }
		var wins []win
		span := int(years * barsPerYear)
		need := warmupBars + span
		if years <= 0 || len(bars) <= need {
			wins = []win{{0, len(bars)}}
		} else {
			stride := (len(bars) - need) / (nWindows - 1)
			if stride < 1 {
				stride = 1
			}
			for lo := 0; lo+need <= len(bars) && len(wins) < nWindows; lo += stride {
				wins = append(wins, win{lo, lo + need})
			}
		}

		for _, w := range wins {
			seg := bars[w.lo:w.hi]
			base := simulate(seg, ones(len(seg)), warmupBars)
			if base.Units <= 0 {
				continue
			}
			for i, r := range rs {
				got := simulate(seg, r.mults(seg), warmupBars)
				if got.Units <= 0 {
					continue
				}
				stats[i].imp = append(stats[i].imp, (base.AvgCost-got.AvgCost)/base.AvgCost*100)
				stats[i].invested = append(stats[i].invested, got.Invested/base.Invested)
				stats[i].irrGap = append(stats[i].irrGap, (got.IRR-base.IRR)*100)
				stats[i].boost = append(stats[i].boost, got.BoostDay)
				if len(got.Monthly) > 0 {
					stats[i].monMean = append(stats[i].monMean, meanOf(got.Monthly))
					stats[i].monP95 = append(stats[i].monP95, quantile(got.Monthly, 0.95))
					stats[i].monMax = append(stats[i].monMax, maxOf(got.Monthly))
					stats[i].mon12 = append(stats[i].mon12, worstRun(got.Monthly, 12))
				}
			}
		}
	}
	return stats
}

// readCSV は date,open,high,low,close[,volume] の CSV を足として読む。
func readCSV(path string) ([]domain.Bar, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bars []domain.Bar
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if i == 0 || len(f) < 5 {
			continue
		}
		num := make([]float64, 4)
		ok := true
		for j := range num {
			v, err := strconv.ParseFloat(f[j+1], 64)
			if err != nil || v <= 0 {
				ok = false
				break
			}
			num[j] = v
		}
		if !ok {
			continue
		}
		bar, err := domain.NewBar(path, f[0],
			decimal.NewFromFloat(num[0]), decimal.NewFromFloat(num[1]),
			decimal.NewFromFloat(num[2]), decimal.NewFromFloat(num[3]), decimal.NewFromFloat(1))
		if err == nil {
			bars = append(bars, bar)
		}
	}
	return bars, nil
}

func main() {
	symbols := flag.String("symbols", "^TOPIX", "対象銘柄（カンマ区切り）")
	universe := flag.Bool("universe", false, "data/bars の日本株すべてを使う")
	sample := flag.Int("sample", 0, "universe から何銘柄おきに拾うか（0 なら全部）")
	years := flag.Float64("years", 5, "1 窓あたりの年数。0 なら全履歴を 1 窓とする")
	nWindows := flag.Int("windows", 12, "窓の数")
	budget := flag.Float64("budget", 20000, "基準となる毎月の積立額（円）")
	barsDir := flag.String("bars", "", "足の置き場（既定: 設定の data/bars）")
	csvPath := flag.String("csv", "", "足を CSV から読む（date,open,high,low,close,volume）")
	flag.Parse()

	if *csvPath != "" {
		barsPerYear = 365 // 暗号資産は年 365 本
	}

	dir := settings.LoadAppSettings().BarsDir()
	if *barsDir != "" {
		dir = *barsDir
	}
	store := data.NewBarStore(dir)
	var syms []string
	if *universe {
		all, err := store.Symbols()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		step := max(1, *sample)
		for i, s := range all {
			if s[0] >= '0' && s[0] <= '9' && i%step == 0 { // 個別株（指数・ETF を除く）
				syms = append(syms, s)
			}
		}
	} else {
		syms = strings.Split(*symbols, ",")
	}

	var seriesList [][]domain.Bar
	if *csvPath != "" {
		syms = nil
		for _, p := range strings.Split(*csvPath, ",") {
			bars, err := readCSV(strings.TrimSpace(p))
			if err != nil || len(bars) < warmupBars+100 {
				fmt.Fprintln(os.Stderr, "csv を読めない/短すぎる:", p, err)
				os.Exit(1)
			}
			fmt.Printf("# %s: %d 本 %s .. %s\n", p, len(bars), bars[0].Date, bars[len(bars)-1].Date)
			seriesList = append(seriesList, bars)
		}
	}
	for _, sym := range syms {
		bars, err := store.Read(strings.TrimSpace(sym), "", "")
		if err != nil || len(bars) < warmupBars+250 {
			continue
		}
		seriesList = append(seriesList, bars)
	}

	stats := evaluate(seriesList, *years, *nWindows)
	usable := len(seriesList)
	rs := rules()

	label := fmt.Sprintf("%d 銘柄 / %.0f 年窓", usable, *years)
	if *years <= 0 {
		label = fmt.Sprintf("%d 銘柄 / 全履歴", usable)
	}
	fmt.Printf("積立ロジック比較（%s、助走 %d 本を除外、翌日の始値で約定）\n", label, warmupBars)
	fmt.Printf("imp = 単純 DCA に対する平均取得単価の改善率（正なら安く買えている。拠出額のスケールには依存しない）\n")
	fmt.Printf("inv = DCA 比の総拠出額 / IRR = 金額加重収益率の DCA との差 / 効率 = imp ÷ 追加拠出\n\n")
	fmt.Printf("%-16s %8s %8s %8s %8s %7s %7s %7s %7s\n",
		"戦略", "imp平均", "imp中央", "勝率", "最悪", "inv", "IRR差", "効率", "発動日")
	fmt.Println(strings.Repeat("-", 90))
	for i, r := range rs {
		s := stats[i]
		if len(s.imp) == 0 {
			continue
		}
		inv := meanOf(s.invested)
		eff := math.NaN()
		if inv > 1.0001 {
			eff = meanOf(s.imp) / (inv - 1)
		}
		fmt.Printf("%-16s %+7.2f%% %+7.2f%% %7.0f%% %+7.2f%% %6.2fx %+6.2f %7s %6.0f%%\n",
			r.name, meanOf(s.imp), median(s.imp), winRate(s.imp), minOf(s.imp),
			inv, meanOf(s.irrGap), fmtEff(eff), meanOf(s.boost))
	}
	fmt.Printf("\n窓の数 = %d\n", len(stats[0].imp))

	// 資金の表。基準額を実際の円に直して、月ごとの負担を出す。
	scale := *budget / 100000.0
	fmt.Printf("\n必要な積立額（1 銘柄あたり。基準 = 毎月 %s 円の定額積立）\n", comma(*budget))
	fmt.Printf("平均 = ならした月額 / 上位5%% = 重い月の目安 / 最悪の月 = 窓の中で最も重かった月\n")
	fmt.Printf("最悪の12ヶ月 = 最も重かった連続 12 ヶ月の月平均（暴落がまたぐぶん）\n\n")
	fmt.Printf("%-16s %12s %12s %12s %12s\n", "戦略", "平均/月", "上位5%/月", "最悪の月", "最悪の12ヶ月")
	fmt.Println(strings.Repeat("-", 68))
	for i, r := range rs {
		s := stats[i]
		if len(s.monMean) == 0 {
			continue
		}
		fmt.Printf("%-16s %12s %12s %12s %12s\n", r.name,
			yen(meanOf(s.monMean)*scale), yen(meanOf(s.monP95)*scale),
			yen(maxOf(s.monMax)*scale), yen(maxOf(s.mon12)*scale))
	}
}

func fmtEff(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%+.2f", v)
}

func closes(bars []domain.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i], _ = b.Close.Float64()
	}
	return out
}

func ones(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 1
	}
	return out
}

func mean(x []float64) float64 {
	s := 0.0
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

func meanOf(x []float64) float64 { return mean(x) }

func median(x []float64) float64 {
	c := append([]float64(nil), x...)
	sort.Float64s(c)
	return c[len(c)/2]
}

func minOf(x []float64) float64 {
	m := x[0]
	for _, v := range x {
		m = math.Min(m, v)
	}
	return m
}

func winRate(x []float64) float64 {
	w := 0
	for _, v := range x {
		if v > 0 {
			w++
		}
	}
	return float64(w) / float64(len(x)) * 100
}

func clamp(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }

func maxOf(x []float64) float64 {
	m := x[0]
	for _, v := range x {
		m = math.Max(m, v)
	}
	return m
}

func quantile(x []float64, q float64) float64 {
	c := append([]float64(nil), x...)
	sort.Float64s(c)
	i := int(math.Ceil(q*float64(len(c)))) - 1
	return c[max(0, min(i, len(c)-1))]
}

// worstRun は連続 n ヶ月の月平均が最も重い区間を返す。
func worstRun(x []float64, n int) float64 {
	if len(x) < n {
		return meanOf(x)
	}
	best := 0.0
	sum := 0.0
	for i, v := range x {
		sum += v
		if i >= n {
			sum -= x[i-n]
		}
		if i >= n-1 {
			best = math.Max(best, sum/float64(n))
		}
	}
	return best
}

// yen は円を 3 桁区切りで返す。
func yen(v float64) string { return comma(math.Round(v)) }

func comma(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
