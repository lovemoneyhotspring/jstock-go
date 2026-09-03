package engine

import (
	"fmt"
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
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

		// 4. 当日までの確定足スライスを準備
		todayBars := make(map[string][]domain.Bar)
		atrMap := make(map[string]decimal.Decimal)
		for sym, bars := range allBars {
			var slice []domain.Bar
			for _, b := range bars {
				if b.Date <= today {
					slice = append(slice, b)
				}
			}
			if len(slice) == 0 {
				continue
			}
			todayBars[sym] = slice

			if len(slice) >= 14 {
				highs := make([]float64, len(slice))
				lows := make([]float64, len(slice))
				closes := make([]float64, len(slice))
				for i, b := range slice {
					h, _ := b.High.Float64()
					l, _ := b.Low.Float64()
					c, _ := b.Close.Float64()
					highs[i] = h
					lows[i] = l
					closes[i] = c
				}
				atrVals, _ := indicators.ATR(highs, lows, closes, 14)
				if len(atrVals) > 0 {
					atrMap[sym] = decimal.NewFromFloat(atrVals[len(atrVals)-1])
				}
			}
		}

		// 5. ストップロスの管理と手仕舞い判定
		stopBook.Ensure(posMap, atrMap, today, setCfg.Sizing.ATRStopMultiple, true)
		stopBook.UpdateTrailing(closePrices, atrMap)

		targets := make(map[string]domain.TargetPosition)
		for _, exitTarget := range stopBook.ExitTargets(closePrices) {
			targets[exitTarget.Symbol] = exitTarget
		}

		// 6. 戦略のシグナル評価とサイジング
		if dayIdx < len(dates)-1 { // 最終日は新規建て不要
			for _, sym := range setCfg.Universe.Symbols {
				if _, hasExit := targets[sym]; hasExit {
					continue
				}
				bars := todayBars[sym]
				if len(bars) == 0 {
					continue
				}
				lastBar := bars[len(bars)-1]

				var sigs []domain.Signal
				for _, s := range strats {
					sig, err := s.OnBars(sym, bars)
					if err == nil && sig != nil {
						sigs = append(sigs, *sig)
					}
				}

				combined := combineFunc(sym, sigs, weights)
				if combined.Direction >= stratCfg.EntryThreshold {
					target, err := portfolio.SizePosition(combined, equity, lastBar.Close, atrMap[sym], lotSizes[sym], setCfg.Sizing)
					if err == nil {
						targets[sym] = target
					}
				}
			}
		}

		// 7. リコンサイル
		openOrders, _ := pb.GetOpenOrders()
		reconciled, err := Reconcile(targets, posMap, openOrders, closePrices, lotSizes, domain.OrderTypeMarket, decimal.Zero, today)
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

		for _, res := range reconciled {
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

	return &BacktestStats{
		InitialEquity: initialCash,
		FinalEquity:   finalEquity,
		TotalReturn:   totalReturn,
		MaxDrawdown:   maxDD,
		TotalFills:    len(allFills),
	}, nil
}
