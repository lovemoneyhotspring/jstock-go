package engine

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/portfolio"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
)

type BacktestStats struct {
	InitialEquity decimal.Decimal
	FinalEquity   decimal.Decimal
	TotalReturn   decimal.Decimal
	MaxDrawdown   decimal.Decimal
	TotalFills    int
	SellFills     int
	Days          int
	WinningTrades int
	LosingTrades  int
	WinRate       float64
	Interest      decimal.Decimal
	// Analysis は表示用の追加指標（勝率・シャープレシオ・最長ドローダウン等）。
	Analysis map[string]string
}

// BacktestOptions は検証の条件。ゼロ値は「全期間・寄付約定・無利息」。
type BacktestOptions struct {
	// Start / End は売買の対象期間（YYYY-MM-DD、両端を含む）。
	// この前の足は捨てずにウォームアップ（指標の確定）に使う。
	Start string
	End   string
	// FillModel は指値の約定判定。"open"（寄付だけ。保守的）か
	// "intrabar"（高安も見る。楽観的）。
	FillModel string
	// CashYield は待機資金の年利（%）の日足。^IRX など。
	// 与えると現金に日割り（360 日）で利息を付ける。
	CashYield []domain.Bar
	// Margin は信用残（週次）。nil なら信用残を使う戦略は黙る。
	Margin *strategy.MarginBook
	// TradingDay は営業日の判定（東証のカレンダー。calendar.Calendar.IsTradingDay）。
	// 時間切れ（stale_exit_days・max_hold_days）の営業日数を本番と同じく数える。
	// nil なら土日だけを除く（祝日も数える。米国株やカレンダーが無いとき）。
	TradingDay func(time.Time) bool
}

// FillModels は選べる約定モデル。
var FillModels = []string{"open", "intrabar"}

// ValidFillModel は fill が選べる約定モデルかどうか。空文字は既定（open）として通す。
func ValidFillModel(fill string) bool {
	if fill == "" {
		return true
	}
	for _, m := range FillModels {
		if m == fill {
			return true
		}
	}
	return false
}

// RunBacktest は指定期間の過去日足データを用いてスイング売買のシミュレーションを行う。
func RunBacktest(
	setCfg *wbjpcfg.SettingsFile,
	stratCfg *wbjpcfg.StrategiesConfig,
	strats []strategy.Strategy,
	weights map[string]float64,
	combineFunc strategy.Combiner,
	allBars map[string][]domain.Bar,
	initialCash decimal.Decimal,
	opts BacktestOptions,
) (*BacktestStats, error) {
	if initialCash.LessThanOrEqual(decimal.Zero) {
		initialCash = decimal.NewFromInt(1000000)
	}
	if !ValidFillModel(opts.FillModel) {
		return nil, fmt.Errorf("約定モデルは open / intrabar のいずれか: %q", opts.FillModel)
	}

	dates, err := backtestDates(allBars, opts)
	if err != nil {
		return nil, err
	}

	sim, err := newBacktestSim(setCfg, stratCfg, strats, weights, combineFunc, allBars, initialCash, opts)
	if err != nil {
		return nil, err
	}
	sim.equityHistory = make([]decimal.Decimal, 0, len(dates))

	// 各日をシミュレーション
	for dayIdx, today := range dates {
		sim.runDay(today, dayIdx == len(dates)-1)
	}

	return sim.stats(initialCash, len(dates)), nil
}

// backtestDates は売買の対象日（全銘柄の日付の和集合を期間で絞り、昇順）。
func backtestDates(allBars map[string][]domain.Bar, opts BacktestOptions) ([]string, error) {
	dateSet := make(map[string]struct{})
	for _, bars := range allBars {
		for _, b := range bars {
			dateSet[b.Date] = struct{}{}
		}
	}
	var dates []string
	for d := range dateSet {
		// Start より前の足は捨てない。ウォームアップとして戦略に見せた上で、
		// 売買の対象からだけ外す（indicators が確定しないまま建てさせない）。
		if opts.Start != "" && d < opts.Start {
			continue
		}
		if opts.End != "" && d > opts.End {
			continue
		}
		dates = append(dates, d)
	}
	sort.Strings(dates)

	if len(dates) == 0 {
		if opts.Start != "" || opts.End != "" {
			return nil, fmt.Errorf("対象期間（%s〜%s）に取引日がありません",
				orDash(opts.Start), orDash(opts.End))
		}
		return nil, fmt.Errorf("バックテスト用の足データがありません")
	}
	return dates, nil
}

// backtestSim はバックテストの 1 回分の状態（模型のブローカー・ストップ・日々の資産）。
type backtestSim struct {
	setCfg      *wbjpcfg.SettingsFile
	stratCfg    *wbjpcfg.StrategiesConfig
	strats      []strategy.Strategy
	weights     map[string]float64
	combineFunc strategy.Combiner
	allBars     map[string][]domain.Bar
	opts        BacktestOptions

	pb        *broker.PaperBroker
	barByDate map[string]map[string]domain.Bar // 銘柄ごとの日付インデックス
	lotSizes  map[string]decimal.Decimal
	riskMgr   *risk.RiskManager
	stopBook  *risk.StopBook
	sizer     *portfolio.Sizer
	universe  *strategy.Universe
	// yields は待機資金の年利（小数）を日付で引く。営業日が飛んでも直前の値を持ち越す
	yields map[string]decimal.Decimal

	allFills      []domain.Fill
	equityHistory []decimal.Decimal
	totalInterest decimal.Decimal
	lastYield     decimal.Decimal
	previousDay   string
}

func newBacktestSim(
	setCfg *wbjpcfg.SettingsFile,
	stratCfg *wbjpcfg.StrategiesConfig,
	strats []strategy.Strategy,
	weights map[string]float64,
	combineFunc strategy.Combiner,
	allBars map[string][]domain.Bar,
	initialCash decimal.Decimal,
	opts BacktestOptions,
) (*backtestSim, error) {
	s := &backtestSim{
		setCfg: setCfg, stratCfg: stratCfg, strats: strats, weights: weights,
		combineFunc: combineFunc, allBars: allBars, opts: opts,
		pb:            broker.NewPaperBroker(initialCash, opts.FillModel),
		totalInterest: decimal.Zero,
		lastYield:     decimal.Zero,
	}

	// 待機資金の年利（%）を日付で引けるようにする。営業日が飛んでも
	// 直前の値を持ち越す（金利は毎日公表されるわけではない）。
	s.yields = make(map[string]decimal.Decimal, len(opts.CashYield))
	for _, b := range opts.CashYield {
		s.yields[b.Date] = b.Close.Div(decimal.NewFromInt(100))
	}

	s.barByDate = make(map[string]map[string]domain.Bar)
	for sym, bars := range allBars {
		s.barByDate[sym] = make(map[string]domain.Bar)
		for _, b := range bars {
			s.barByDate[sym][b.Date] = b
		}
	}

	s.lotSizes = make(map[string]decimal.Decimal)
	for _, sym := range setCfg.Universe.Symbols {
		s.lotSizes[sym] = decimal.NewFromInt(100)
		if ov, ok := setCfg.Universe.LotSizeOverrides[sym]; ok && ov > 0 {
			s.lotSizes[sym] = decimal.NewFromInt(int64(ov))
		}
	}

	s.riskMgr = risk.NewRiskManager(setCfg.Risk, setCfg.Universe.Symbols)
	s.stopBook = risk.NewStopBook(nil)

	// サイジングはライブと同じ実装を使う。保有上限・手仕舞い閾値・
	// 再サイジング抑制が検証側だけ効かない状態を作らない。
	sizer, err := portfolio.NewSizer(setCfg.Sizing)
	if err != nil {
		return nil, err
	}
	s.sizer = sizer

	// 指標は全履歴に対して一度だけ計算し、日ごとに切り詰めて見せる。
	// 日ごとに足を切り出して計算し直すと、日数の二乗に比例して遅くなる。
	s.universe = strategy.NewUniverse(allBars).SetMargin(opts.Margin)
	return s, nil
}

// dayPrices は今日の足（寄付・高値・安値・終値）と、今日立ち会った銘柄。
type dayPrices struct {
	open, close, high, low map[string]decimal.Decimal
	traded                 map[string]struct{}
}

func (s *backtestSim) pricesOn(today string) dayPrices {
	p := dayPrices{
		open:   make(map[string]decimal.Decimal),
		close:  make(map[string]decimal.Decimal),
		high:   make(map[string]decimal.Decimal),
		low:    make(map[string]decimal.Decimal),
		traded: make(map[string]struct{}),
	}
	for sym := range s.allBars {
		if b, ok := s.barByDate[sym][today]; ok {
			p.open[sym] = b.Open
			p.close[sym] = b.Close
			p.high[sym] = b.High
			p.low[sym] = b.Low
			p.traded[sym] = struct{}{}
		}
	}
	return p
}

// accrueInterest は待機資金に利息を付ける。暦日の差で日割りするので、
// 連休を挟んだ日はその日数ぶんまとめて付く。
func (s *backtestSim) accrueInterest(today string) {
	if len(s.yields) > 0 {
		if y, ok := s.yields[today]; ok {
			s.lastYield = y
		}
		if s.previousDay != "" {
			if days := calendarDaysBetween(s.previousDay, today); days > 0 {
				s.totalInterest = s.totalInterest.Add(s.pb.AccrueInterest(s.lastYield, days))
			}
		}
	}
	s.previousDay = today
}

// settleOpen は前日出した注文を当日の寄付で約定させ、当日の実現損益を返す。
// intrabar はその足の高安でも指値を約定させる（楽観的な第 2 の見立て）。
func (s *backtestSim) settleOpen(p dayPrices) (realizedToday decimal.Decimal) {
	s.pb.Mark(p.open)
	s.pb.BeginDay()
	realizedBefore := s.pb.RealizedPnL()
	var fills []domain.Fill
	if s.opts.FillModel == "intrabar" {
		fills = s.pb.Settle(p.open, p.high, p.low, nil)
	} else {
		fills = s.pb.Settle(p.open, nil, nil, nil)
	}
	s.allFills = append(s.allFills, fills...)
	// 日付の軸は全銘柄の和集合なので、ベンチマークだけが立ち会った日（東証の休場日）が
	// 混ざりうる。その日に足の無い銘柄の注文は失効させず、次の立会いで約定させる
	s.pb.ExpireOpenOrdersFor(p.traded)
	return s.pb.RealizedPnL().Sub(realizedBefore)
}

// runDay は 1 日分を進める（寄付の約定 → 終値のマーク → ストップ → シグナル → 発注）。
// last は最終日（新規建てのシグナルを出さない）。
func (s *backtestSim) runDay(today string, last bool) {
	// 1. 今日の寄付・高値・安値・終値マップ
	p := s.pricesOn(today)

	// 2. 待機資金に利息を付ける。
	s.accrueInterest(today)

	// 3. 前日出した注文を当日の寄付で約定。
	realizedToday := s.settleOpen(p)

	// 4. 当日の終値でマーク
	s.pb.Mark(p.close)
	bal, _ := s.pb.GetBalance()
	equity := bal.CashBalance.Add(bal.MarketValue)
	s.equityHistory = append(s.equityHistory, equity)
	posMap, _ := s.pb.PositionsBySymbol()

	// 5. 当日までの確定足を戦略に見せる眺めを作る。
	//
	// 未来の足は構造として見えない（Context が as_of で切り詰める）ので、
	// 先読みバイアスは規律ではなく仕組みで防がれる。
	stratCtx := s.universe.At(today, posMap, equity)
	atrMap := atrBySymbol(stratCtx)

	// 6. ストップロスの管理と手仕舞い判定
	//
	// ライブ（cmd/wbjp/run.go）と同じ順序・同じ設定で処理する（発注の審査も売りを先に: placeReviewed）。
	// ここが食い違うと、検証結果が実運用を予測しなくなる。
	// 手仕舞った銘柄のストップを外すのは RetainHeld だけ（模型の建玉は常に確か）
	s.stopBook.RetainHeld(posMap)
	s.stopBook.EnsureWithOptions(posMap, atrMap, today,
		risk.EnsureOptionsFrom(s.setCfg.Stops, s.setCfg.Sizing.ATRStopMultiple))
	s.stopBook.UpdateTrailing(p.close, atrMap)
	s.stopBook.UpdateBreakeven(p.close, s.setCfg.Stops.BreakevenAfterR)

	// 7. 戦略のシグナル評価とサイジング
	targets := s.targets(today, last, stratCtx, bal, equity, posMap, p.close, atrMap)

	// 8. リコンサイル
	//
	// ここで出す注文は翌営業日の寄付で約定する。当日の寄付で買った銘柄を翌日に売るのは
	// 差金決済にならないので、当日買付の銘柄は渡さない（渡すと手仕舞いが常に 1 日遅れる）。
	// 差金決済の判定そのもの（BlocksSameDaySale）はライブと同じく有効のまま。
	openOrders, _ := s.pb.GetOpenOrders()
	plan, err := Reconcile(targets, posMap, openOrders, p.close, s.lotSizes,
		ReconcileSettings{
			OrderType:         domain.OrderTypeMarket,
			LimitOffset:       decimal.Zero,
			TaxType:           domain.TaxAccountSpecific,
			BlocksSameDaySale: true,
		},
		nil, today)
	if err != nil {
		return
	}

	// 9. リスク検査と発注。当日の実現損益は寄付の約定から出す（日次の損失上限を効かせる）
	riskCtx := risk.RiskContext{
		Equity:           equity,
		Balance:          *bal,
		Positions:        posMap,
		BasePrices:       p.close,
		PendingValue:     make(map[string]decimal.Decimal),
		OrdersToday:      0,
		RealizedPnLToday: realizedToday,
	}

	placeReviewed(plan.Orders, s.riskMgr, &riskCtx, func(req domain.OrderRequest) { _, _ = s.pb.Place(req) })
}

// atrBySymbol は 14 本以上の足がある銘柄の ATR(14)。
func atrBySymbol(stratCtx *strategy.Context) map[string]decimal.Decimal {
	atrMap := make(map[string]decimal.Decimal)
	for _, sym := range stratCtx.Symbols() {
		v, ok := stratCtx.Bars(sym)
		if !ok || v.Len() < 14 {
			continue
		}
		if value := lastFiniteOf(v.ATR(14)); !math.IsNaN(value) {
			atrMap[sym] = decimal.NewFromFloat(value)
		}
	}
	return atrMap
}

// signals は戦略のシグナルを銘柄ごとにまとめる。
//
// ライブ（cmd/wbjp/run.go）と同じく、戦略には全銘柄をまとめて渡す。
func (s *backtestSim) signals(stratCtx *strategy.Context) map[string]domain.CombinedSignal {
	signalMap := make(map[string]domain.CombinedSignal)
	signalsBySymbol := make(map[string][]domain.Signal)
	for _, st := range s.strats {
		sigs, err := st.OnBars(stratCtx)
		if err != nil {
			continue
		}
		for _, sig := range sigs {
			signalsBySymbol[sig.Symbol] = append(signalsBySymbol[sig.Symbol], sig)
		}
	}
	for _, sym := range s.setCfg.Universe.Symbols {
		if !stratCtx.HasBars(sym, 1) {
			continue
		}
		signalMap[sym] = s.combineFunc(sym, signalsBySymbol[sym], s.weights)
	}
	return signalMap
}

// targets は戦略の目標（地合いとサイジングを通したもの）とストップ由来の目標を合わせる。
func (s *backtestSim) targets(
	today string,
	last bool,
	stratCtx *strategy.Context,
	bal *domain.Balance,
	equity decimal.Decimal,
	posMap map[string]domain.Position,
	closePrices, atrMap map[string]decimal.Decimal,
) map[string]domain.TargetPosition {
	signalMap := make(map[string]domain.CombinedSignal)
	if !last { // 最終日は新規建て不要
		signalMap = s.signals(stratCtx)
	}

	sizingEquity := equity
	if s.setCfg.Regime.Enabled {
		regimeName, exposure := risk.RegimeExposure(s.setCfg.Regime, benchmarkInput(stratCtx, s.setCfg.Regime))
		signalMap, sizingEquity = risk.ApplyRegime(regimeName, exposure, signalMap, posMap, equity)
	}

	strategyTargets := s.sizer.Size(signalMap, portfolio.SizingContext{
		Equity:      sizingEquity,
		BuyingPower: bal.BuyingPower,
		Prices:      closePrices,
		ATR:         atrMap,
		LotSizes:    s.lotSizes,
		Positions:   posMap,
	}, s.stratCfg.EntryThreshold, s.stratCfg.ExitThreshold)

	quantities := make(map[string]decimal.Decimal, len(posMap))
	for sym, pos := range posMap {
		quantities[sym] = pos.Quantity
	}
	// ストップ由来の目標は本番と同じ risk.ExitPlan を通す（損切り・時間切れ・利確・残り玉）。
	// 出した売りは翌寄りで必ず約定するので、利確は決めた時点で確定する（AssumeFilled）。
	// 本番は保有が減ったのを見てから確定する（約定しなかった利確を出し直すため）
	stopTargets := s.stopBook.ExitPlan(s.setCfg.Stops, risk.ExitInputs{
		Closes: closePrices, Quantities: quantities, LotSizes: s.lotSizes, AsOf: today,
		AssumeFilled: true, TradingDay: s.opts.TradingDay,
		Bars: func(sym string) []domain.Bar {
			v, ok := stratCtx.Bars(sym)
			if !ok {
				return nil
			}
			return v.Bars()
		},
	})

	targets := make(map[string]domain.TargetPosition)
	for _, t := range risk.ApplyStopPriority(strategyTargets, stopTargets) {
		targets[t.Symbol] = t
	}
	return targets
}

// stats は最終日までの結果をまとめる。
func (s *backtestSim) stats(initialCash decimal.Decimal, days int) *BacktestStats {
	finalBal, _ := s.pb.GetBalance()
	finalEquity := finalBal.CashBalance.Add(finalBal.MarketValue)
	totalReturn := finalEquity.Sub(initialCash).Div(initialCash)

	// 勝率は約定を FIFO で往復に突き合わせないと出せない（analysis.go）
	winning, losing, winRate := TradeStats(s.allFills)

	sells := 0
	for _, f := range s.allFills {
		if f.Side == domain.SideSell {
			sells++
		}
	}

	return &BacktestStats{
		InitialEquity: initialCash,
		FinalEquity:   finalEquity,
		TotalReturn:   totalReturn,
		MaxDrawdown:   maxDrawdown(initialCash, s.equityHistory),
		TotalFills:    len(s.allFills),
		SellFills:     sells,
		Days:          days,
		WinningTrades: winning,
		LosingTrades:  losing,
		WinRate:       winRate,
		Interest:      s.totalInterest,
		Analysis:      Analyze(s.equityHistory, s.allFills),
	}
}

// maxDrawdown は資産の推移の最大ドローダウン（初期資産を最初の山とする）。
func maxDrawdown(initialCash decimal.Decimal, equityHistory []decimal.Decimal) decimal.Decimal {
	maxPeak := initialCash
	maxDD := decimal.Zero
	for _, eq := range equityHistory {
		if eq.GreaterThan(maxPeak) {
			maxPeak = eq
		}
		if maxPeak.GreaterThan(decimal.Zero) {
			dd := maxPeak.Sub(eq).Div(maxPeak)
			if dd.GreaterThan(maxDD) {
				maxDD = dd
			}
		}
	}
	return maxDD
}

// orDash は空文字を "—" にする（期間の片側だけ指定されたときの表示用）。
func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// calendarDaysBetween は 2 つの ISO 日付（YYYY-MM-DD）の暦日の差。
// 解釈できなければ 0（利息を付けない）。
func calendarDaysBetween(from, to string) int {
	a, err := time.Parse("2006-01-02", from)
	if err != nil {
		return 0
	}
	b, err := time.Parse("2006-01-02", to)
	if err != nil {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}

// benchmarkInput はバックテスト時点での地合い判定材料を組み立てる。
//
// ライブの regimeInput と同じ規則。指標が揃わなければ nil のままにして、
// 判断できない日を弱気として扱わせる。
func benchmarkInput(ctx *strategy.Context, cfg wbjpcfg.RegimeConfig) risk.RegimeInput {
	var in risk.RegimeInput
	if cfg.Benchmark == "" {
		return in
	}
	v, ok := ctx.Bars(cfg.Benchmark)
	if !ok || v.Len() == 0 {
		return in
	}

	bars := v.Bars()
	last := bars[len(bars)-1].Close
	in.Close = &last

	longMA := v.SMA(cfg.SMALong)
	midMA := v.SMA(cfg.SMAMid)
	if value := lastFiniteOf(longMA); !math.IsNaN(value) {
		d := decimal.NewFromFloat(value)
		in.LongMA = &d
	}
	if value := lastFiniteOf(midMA); !math.IsNaN(value) {
		d := decimal.NewFromFloat(value)
		in.MidMA = &d
	}
	idx := len(longMA) - 1 - cfg.SlopeLookback
	if in.LongMA != nil && idx >= 0 && !math.IsNaN(longMA[idx]) {
		slope := in.LongMA.Sub(decimal.NewFromFloat(longMA[idx]))
		in.Slope = &slope
	}
	return in
}

// lastFiniteOf は並びの末尾にある有効な値。NaN しか無ければ NaN。
func lastFiniteOf(series []float64) float64 {
	for i := len(series) - 1; i >= 0; i-- {
		if !math.IsNaN(series[i]) {
			return series[i]
		}
	}
	return math.NaN()
}

// placeReviewed は Reconcile の注文をリスク審査に通し、通ったものを place に渡す。
//
// ライブ（cmd/wbjp/run.go）と同じく売りを先に審査する（risk.SellsFirst）。Reconcile の順
// （銘柄コード順）のままだと、max_orders_per_day を買いで使い切った日に損切りの売りが
// 見送られ、ライブより損切りが遅れる検証になる。
func placeReviewed(orders []ReconcileResult, riskMgr *risk.RiskManager, riskCtx *risk.RiskContext,
	place func(domain.OrderRequest)) {
	var requests []domain.OrderRequest
	for _, res := range orders {
		if res.Request != nil {
			requests = append(requests, *res.Request)
		}
	}
	for _, req := range risk.SellsFirst(requests) {
		if riskMgr.Check(req, *riskCtx, nil).Approved {
			place(req)
			riskCtx.OrdersToday++
		}
	}
}
