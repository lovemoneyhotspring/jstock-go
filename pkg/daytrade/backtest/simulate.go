package backtest

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/fees"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

const dayLayout = "2006-01-02"

// FillModel は建値と手仕舞い値の決め方。
//
// 順位付け（ギャップ）と株数は 9:00 の価格（日足の寄付）で決まり、ここで決めるのは
// 「いくらで建って、いくらで手仕舞えたか」だけ。日足しか無ければ寄付と引け（OpenCloseFill）。
// 分足を入れるときは、9:01 の足や 15:20 の足を返す実装をここに差し替える——順位付けと
// 日次の集計はそのままでよい。
type FillModel interface {
	// Fill は建値・手仕舞い値。ok が偽ならその日その銘柄は建てない（足が無い等）。
	Fill(r Row) (entry, exit float64, ok bool)
}

// OpenCloseFill は寄付で建てて引けで手仕舞う（日足だけの近似。滑りなし）。
type OpenCloseFill struct{}

// Fill は寄付と引け。
func (OpenCloseFill) Fill(r Row) (float64, float64, bool) {
	return r.Open, r.Close, r.Open > 0 && r.Close > 0
}

// Options は検証の指定。ゼロ値は「寄付・引けで約定」「寄付の遅れは見ない」。
type Options struct {
	Fill FillModel
	// Opened は「その日 09:00 の板寄せで寄ったか」。known が偽なら判定しない
	// （分足の無い期間）。signal.skip_opened を検証で再現するために渡す
	// （MinuteBars.Opened がそのまま入る）。nil なら設定が真でも絞らない。
	Opened func(day time.Time, code string) (opened, known bool)
}

func (o Options) fill() FillModel {
	if o.Fill == nil {
		return OpenCloseFill{}
	}
	return o.Fill
}

// Trade は 1 取引（ある日・ある銘柄を建てて同日に手仕舞う）。
//
// 危険信号で縮めた日は金額の列（Shares・Amount・Fees・Gross・PnL）に Scale を掛けてある
// （Daily と同じ近似。単元の切り捨てはやり直さない）。
type Trade struct {
	Date   time.Time
	Code   string
	Rank   int
	Gap    float64
	Shares float64
	// Entry / Exit は建値と手仕舞い値（FillModel が決める。日足なら寄付と引け）。
	Entry  float64
	Exit   float64
	Amount float64
	// Fees は費用の合計。Commission はそのうち現物の定額手数料（台帳の実現損益には
	// 含まれないので、資産曲線の判定では足し戻す）。
	Fees       float64
	Commission float64
	Gross      float64
	PnL        float64
	Scale      float64
	// Carried は引けが制限値幅に張り付いて手仕舞えず、翌寄りに持ち越した
	// （ショートは引けストップ高、ロングは引けストップ安）。
	Carried bool
}

// Daily は 1 営業日の集計。
type Daily struct {
	Date   time.Time
	PnL    float64
	Gross  float64
	Fees   float64
	Amount float64
	N      int
	// Commission は Fees のうち現物の定額手数料。
	Commission float64
	// Scale は危険信号による資金の倍率（0 なら休んだ日）。
	Scale float64
	On    bool

	// レッグ別の内訳（simulate_margin のとき）。
	LongPnL, LongGross, LongFees, LongAmount     float64
	LongCommission                               float64
	LongN                                        int
	LongScale                                    float64
	ShortPnL, ShortGross, ShortFees, ShortAmount float64
	ShortN                                       int
	ShortMultiplier                              float64
}

// Summary は期間の要約。
type Summary struct {
	Days          int
	TradedDays    int
	Capital       decimal.Decimal
	TotalPnL      float64
	MeanDaily     float64
	AnnualReturn  float64
	Sharpe        float64
	MaxDrawdown   float64
	WinRate       float64
	MonthlyMean   float64
	MonthlyMedian float64
	MonthlyP10    float64
	MonthlyWin    float64
	AvgPositions  float64
	RoundTripBP   float64
}

// Result は simulate の結果。
type Result struct {
	Daily   []Daily
	Trades  []Trade
	Summary Summary
}

// MarginResult は simulateMargin の結果。ロング・ショートを合算した Daily に加えて
// レッグごとの内訳を持つ。
type MarginResult struct {
	Daily        []Daily
	LongTrades   []Trade
	ShortTrades  []Trade
	Summary      Summary
	LongSummary  Summary
	ShortSummary Summary
}

// Yearly は年別の集計 1 行。
type Yearly struct {
	Year      int
	Days      int
	Traded    int
	PnL       float64
	LongPnL   float64
	ShortPnL  float64
	MeanDaily float64
	WinRate   float64
}

// legParams は 1 レッグぶんの選定と価格付けの指定。
//
// 選定の規則そのもの（帯・順位・2 段階選定・業種上限・配分・株数）は持たない
// ——本番の設定（signal / margin）と selection.PickOptions をそのまま渡し、
// selection.Rank / RankShort / PickFrom に決めさせる。
type legParams struct {
	n      int
	budget decimal.Decimal
	// sign は損益の向き（買い +1: C − O、売り −1: O − C）。
	sign float64
	// extraCostBP は約定代金に対する往復の概算コスト（貸株料・金利・滑り、bp）。
	extraCostBP float64
	// commission が偽なら現物の定額手数料を掛けない（立花証券の信用取引は 0 円）。
	commission bool
	fill       FillModel
	// side は建てる向き。SELL なら RankShort と空売りの数量上限（PickFrom）。
	side domain.Side
	// signal / margin は順位付けの帯とストップの扱い（本番と同じ設定をそのまま渡す）。
	signal config.Signal
	margin config.Margin
	// pick は株数の決め方（N と Budget は日ごとに入れる）。
	pick selection.PickOptions
}

// pickAndPrice はランク付け・按分・価格付け。simulate のロング側の計算を一般化したもの。
func pickAndPrice(byDay map[string][]Row, days []time.Time, p legParams) []Trade {
	var trades []Trade
	for _, day := range days {
		trades = append(trades, pickDay(byDay[day.Format(dayLayout)], p, p.n, p.budget)...)
	}
	return trades
}

// pickDay は 1 日ぶんの選定と価格付け。n は銘柄数、budget は 1 注文の予算
// （既定は p.budget。ショートの余りをロングに回す日はそれより大きい——margin.spill_to_long）。
//
// 選定は本番と同じ関数（selection.Rank / RankShort → PickFrom）。ここでするのは
// パネルの行への変換と、建値・手仕舞い値・手数料の按分だけ。
func pickDay(rows []Row, p legParams, n int, budget decimal.Decimal) []Trade {
	if len(rows) == 0 || n < 1 {
		return nil
	}
	fill := p.fill
	if fill == nil {
		fill = OpenCloseFill{}
	}
	cands := make([]universe.Candidate, 0, len(rows))
	quotes := make(map[string]selection.Quote, len(rows))
	byCode := make(map[string]Row, len(rows))
	for _, r := range rows {
		cands = append(cands, candidateOf(r))
		quotes[r.Code] = quoteOf(r)
		byCode[r.Code] = r
	}
	var ranked []selection.Ranked
	if p.side == domain.SideSell {
		ranked = selection.RankShort(cands, quotes, p.margin)
	} else {
		ranked = selection.Rank(cands, quotes, p.signal)
	}
	opts := p.pick
	opts.N = n
	opts.Budget = budget
	picks := selection.PickFrom(ranked, opts)
	if len(picks) == 0 {
		return nil
	}

	// 建値・手仕舞い値は約定モデルが決める。決まらない銘柄は建てない
	shares := make([]float64, len(picks))
	entries := make([]float64, len(picks))
	exits := make([]float64, len(picks))
	for i, pick := range picks {
		entry, exit, ok := fill.Fill(byCode[pick.Code])
		if !ok || entry <= 0 || exit <= 0 {
			continue
		}
		shares[i] = pick.Quantity.InexactFloat64()
		entries[i], exits[i] = entry, exit
	}

	// 定額コースは 1 日の合計（買い＋売り）で段階が決まるので、
	// その日の手数料を約定代金の比で各取引に配る
	dayTotal := 0.0
	for i := range picks {
		dayTotal += shares[i] * (entries[i] + exits[i])
	}
	dayFee := 0.0
	if p.commission && dayTotal > 0 {
		f, _ := fees.Commission(decimal.NewFromFloat(dayTotal)).Float64()
		dayFee = f
	}

	var trades []Trade
	rank := 0
	for i, pick := range picks {
		if shares[i] <= 0 {
			continue
		}
		rank++ // 順位表の番号ではなく「建てた順」（--trades-csv の互換）
		amount := shares[i] * entries[i]
		extra := amount * p.extraCostBP / 1e4
		commission := 0.0
		if dayTotal > 0 {
			commission = dayFee * shares[i] * (entries[i] + exits[i]) / dayTotal
		}
		gross := shares[i] * p.sign * (exits[i] - entries[i])
		fee := extra + commission
		row := byCode[pick.Code]
		trades = append(trades, Trade{
			Date: row.Date, Code: pick.Code, Rank: rank, Gap: pick.Gap.InexactFloat64(),
			Shares: shares[i], Entry: entries[i], Exit: exits[i],
			Amount: amount, Fees: fee, Commission: commission,
			Gross: gross, PnL: gross - fee, Scale: 1,
		})
	}
	return trades
}

// applyCarry は引けが制限値幅に張り付いて手仕舞えなかった取引を「翌営業日の寄付で
// 手仕舞った」ことにする。
//
// ショート（sign −1）は引けストップ高——買い気配に張り付いて返済買いが約定しない。
// ロング（sign +1）は引けストップ安——売り気配に張り付いて売りが約定しない。
// 損益は penalty の割合だけ翌寄りに置き換える（1 で全額、0 で無視）——実際に
// 約定しない割合は日足からは分からない。
func applyCarry(trades []Trade, byKey map[string]Row, sign, penalty float64) []Trade {
	for i := range trades {
		row, ok := byKey[trades[i].Date.Format(dayLayout)+"|"+trades[i].Code]
		if !ok || row.NextOpen == nil {
			continue
		}
		// 浮動小数の丸めで制限値幅をわずかに外すことがあるので 1e-6 の余裕を持つ
		pinned := false
		if sign < 0 {
			pinned = row.Close >= row.LimitHigh-1e-6
		} else {
			pinned = row.Close <= row.LimitLow+1e-6
		}
		if !pinned {
			continue
		}
		trades[i].Carried = true
		trades[i].Gross += penalty * trades[i].Shares * sign * (*row.NextOpen - trades[i].Exit)
		trades[i].PnL = trades[i].Gross - trades[i].Fees
	}
	return trades
}

// dailyFromTrades は 1 レッグぶんの日次集計（取引の無い日も 0 で並べる）。
func dailyFromTrades(trades []Trade, days []time.Time) map[string]*Daily {
	out := make(map[string]*Daily, len(days))
	for _, day := range days {
		out[day.Format(dayLayout)] = &Daily{Date: day, Scale: 1, On: true}
	}
	for _, t := range trades {
		d, ok := out[t.Date.Format(dayLayout)]
		if !ok {
			continue
		}
		d.PnL += t.PnL
		d.Gross += t.Gross
		d.Fees += t.Fees
		d.Commission += t.Commission
		d.Amount += t.Amount
		d.N++
	}
	return out
}

// ledgerPnL は台帳の実現損益に当たる値（約定単価の差 × 数量）。現物の定額手数料は
// 台帳に載らないので足し戻す。資産曲線ゲートは実運用でこの定義を見る。
func ledgerPnL(pnl, commission float64) float64 { return pnl + commission }

// recentWindow は資産曲線ゲートの入力（前日までの直近 days 日の実現損益）。
//
// 実運用（cmd/daytrade の recentPnL）と同じ規則: 縮めた日はその倍率で、止めた日は 0 で
// 数える。窓に建てた日が 1 日も無ければ nil——12 月を休んだ直後の 1 月に「損益 0 ≤ 0」で
// 縮めてしまわないため（実運用は建てが無ければ判定しない）。
func recentWindow(pnl, scales []float64, traded []bool, i, days int) *float64 {
	if days <= 0 || i < days {
		return nil
	}
	total := 0.0
	any := false
	for j := i - days; j < i; j++ {
		total += pnl[j] * scales[j]
		any = any || traded[j]
	}
	if !any {
		return nil
	}
	return &total
}

// scaleTrades は日ごとの倍率を取引に掛け、倍率 0 の日の取引を落とす。
func scaleTrades(trades []Trade, scale map[string]float64) []Trade {
	out := trades[:0]
	for _, t := range trades {
		s := scale[t.Date.Format(dayLayout)]
		if s <= 0 {
			continue
		}
		t.Scale = s
		t.Shares *= s
		t.Amount *= s
		t.Fees *= s
		t.Commission *= s
		t.Gross *= s
		t.PnL *= s
		out = append(out, t)
	}
	return out
}

// marketGapByDay は候補全体のギャップの中央値（9:00 の市場ギャップの代用）。
func marketGapByDay(panel *Panel) map[string]*float64 {
	gaps := map[string][]float64{}
	for _, r := range panel.Rows {
		if r.Eligible {
			key := r.Date.Format(dayLayout)
			gaps[key] = append(gaps[key], r.Gap)
		}
	}
	out := make(map[string]*float64, len(gaps))
	for key, values := range gaps {
		out[key] = regime.MarketGapOf(values)
	}
	return out
}

// groupByDay は行を日付ごとに分ける。keep で脚ごとの母集団に絞る。
func groupByDay(panel *Panel, keep func(Row) bool) map[string][]Row {
	out := map[string][]Row{}
	for _, r := range panel.Rows {
		if !keep(r) {
			continue
		}
		key := r.Date.Format(dayLayout)
		out[key] = append(out[key], r)
	}
	return out
}

// Simulate はパネルに規則を当て、資金固定で日次損益を出す（寄付・引けで約定）。
//
// 危険信号は日次損益に後から掛ける: 止めた日は 0。実運用の open と同じ判定
// （regime.Evaluate）を日ごとに呼ぶ。
func Simulate(panel *Panel, cfg config.Config, signals *Inputs) (*Result, error) {
	return SimulateWith(panel, cfg, signals, Options{})
}

// SimulateWith は約定モデルを指定して検証する。
func SimulateWith(panel *Panel, cfg config.Config, signals *Inputs, opts Options) (*Result, error) {
	n := cfg.Capital.Positions()
	if n == 0 {
		return nil, fmt.Errorf("max_capital が 0 のため検証できません（買わない設定）")
	}
	budget := cfg.Capital.BudgetPerOrder()
	capital, _ := cfg.Capital.MaxCapital.Float64()
	carryPenalty, _ := cfg.Margin.CarryPenalty.Float64()

	longRows := groupByDay(panel, longKeep(cfg, opts))
	trades := pickAndPrice(longRows, panel.Days, legParams{
		n: n, budget: budget, sign: 1, commission: true, fill: opts.fill(),
		side: domain.SideBuy, signal: cfg.Signal, margin: cfg.Margin,
		pick: selection.PickOptions{
			Weighting:    cfg.Capital.Weighting,
			Side:         domain.SideBuy,
			MaxAmount:    cfg.Capital.MaxOrder,
			ValuePool:    cfg.Signal.ValuePool,
			MaxPerSector: cfg.Signal.MaxPerSector,
		},
	})
	trades = applyCarry(trades, rowsByKey(panel), 1, carryPenalty)

	daily := dailyFromTrades(trades, panel.Days)
	series, err := applyRegime(daily, panel, cfg, signals)
	if err != nil {
		return nil, err
	}
	// 縮めた日は取引もその倍率にし、止めた日の取引は落とす（実運用ではその日は建てていない）
	scale := map[string]float64{}
	for _, d := range series {
		scale[d.Date.Format(dayLayout)] = d.Scale
	}
	trades = scaleTrades(trades, scale)
	return &Result{Daily: series, Trades: trades, Summary: summarize(series, capital, legAll)}, nil
}

// longKeep はロングの母集団: eligible で、寄付の遅れで外れていない。
// ストップ安の除外（signal.skip_limit_down）と帯の判定は selection.Rank が見る。
func longKeep(cfg config.Config, opts Options) func(Row) bool {
	skipOpened := openedFilter(cfg, opts)
	outside := outsideGap(cfg.Signal.MinGap, cfg.Signal.MaxGap)
	return func(r Row) bool {
		return r.Eligible && !outside(r) && !skipOpened(r)
	}
}

// gapSlack は粗ふるいの余裕。パネルの gap（SQL）と selection の gap（decimal）は
// 同じ式なので差は倍精度の丸めだけ。帯の境界を挟んで判定が食い違わない幅を取る。
const gapSlack = 1e-9

// outsideGap は「帯 [min, max) に**どう転んでも入らない**」行を落とす粗ふるい。
//
// 帯の判定そのものは selection.Rank / RankShort が持つ（規則は 1 箇所）。ここで削るのは
// 10 年ぶんの候補を decimal に変換する手間で、境界の銘柄は余裕を付けて必ず残す
// ——これを入れないと 1 本 14 秒が 34 秒になる。
func outsideGap(min, max decimal.Decimal) func(Row) bool {
	lo, _ := min.Float64()
	hi, _ := max.Float64()
	return func(r Row) bool {
		return r.Gap < lo-gapSlack || r.Gap >= hi+gapSlack
	}
}

// openedFilter は「9:01 の時点で既に寄っている銘柄を外す」（signal.skip_opened）。
//
// 実運用は気配（時価問合の始値が入っているか）で判定し、検証は分足の最初の約定で
// 判定する。分足の無い日は判定しない——10 年の検証をこの設定で止めないため。
// 脚は問わない（実運用は気配そのものを落とすので、ロングにもショートにも効く）。
func openedFilter(cfg config.Config, opts Options) func(Row) bool {
	if !cfg.Signal.SkipOpened || opts.Opened == nil {
		return func(Row) bool { return false }
	}
	return func(r Row) bool {
		opened, known := opts.Opened(r.Date, r.Code)
		return known && opened
	}
}

// rowsByKey は (日付|銘柄) → 行。張り付きの判定で翌寄りを引くのに使う。
func rowsByKey(panel *Panel) map[string]Row {
	byKey := make(map[string]Row, len(panel.Rows))
	for _, r := range panel.Rows {
		byKey[r.Date.Format(dayLayout)+"|"+r.Code] = r
	}
	return byKey
}

// Inputs は危険信号の材料（バックテスト用に日付で引ける形）。
type Inputs struct {
	// IVPrev は日付 → 前日 IV。
	IVPrev map[string]*float64
	// Drift は日付 → TOPIX の日中ドリフト（前日までの平均）。
	Drift map[string]*float64
	// UsRet / Vix は日付 → 前夜の米国。
	UsRet map[string]*float64
	Vix   map[string]*float64
}

func (i *Inputs) lookup(m map[string]*float64, key string) *float64 {
	if i == nil || m == nil {
		return nil
	}
	return m[key]
}

// applyRegime は日ごとに regime.Evaluate を呼び、止めた日の損益を 0 にする。
func applyRegime(daily map[string]*Daily, panel *Panel, cfg config.Config, signals *Inputs) ([]Daily, error) {
	r := cfg.Regime
	if err := requireSignals(r, signals); err != nil {
		return nil, err
	}
	gaps := marketGapByDay(panel)
	out := make([]Daily, 0, len(panel.Days))
	pnl := make([]float64, 0, len(panel.Days))
	scales := make([]float64, 0, len(panel.Days))
	traded := make([]bool, 0, len(panel.Days))
	for i, day := range panel.Days {
		key := day.Format(dayLayout)
		d := daily[key]
		recent := recentWindow(pnl, scales, traded, i, r.EquityCurveDays)
		verdict := regime.Evaluate(r, regime.Signals{
			Day:       day,
			IVPrev:    signals.lookup(signalsIV(signals), key),
			Drift:     signals.lookup(signalsDrift(signals), key),
			MarketGap: gaps[key],
			RecentPnL: recent,
			UsRet:     signals.lookup(signalsUsRet(signals), key),
			Vix:       signals.lookup(signalsVix(signals), key),
		})
		scale := 0.0
		if verdict.Trade {
			scale = verdict.Scale * verdict.ShockLong
		}
		pnl = append(pnl, ledgerPnL(d.PnL, d.Commission))
		scales = append(scales, scale)
		traded = append(traded, scale > 0 && d.N > 0)

		scaled := *d
		scaled.Scale = scale
		scaled.On = scale > 0
		scaled.PnL *= scale
		scaled.Gross *= scale
		scaled.Fees *= scale
		scaled.Commission *= scale
		scaled.Amount *= scale
		if !scaled.On {
			scaled.N = 0
		}
		out = append(out, scaled)
	}
	return out, nil
}

func requireSignals(r config.Regime, signals *Inputs) error {
	if r.IVGate.GreaterThan(decimal.Zero) && (signals == nil || len(signals.IVPrev) == 0) {
		return fmt.Errorf("iv_gate を使うにはオプションのアーカイブが要ります")
	}
	if r.DriftGate != nil && (signals == nil || len(signals.Drift) == 0) {
		return fmt.Errorf("drift_gate を使うには TOPIX のアーカイブが要ります")
	}
	if r.UsSkipHigh != nil && (signals == nil || len(signals.UsRet) == 0) {
		return fmt.Errorf("us_skip_high を使うには米国市場のデータが要ります（FRED）")
	}
	return nil
}

func signalsIV(s *Inputs) map[string]*float64 {
	if s == nil {
		return nil
	}
	return s.IVPrev
}
func signalsDrift(s *Inputs) map[string]*float64 {
	if s == nil {
		return nil
	}
	return s.Drift
}
func signalsUsRet(s *Inputs) map[string]*float64 {
	if s == nil {
		return nil
	}
	return s.UsRet
}
func signalsVix(s *Inputs) map[string]*float64 {
	if s == nil {
		return nil
	}
	return s.Vix
}

// leg は要約を取り出す脚。
type leg int

const (
	legAll leg = iota
	legLong
	legShort
)

func (l leg) pick(d Daily) (pnl, amount, fee float64, n int) {
	switch l {
	case legLong:
		return d.LongPnL, d.LongAmount, d.LongFees, d.LongN
	case legShort:
		return d.ShortPnL, d.ShortAmount, d.ShortFees, d.ShortN
	default:
		return d.PnL, d.Amount, d.Fees, d.N
	}
}

func summarize(daily []Daily, capital float64, which leg) Summary {
	days := len(daily)
	if days == 0 {
		return Summary{}
	}
	pnl := make([]float64, days)
	equity := 0.0
	peak := 0.0
	mdd := 0.0
	tradedDays, wins := 0, 0
	amount, fee := 0.0, 0.0
	positions := 0
	type month struct {
		pnl    float64
		days   int
		onDays int
	}
	monthly := map[string]*month{}
	var months []string
	for i, d := range daily {
		p, a, f, n := which.pick(d)
		pnl[i] = p
		equity += p
		peak = math.Max(peak, equity)
		mdd = math.Min(mdd, equity-peak)
		if n > 0 {
			tradedDays++
			positions += n
			amount += a
			fee += f
			if p > 0 {
				wins++
			}
		}
		key := d.Date.Format("2006-01")
		m, ok := monthly[key]
		if !ok {
			m = &month{}
			monthly[key] = m
			months = append(months, key)
		}
		m.pnl += p
		m.days++
		if d.On {
			m.onDays++
		}
	}
	total, mean := sum(pnl), meanOf(pnl)
	sharpe := 0.0
	if std := stdev(pnl); std > 0 {
		sharpe = mean / std * math.Sqrt(TradingDays)
	}
	// 月次は「15 営業日以上ある月」だけ（期間の端の半端な月を混ぜない）。
	// 丸ごと休んだ月（skip_months）は損益 0 の負け月として数えない
	var monthlyPnL []float64
	monthlyWins := 0
	slices.Sort(months)
	for _, key := range months {
		if monthly[key].days < 15 || monthly[key].onDays == 0 {
			continue
		}
		monthlyPnL = append(monthlyPnL, monthly[key].pnl)
		if monthly[key].pnl > 0 {
			monthlyWins++
		}
	}
	summary := Summary{
		Days:         days,
		TradedDays:   tradedDays,
		Capital:      decimal.NewFromFloat(capital),
		TotalPnL:     total,
		MeanDaily:    mean,
		AnnualReturn: safeDiv(mean*TradingDays, capital),
		Sharpe:       sharpe,
		MaxDrawdown:  mdd,
		WinRate:      safeDiv(float64(wins), float64(tradedDays)),
		RoundTripBP:  safeDiv(fee, amount) * 1e4,
		AvgPositions: safeDiv(float64(positions), float64(tradedDays)),
	}
	if len(monthlyPnL) > 0 {
		summary.MonthlyMean = meanOf(monthlyPnL)
		summary.MonthlyMedian = quantile(monthlyPnL, 0.5)
		summary.MonthlyP10 = quantile(monthlyPnL, 0.1)
		summary.MonthlyWin = float64(monthlyWins) / float64(len(monthlyPnL))
	}
	return summary
}

// YearlyOf は年別の集計。
func YearlyOf(daily []Daily) []Yearly {
	byYear := map[int]*Yearly{}
	var years []int
	tradedWins := map[int]int{}
	for _, d := range daily {
		year := d.Date.Year()
		y, ok := byYear[year]
		if !ok {
			y = &Yearly{Year: year}
			byYear[year] = y
			years = append(years, year)
		}
		y.Days++
		y.PnL += d.PnL
		y.LongPnL += d.LongPnL
		y.ShortPnL += d.ShortPnL
		if d.N > 0 {
			y.Traded++
			if d.PnL > 0 {
				tradedWins[year]++
			}
		}
	}
	slices.Sort(years)
	out := make([]Yearly, 0, len(years))
	for _, year := range years {
		y := byYear[year]
		y.MeanDaily = safeDiv(y.PnL, float64(y.Days))
		y.WinRate = safeDiv(float64(tradedWins[year]), float64(y.Traded))
		out = append(out, *y)
	}
	return out
}

func sum(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

func meanOf(values []float64) float64 { return safeDiv(sum(values), float64(len(values))) }

func stdev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	mean := meanOf(values)
	acc := 0.0
	for _, v := range values {
		acc += (v - mean) * (v - mean)
	}
	return math.Sqrt(acc / float64(len(values)-1))
}

// quantile は線形補間（polars / numpy の既定と同じ）。
func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
