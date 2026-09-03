package engine

import (
	"fmt"
	"math"
	"sort"

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
	WinningTrades int
	LosingTrades  int
	WinRate       float64
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
) (*BacktestStats, error) {
	if initialCash.LessThanOrEqual(decimal.Zero) {
		initialCash = decimal.NewFromInt(1000000)
	}

	pb := broker.NewPaperBroker(initialCash, "open")

	// 全日付のユニオンを昇順で作成
	dateSet := make(map[string]struct{})
	for _, bars := range allBars {
		for _, b := range bars {
			dateSet[b.Date] = struct{}{}
		}
	}
	var dates []string
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	if len(dates) == 0 {
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

	// 各日をシミュレーション
	for dayIdx, today := range dates {
		// 1. 今日の寄付・高値・安値・終値マップ
		openPrices := make(map[string]decimal.Decimal)
		closePrices := make(map[string]decimal.Decimal)
		for sym := range allBars {
			if b, ok := barByDate[sym][today]; ok {
				openPrices[sym] = b.Open
				closePrices[sym] = b.Close
			}
		}

		// 2. 前日出した注文を当日の寄付で約定
		pb.Mark(openPrices)
		pb.BeginDay()
		fills := pb.Settle(openPrices, nil, nil, nil)
		allFills = append(allFills, fills...)
		pb.ExpireOpenOrders()

		// 3. 当日の終値でマーク
		pb.Mark(closePrices)
		bal, _ := pb.GetBalance()
		equity := bal.CashBalance.Add(bal.MarketValue)
		equityHistory = append(equityHistory, equity)
		posMap, _ := pb.PositionsBySymbol()

		// 4. 当日までの確定足を戦略に見せる眺めを作る。
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

		// 5. ストップロスの管理と手仕舞い判定
		//
		// ライブ（cmd/wbjp/run.go）と同じ順序・同じ設定で処理する。
		// ここが食い違うと、検証結果が実運用を予測しなくなる。
		stopBook.EnsureWithOptions(posMap, atrMap, today,
			risk.EnsureOptionsFrom(setCfg.Stops, setCfg.Sizing.ATRStopMultiple))
		stopBook.UpdateTrailing(closePrices, atrMap)
		stopBook.UpdateBreakeven(closePrices, setCfg.Stops.BreakevenAfterR)

		// 6. 戦略のシグナル評価とサイジング
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

		// 7. リコンサイル
		openOrders, _ := pb.GetOpenOrders()
		plan, err := Reconcile(targets, posMap, openOrders, closePrices, lotSizes,
			ReconcileSettings{
				OrderType:         domain.OrderTypeMarket,
				LimitOffset:       decimal.Zero,
				TaxType:           domain.TaxAccountSpecific,
				BlocksSameDaySale: true,
			},
			pb.BoughtToday(), today)
		if err != nil {
			continue
		}

		// 8. リスク検査と発注
		riskCtx := risk.RiskContext{
			Equity:           equity,
			Balance:          *bal,
			Positions:        posMap,
			BasePrices:       closePrices,
			PendingValue:     make(map[string]decimal.Decimal),
			OrdersToday:      0,
			RealizedPnLToday: decimal.Zero,
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

	return &BacktestStats{
		InitialEquity: initialCash,
		FinalEquity:   finalEquity,
		TotalReturn:   totalReturn,
		MaxDrawdown:   maxDD,
		TotalFills:    len(allFills),
		WinningTrades: winning,
		LosingTrades:  losing,
		WinRate:       winRate,
	}, nil
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
