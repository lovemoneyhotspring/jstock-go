// Package basket は複数銘柄への配分（バスケット）。「その月の予算をどの銘柄に
// いくら振るか」を決める。
//
// なぜ積立型戦略（tactics）と分けるのか:
// tactics は1銘柄の足だけを見て「今日は何倍買うか」を返す。銘柄をまたぐ判断
// （指数の構成比で買う、コアとサテライトに分ける）はそこに入れられない。
// そこで配分は銘柄をまたぐ層として独立させ、倍率は従来どおり銘柄ごとの戦略に任せる。
//
//	予算 × 配分比率（この層） × 倍率（戦略） = その日その銘柄の投下額
//
// 足が無い銘柄（上場前・廃止後・未取得）には振れない。その分は現金として残さず、
// 足のある銘柄の間で比率を正規化して投じる。現金の滞留は平均取得単価を確実に
// 悪化させるため。
//
// 投下額が時期ごとに違うので単純な総リターンでは比較にならない。BasketResult は
// 内部収益率（XIRR）と、時間加重の評価額指数から求めた最大ドローダウンを持ち、
// 同じ資金の流れを基準銘柄に投じた場合と並べる。
package basket

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// WeightEntry は「この日から有効」な比率表。
//
// Effective が空文字なら常に有効（時間で変わらない配分）。日付は足と同じ
// "YYYY-MM-DD" の文字列で持つ——accum の他の層（plan / execute）も足の日付を
// 文字列で扱っており、辞書順がそのまま日付順になるため。
type WeightEntry struct {
	Effective string
	Weights   map[string]float64
}

// WeightSchedule は有効開始日の昇順に並んだ比率表の列。
//
// 今の設定は固定比率（1 表）だが、指数の入れ替えなど比率が日付で変わる配分も
// この形で表せる。
type WeightSchedule struct {
	Entries []WeightEntry
}

// Static は時間で変わらない配分。
func Static(weights map[string]float64) WeightSchedule {
	return WeightSchedule{Entries: []WeightEntry{{Effective: "", Weights: copyWeights(weights)}}}
}

// FromPairs は「有効開始日 → 比率」の組から配分表を作る。比率は正でなければならない。
func FromPairs(pairs []WeightEntry) (WeightSchedule, error) {
	entries := make([]WeightEntry, 0, len(pairs))
	for _, p := range pairs {
		for symbol, value := range p.Weights {
			if value < 0 {
				return WeightSchedule{}, fmt.Errorf("比率は負にできません: %s=%v", symbol, value)
			}
		}
		entries = append(entries, WeightEntry{Effective: p.Effective, Weights: copyWeights(p.Weights)})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Effective < entries[j].Effective })
	return WeightSchedule{Entries: entries}, nil
}

// At は date に有効な比率。開始日と同じ日は **まだ** 前の表を使う
// （表の切り替えは前日の引けで判断できないため、当日から適用すると先読みになる）。
func (s WeightSchedule) At(date string) map[string]float64 {
	current := map[string]float64{}
	for _, e := range s.Entries {
		if e.Effective < date {
			current = e.Weights
		} else {
			break
		}
	}
	return current
}

// Symbols は一度でも登場する銘柄。足の取得対象になる。
func (s WeightSchedule) Symbols() []string {
	seen := map[string]struct{}{}
	for _, e := range s.Entries {
		for symbol := range e.Weights {
			seen[symbol] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for symbol := range seen {
		out = append(out, symbol)
	}
	sort.Strings(out)
	return out
}

// Blend は固定のコアと組み合わせる。
//
// core を 1-satelliteShare で、この表を satelliteShare で持つ。
// コア・サテライト（指数 70% ＋ 個別 30%）を作る。
func (s WeightSchedule) Blend(core map[string]float64, satelliteShare float64) (WeightSchedule, error) {
	if satelliteShare <= 0 || satelliteShare > 1 {
		return WeightSchedule{}, fmt.Errorf("satellite_share は 0 より大きく 1 以下: %v", satelliteShare)
	}
	coreTotal := 0.0
	for _, v := range core {
		coreTotal += v
	}
	if len(core) > 0 && coreTotal <= 0 {
		return WeightSchedule{}, fmt.Errorf("core の比率の合計が正ではありません")
	}
	scaledCore := map[string]float64{}
	for symbol, v := range core {
		scaledCore[symbol] = v / coreTotal * (1 - satelliteShare)
	}

	entries := make([]WeightEntry, 0, len(s.Entries))
	for _, e := range s.Entries {
		total := 0.0
		for _, v := range e.Weights {
			total += v
		}
		merged := copyWeights(scaledCore)
		for symbol, v := range e.Weights {
			if total > 0 {
				merged[symbol] += v / total * satelliteShare
			}
		}
		entries = append(entries, WeightEntry{Effective: e.Effective, Weights: merged})
	}
	return WeightSchedule{Entries: entries}, nil
}

func copyWeights(src map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// DrawdownTilt は構成銘柄のうち「高値から深く下げているもの」へ配分を寄せる。
//
// 各銘柄の比率に 1 + strength × 下落率 を掛けてから正規化する。下落率は
// lookback 本の高値に対する割合（0〜1）。strength=2 なら 30% 下げた銘柄の比率が
// 1.6 倍になる。
//
// 戦略（tactics）と違い予算の総額は変えない。銘柄 **間** で配分を動かすだけなので
// 追加資金は要らない。
type DrawdownTilt struct {
	Strength float64
	Lookback int
}

// NewDrawdownTilt は傾斜の規則を作る。0 は「傾斜なし」の意味で使うので値域を確かめる。
func NewDrawdownTilt(strength float64, lookback int) (*DrawdownTilt, error) {
	if strength <= 0 {
		return nil, fmt.Errorf("strength は正の値: %v", strength)
	}
	if lookback < 2 {
		return nil, fmt.Errorf("lookback は 2 以上: %d", lookback)
	}
	return &DrawdownTilt{Strength: strength, Lookback: lookback}, nil
}

// Factor は日ごとの係数（1 以上）。高値未確定の期間はその時点までの高値で測る。
func (t *DrawdownTilt) Factor(bars []domain.Bar) []float64 {
	out := make([]float64, len(bars))
	for i := range bars {
		start := i - t.Lookback + 1
		if start < 0 {
			start = 0
		}
		peak := 0.0
		for j := start; j <= i; j++ {
			c, _ := bars[j].Close.Float64()
			if c > peak {
				peak = c
			}
		}
		close, _ := bars[i].Close.Float64()
		drawdown := 0.0
		if peak > 0 {
			drawdown = 1.0 - close/peak
		}
		if drawdown < 0 {
			drawdown = 0
		}
		if drawdown > 1 {
			drawdown = 1
		}
		out[i] = 1.0 + t.Strength*drawdown
	}
	return out
}

// BasketSettings はバスケット全体の設定。
type BasketSettings struct {
	// MonthlyBudget はバスケット全体の毎月の基本予算（円）。
	MonthlyBudget decimal.Decimal
	Schedule      WeightSchedule
	// Tactic は各銘柄に掛ける倍率戦略。nil なら定額。
	Tactic tactics.Tactic
	// Tilt は銘柄間で配分を傾ける規則。nil なら配分表どおり。
	Tilt *DrawdownTilt
}

// PlanRow は銘柄ごとの計画表の 1 行。配分比率を加えた plan.PlanRow。
type PlanRow struct {
	plan.PlanRow
	Weight float64
}

// BuildBasketPlan は銘柄ごとの計画表を作る。
//
// 銘柄単位の plan.BuildPlan をバスケットの予算で走らせ、日ごとの配分比率を掛ける。
// 比率はその日に足のある銘柄の間で正規化する。配分が 0 の銘柄は返り値に含めない。
func BuildBasketPlan(bars map[string][]domain.Bar, settings BasketSettings) (map[string][]PlanRow, error) {
	if len(bars) == 0 {
		return nil, fmt.Errorf("足がありません")
	}
	if !settings.MonthlyBudget.IsPositive() {
		return nil, fmt.Errorf("monthly_budget は正の値: %s", settings.MonthlyBudget)
	}
	tactic := settings.Tactic
	if tactic == nil {
		tactic = &tactics.Constant{}
	}

	// 全銘柄の営業日の和集合。バスケット共通の暦になる
	available := map[string]map[string]struct{}{}
	dateSet := map[string]struct{}{}
	for symbol, frame := range bars {
		days := make(map[string]struct{}, len(frame))
		for _, b := range frame {
			days[b.Date] = struct{}{}
			dateSet[b.Date] = struct{}{}
		}
		available[symbol] = days
	}
	dates := make([]string, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	// 傾斜の係数（銘柄 → 日付 → 係数）。傾斜が無ければ空のまま
	tiltFactor := map[string]map[string]float64{}
	if settings.Tilt != nil {
		for symbol, frame := range bars {
			factors := settings.Tilt.Factor(frame)
			byDate := make(map[string]float64, len(frame))
			for i, b := range frame {
				byDate[b.Date] = factors[i]
			}
			tiltFactor[symbol] = byDate
		}
	}

	// 日 × 銘柄の比率表
	weights := map[string]map[string]float64{}
	for symbol := range bars {
		weights[symbol] = make(map[string]float64, len(dates))
	}
	for _, date := range dates {
		raw := settings.Schedule.At(date)
		tilted := map[string]float64{}
		total := 0.0
		for symbol, w := range raw {
			days, known := available[symbol]
			if !known {
				continue
			}
			if _, present := days[date]; !present {
				continue
			}
			f := 1.0
			if byDate, ok := tiltFactor[symbol]; ok {
				if v, ok := byDate[date]; ok {
					f = v
				}
			}
			tilted[symbol] = w * f
			total += w * f
		}
		if total <= 0 {
			continue
		}
		for symbol, v := range tilted {
			weights[symbol][date] = v / total
		}
	}

	// 入金日はバスケット共通の暦で決める。銘柄ごとに決めると、途中上場した銘柄が
	// 上場初日に「その月の入金日」を持ってしまい、その月だけ予算が二重になる
	payday := map[string]bool{}
	seenMonth := map[string]bool{}
	for _, date := range dates {
		month := date[:7]
		if !seenMonth[month] {
			seenMonth[month] = true
			payday[date] = true
		}
	}

	budget, _ := settings.MonthlyBudget.Float64()
	plans := map[string][]PlanRow{}
	for symbol, frame := range bars {
		byDate := weights[symbol]
		any := false
		for _, b := range frame {
			if byDate[b.Date] > 0 {
				any = true
				break
			}
		}
		if !any {
			continue
		}

		base, err := plan.BuildPlan(frame, tactic, settings.MonthlyBudget)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", symbol, err)
		}
		rows := make([]PlanRow, 0, len(base.Rows))
		for _, r := range base.Rows {
			w := byDate[r.Date]
			baseAmount := 0.0
			if payday[r.Date] {
				baseAmount = math.Floor(budget * w)
			}
			extraFloat, _ := r.Extra.Float64()
			extra := math.Floor(extraFloat * w)
			scaled := r
			scaled.Base = decimal.NewFromInt(int64(baseAmount))
			scaled.Extra = decimal.NewFromInt(int64(extra))
			scaled.Amount = scaled.Base.Add(scaled.Extra)
			rows = append(rows, PlanRow{PlanRow: scaled, Weight: w})
		}
		plans[symbol] = rows
	}
	return plans, nil
}

// -- 検証 -----------------------------------------------------------------

// Leg は 1 本の資金の流れの結果（バスケット本体、または基準銘柄）。
type Leg struct {
	Contributed   float64
	TerminalValue float64
	XIRR          float64
	// MaxDrawdown は時間加重の評価額指数の最大下落率（0〜1）。
	MaxDrawdown float64
}

// TotalReturn は総投入額に対する期末評価額の比。
func (l Leg) TotalReturn() float64 {
	if l.Contributed == 0 {
		return 0
	}
	return l.TerminalValue/l.Contributed - 1.0
}

// BasketResult はバスケットの検証結果。
type BasketResult struct {
	Start  string
	End    string
	Basket Leg
	// Benchmark は基準銘柄に同じ資金の流れを投じた場合。指定が無ければ nil。
	Benchmark *Leg
	// Symbols は一度でも投下した銘柄。
	Symbols     []string
	BoostedDays int
}

// Summary は表示・JSON 用の要約。
func (r BasketResult) Summary() map[string]any {
	out := map[string]any{
		"期間":     fmt.Sprintf("%s 〜 %s", r.Start, r.End),
		"総投入額":   r.Basket.Contributed,
		"期末評価額":  r.Basket.TerminalValue,
		"総リターン":  r.Basket.TotalReturn(),
		"XIRR":   r.Basket.XIRR,
		"最大DD":   r.Basket.MaxDrawdown,
		"銘柄数":    len(r.Symbols),
		"増額発動日数": r.BoostedDays,
	}
	if r.Benchmark != nil {
		out["基準 期末評価額"] = r.Benchmark.TerminalValue
		out["基準 XIRR"] = r.Benchmark.XIRR
		out["基準 最大DD"] = r.Benchmark.MaxDrawdown
	}
	return out
}

// fills は約定価格＝翌営業日の寄付（無ければ翌日終値、最終日は当日終値）。
func fills(bars []domain.Bar) map[string]float64 {
	out := make(map[string]float64, len(bars))
	for i, b := range bars {
		var price float64
		if i+1 < len(bars) {
			price, _ = bars[i+1].Open.Float64()
			if price <= 0 {
				price, _ = bars[i+1].Close.Float64()
			}
		} else {
			price, _ = b.Close.Float64()
		}
		out[b.Date] = price
	}
	return out
}

func closes(bars []domain.Bar) map[string]float64 {
	out := make(map[string]float64, len(bars))
	for _, b := range bars {
		c, _ := b.Close.Float64()
		out[b.Date] = c
	}
	return out
}

// leg は評価額の推移から内部収益率と最大DDを出す。
//
// dates は営業日の昇順、cashflows はそれに対応する投下額（円）、values は
// その日の引け時点の評価額。
func leg(dates []string, cashflows []float64, values map[string]float64) Leg {
	n := len(dates)
	series := make([]float64, n)
	for i, d := range dates {
		series[i] = values[d]
	}
	contributed := 0.0
	for _, c := range cashflows {
		contributed += c
	}
	terminal := 0.0
	if n > 0 {
		terminal = series[n-1]
	}

	// 時間加重指数: 当日の投下を除いた評価額の変化率をつなぐ。投下は翌営業日の
	// 寄付で約定するので、値上がり率は (今日の評価額)/(昨日の評価額+昨日の投下額)。
	index := 1.0
	peak := 0.0
	maxDD := 0.0
	for i := 0; i < n; i++ {
		prev := 0.0
		if i > 0 {
			prev = series[i-1] + cashflows[i-1]
		}
		ratio := 1.0
		if prev > 0 {
			ratio = series[i] / prev
		}
		index *= ratio
		if index > peak {
			peak = index
		}
		if peak > 0 {
			if dd := 1.0 - index/peak; dd > maxDD {
				maxDD = dd
			}
		}
	}

	negated := make([]float64, n)
	for i, c := range cashflows {
		negated[i] = -c
	}
	return Leg{
		Contributed:   math.Trunc(contributed),
		TerminalValue: terminal,
		XIRR:          XIRR(dates, negated, terminal),
		MaxDrawdown:   maxDD,
	}
}

// XIRR は不定期キャッシュフローの内部収益率（年率）。
//
// flows は投下を負で渡す。最終日に terminal を正で加えて解く。
// 二分法で解くので初期値に依存せず、符号が変わらない（解が無い）ときは 0 を返す。
// 専用ライブラリを使わないのは、必要なのがこの 1 本だけで、依存を足すほどの
// ものではないため。
func XIRR(dates []string, flows []float64, terminal float64) float64 {
	n := len(dates)
	if n == 0 {
		return 0
	}
	first, err := time.Parse("2006-01-02", dates[0])
	if err != nil {
		return 0
	}
	years := make([]float64, n)
	for i, d := range dates {
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			return 0
		}
		years[i] = t.Sub(first).Hours() / 24.0 / 365.0
	}
	cf := make([]float64, n)
	copy(cf, flows)
	cf[n-1] += terminal

	hasNeg, hasPos, scale := false, false, 0.0
	for _, v := range cf {
		if v < 0 {
			hasNeg = true
		}
		if v > 0 {
			hasPos = true
		}
		scale += math.Abs(v)
	}
	if !hasNeg || !hasPos {
		return 0
	}

	npv := func(rate float64) float64 {
		sum := 0.0
		for i, v := range cf {
			sum += v / math.Pow(1.0+rate, years[i])
		}
		return sum
	}

	// 打ち切りは金額に対する相対誤差で。絶対値だと、割引が効く長期の系列で
	// NPV が最初から小さく、粗い解で止まってしまう
	tolerance := 1e-9 * scale
	lo, hi := -0.99, 10.0
	fLo := npv(lo)
	if fLo*npv(hi) > 0 {
		return 0
	}
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		fMid := npv(mid)
		if math.Abs(fMid) < tolerance {
			return mid
		}
		if fLo*fMid < 0 {
			hi = mid
		} else {
			lo, fLo = mid, fMid
		}
	}
	return (lo + hi) / 2
}

// SimulateBasket は計画どおりに買った結果を返す。基準銘柄には同じ日に同じ額を投じる。
func SimulateBasket(bars map[string][]domain.Bar, plans map[string][]PlanRow, benchmark []domain.Bar) (*BasketResult, error) {
	if len(plans) == 0 {
		return nil, fmt.Errorf("計画表が空です（配分が 0 か、足がありません）")
	}

	valueByDate := map[string]float64{}
	flowByDate := map[string]float64{}
	dateSet := map[string]struct{}{}
	boosted := 0
	totalUnits := map[string]float64{}

	for symbol, rows := range plans {
		frame, ok := bars[symbol]
		if !ok {
			continue
		}
		fill := fills(frame)
		close := closes(frame)
		units := 0.0
		for _, r := range rows {
			amount, _ := r.Amount.Float64()
			// 投下日の翌営業日に約定するので、当日の評価は前日までの口数で
			held := units
			price := fill[r.Date]
			if price > 0 {
				units += amount / price
			}
			valueByDate[r.Date] += held * close[r.Date]
			flowByDate[r.Date] += amount
			dateSet[r.Date] = struct{}{}
			if r.Extra.IsPositive() {
				boosted++
			}
		}
		totalUnits[symbol] = units
	}

	dates := make([]string, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	// 配分が始まる前（最初の投下より前）の日は結果に含めない。起点が何年も前に
	// あると XIRR の割引が効きすぎて解が不安定になる
	firstFlow := ""
	for _, d := range dates {
		if flowByDate[d] > 0 {
			firstFlow = d
			break
		}
	}
	if firstFlow == "" {
		return nil, fmt.Errorf("一度も投下がありません")
	}
	kept := make([]string, 0, len(dates))
	for _, d := range dates {
		if d >= firstFlow {
			kept = append(kept, d)
		}
	}
	dates = kept

	last := dates[len(dates)-1]
	// 最終日の評価は当日引けの口数で（未約定分は当日終値で買えたとみなす）
	terminal := 0.0
	for symbol, units := range totalUnits {
		frame := bars[symbol]
		lastClose := 0.0
		for _, b := range frame {
			if b.Date <= last {
				lastClose, _ = b.Close.Float64()
			}
		}
		terminal += units * lastClose
	}
	valueByDate[last] = terminal

	cashflows := make([]float64, len(dates))
	for i, d := range dates {
		cashflows[i] = flowByDate[d]
	}
	basket := leg(dates, cashflows, valueByDate)

	var benchLeg *Leg
	if len(benchmark) > 0 {
		bFill := fills(benchmark)
		bClose := closes(benchmark)
		bDates := make([]string, 0, len(benchmark))
		for _, b := range benchmark {
			if b.Date >= firstFlow {
				bDates = append(bDates, b.Date)
			}
		}
		if len(bDates) > 0 {
			bFlows := make([]float64, len(bDates))
			bValues := map[string]float64{}
			units := 0.0
			for i, d := range bDates {
				amount := flowByDate[d]
				bFlows[i] = amount
				bValues[d] = units * bClose[d]
				if price := bFill[d]; price > 0 {
					units += amount / price
				}
			}
			bLast := bDates[len(bDates)-1]
			bValues[bLast] = units * bClose[bLast]
			l := leg(bDates, bFlows, bValues)
			benchLeg = &l
		}
	}

	symbols := make([]string, 0, len(plans))
	for symbol, rows := range plans {
		sum := decimal.Zero
		for _, r := range rows {
			sum = sum.Add(r.Amount)
		}
		if sum.IsPositive() {
			symbols = append(symbols, symbol)
		}
	}
	sort.Strings(symbols)

	return &BasketResult{
		Start:       dates[0],
		End:         last,
		Basket:      basket,
		Benchmark:   benchLeg,
		Symbols:     symbols,
		BoostedDays: boosted,
	}, nil
}
