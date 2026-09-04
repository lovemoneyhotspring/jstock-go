package engine

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
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

	pb := broker.NewPaperBroker(initialCash, opts.FillModel)

	// 待機資金の年利（%）を日付で引けるようにする。営業日が飛んでも
	// 直前の値を持ち越す（金利は毎日公表されるわけではない）。
	yields := make(map[string]decimal.Decimal, len(opts.CashYield))
	for _, b := range opts.CashYield {
		yields[b.Date] = b.Close.Div(decimal.NewFromInt(100))
	}

	// 全日付のユニオンを昇順で作成
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

	// 銘柄ごとの日付インデックス
	barByDate := make(map[string]map[string]domain.Bar)
	for sym, bars := range allBars {
		barByDate[sym] = make(map[string]domain.Bar)
		for _, b := range bars {
			barByDate[sym][b.Date] = b
		}
	}

	lotSizes := make(map[string]decimal.Decimal)
	for _, sym := range setCfg.Universe.Symbols {
		lotSizes[sym] = decimal.NewFromInt(100)
		if ov, ok := setCfg.Universe.LotSizeOverrides[sym]; ok && ov > 0 {
			lotSizes[sym] = decimal.NewFromInt(int64(ov))
		}
	}

	riskMgr := risk.NewRiskManager(setCfg.Risk, setCfg.Universe.Symbols)
	stopBook := risk.NewStopBook(nil)

	// サイジングはライブと同じ実装を使う。保有上限・手仕舞い閾値・
	// 再サイジング抑制が検証側だけ効かない状態を作らない。
	sizer, err := portfolio.NewSizer(setCfg.Sizing)
	if err != nil {
		return nil, err
	}

	// 指標は全履歴に対して一度だけ計算し、日ごとに切り詰めて見せる。
	// 日ごとに足を切り出して計算し直すと、日数の二乗に比例して遅くなる。
	universe := strategy.NewUniverse(allBars)

	var allFills []domain.Fill
	equityHistory := make([]decimal.Decimal, 0, len(dates))
	totalInterest := decimal.Zero
	lastYield := decimal.Zero
	previousDay := ""

	// 各日をシミュレーション
	for dayIdx, today := range dates {
		// 1. 今日の寄付・高値・安値・終値マップ
		openPrices := make(map[string]decimal.Decimal)
		closePrices := make(map[string]decimal.Decimal)
		highs := make(map[string]decimal.Decimal)
		lows := make(map[string]decimal.Decimal)
		traded := make(map[string]struct{})
		for sym := range allBars {
			if b, ok := barByDate[sym][today]; ok {
				openPrices[sym] = b.Open
				closePrices[sym] = b.Close
				highs[sym] = b.High
				lows[sym] = b.Low
				traded[sym] = struct{}{}
			}
		}

		// 2. 待機資金に利息を付ける。暦日の差で日割りするので、
		//    連休を挟んだ日はその日数ぶんまとめて付く。
		if len(yields) > 0 {
			if y, ok := yields[today]; ok {
				lastYield = y
			}
			if previousDay != "" {
				if days := calendarDaysBetween(previousDay, today); days > 0 {
					totalInterest = totalInterest.Add(pb.AccrueInterest(lastYield, days))
				}
			}
		}
		previousDay = today

		// 3. 前日出した注文を当日の寄付で約定。
		//    intrabar はその足の高安でも指値を約定させる（楽観的な第 2 の見立て）。
		pb.Mark(openPrices)
		pb.BeginDay()
		realizedBefore := pb.RealizedPnL()
		var fills []domain.Fill
		if opts.FillModel == "intrabar" {
			fills = pb.Settle(openPrices, highs, lows, nil)
		} else {
			fills = pb.Settle(openPrices, nil, nil, nil)
		}
		allFills = append(allFills, fills...)
		// 日付の軸は全銘柄の和集合なので、ベンチマークだけが立ち会った日（東証の休場日）が
		// 混ざりうる。その日に足の無い銘柄の注文は失効させず、次の立会いで約定させる
		pb.ExpireOpenOrdersFor(traded)
		realizedToday := pb.RealizedPnL().Sub(realizedBefore)

		// 4. 当日の終値でマーク
		pb.Mark(closePrices)
		bal, _ := pb.GetBalance()
		equity := bal.CashBalance.Add(bal.MarketValue)
		equityHistory = append(equityHistory, equity)
		posMap, _ := pb.PositionsBySymbol()

		// 5. 当日までの確定足を戦略に見せる眺めを作る。
		//
		// 未来の足は構造として見えない（Context が as_of で切り詰める）ので、
		// 先読みバイアスは規律ではなく仕組みで防がれる。
		stratCtx := universe.At(today, posMap, equity)

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

		// 6. ストップロスの管理と手仕舞い判定
		//
		// ライブ（cmd/wbjp/run.go）と同じ順序・同じ設定で処理する。
		// ここが食い違うと、検証結果が実運用を予測しなくなる。
		stopBook.EnsureWithOptions(posMap, atrMap, today,
			risk.EnsureOptionsFrom(setCfg.Stops, setCfg.Sizing.ATRStopMultiple))
		stopBook.UpdateTrailing(closePrices, atrMap)
		stopBook.UpdateBreakeven(closePrices, setCfg.Stops.BreakevenAfterR)

		// 7. 戦略のシグナル評価とサイジング
		//
		// ライブ（cmd/wbjp/run.go）と同じく、戦略には全銘柄をまとめて渡す。
		signalMap := make(map[string]domain.CombinedSignal)
		if dayIdx < len(dates)-1 { // 最終日は新規建て不要
			signalsBySymbol := make(map[string][]domain.Signal)
			for _, s := range strats {
				sigs, err := s.OnBars(stratCtx)
				if err != nil {
					continue
				}
				for _, sig := range sigs {
					signalsBySymbol[sig.Symbol] = append(signalsBySymbol[sig.Symbol], sig)
				}
			}
			for _, sym := range setCfg.Universe.Symbols {
				if !stratCtx.HasBars(sym, 1) {
					continue
				}
				signalMap[sym] = combineFunc(sym, signalsBySymbol[sym], weights)
			}
		}

		sizingEquity := equity
		if setCfg.Regime.Enabled {
			regimeName, exposure := risk.RegimeExposure(setCfg.Regime, benchmarkInput(stratCtx, setCfg.Regime))
			signalMap, sizingEquity = risk.ApplyRegime(regimeName, exposure, signalMap, posMap, equity)
		}

		strategyTargets := sizer.Size(signalMap, portfolio.SizingContext{
			Equity:      sizingEquity,
			BuyingPower: bal.BuyingPower,
			Prices:      closePrices,
			ATR:         atrMap,
			LotSizes:    lotSizes,
			Positions:   posMap,
		}, stratCfg.EntryThreshold, stratCfg.ExitThreshold)

		quantities := make(map[string]decimal.Decimal, len(posMap))
		for sym, pos := range posMap {
			quantities[sym] = pos.Quantity
		}
		stopTargets := stopBook.ExitTargets(closePrices)
		stopTargets = append(stopTargets,
			stopBook.TimeExitTargets(closePrices, today, setCfg.Stops.StaleExitDays, setCfg.Stops.MaxHoldDays)...)
		stopTargets = append(stopTargets,
			stopBook.TakeProfitTargets(closePrices, quantities, lotSizes,
				setCfg.Stops.TakeProfitR, setCfg.Stops.TakeProfitFraction, marketrules.DefaultLotSize)...)

		targets := make(map[string]domain.TargetPosition)
		for _, t := range risk.ApplyStopPriority(strategyTargets, stopTargets) {
			targets[t.Symbol] = t
		}

		// 8. リコンサイル
		//
		// ここで出す注文は翌営業日の寄付で約定する。当日の寄付で買った銘柄を翌日に売るのは
		// 差金決済にならないので、当日買付の銘柄は渡さない（渡すと手仕舞いが常に 1 日遅れる）。
		// 差金決済の判定そのもの（BlocksSameDaySale）はライブと同じく有効のまま。
		openOrders, _ := pb.GetOpenOrders()
		plan, err := Reconcile(targets, posMap, openOrders, closePrices, lotSizes,
			ReconcileSettings{
				OrderType:         domain.OrderTypeMarket,
				LimitOffset:       decimal.Zero,
				TaxType:           domain.TaxAccountSpecific,
				BlocksSameDaySale: true,
			},
			nil, today)
		if err != nil {
			continue
		}

		// 9. リスク検査と発注。当日の実現損益は寄付の約定から出す（日次の損失上限を効かせる）
		riskCtx := risk.RiskContext{
			Equity:           equity,
			Balance:          *bal,
			Positions:        posMap,
			BasePrices:       closePrices,
			PendingValue:     make(map[string]decimal.Decimal),
			OrdersToday:      0,
			RealizedPnLToday: realizedToday,
		}

		for _, res := range plan.Orders {
			if res.Request == nil {
				continue
			}
			req := *res.Request
			decision := riskMgr.Check(req, riskCtx, nil)
			if decision.Approved {
				_, _ = pb.Place(req)
				riskCtx.OrdersToday++
			}
		}
	}

	finalBal, _ := pb.GetBalance()
	finalEquity := finalBal.CashBalance.Add(finalBal.MarketValue)
	totalReturn := finalEquity.Sub(initialCash).Div(initialCash)

	// 最大ドローダウン計算
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

	// 勝率は約定を FIFO で往復に突き合わせないと出せない（analysis.go）
	winning, losing, winRate := TradeStats(allFills)

	sells := 0
	for _, f := range allFills {
		if f.Side == domain.SideSell {
			sells++
		}
	}

	return &BacktestStats{
		InitialEquity: initialCash,
		FinalEquity:   finalEquity,
		TotalReturn:   totalReturn,
		MaxDrawdown:   maxDD,
		TotalFills:    len(allFills),
		SellFills:     sells,
		Days:          len(dates),
		WinningTrades: winning,
		LosingTrades:  losing,
		WinRate:       winRate,
		Interest:      totalInterest,
		Analysis:      Analyze(equityHistory, allFills),
	}, nil
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
