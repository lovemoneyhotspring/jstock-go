package engine

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// バックテスト結果の分析値（ドローダウン期間・トレード統計・シャープレシオ）。
//
// BacktestResult の資産曲線と約定列だけから出す。外部ライブラリには頼らない。

// TradingDays は年率換算に使う 1 年の営業日数（東証）。
const TradingDays = 245

// ClosedTrade は買いと売りを突き合わせて閉じた 1 往復（FIFO）。
type ClosedTrade struct {
	Symbol   string
	Quantity decimal.Decimal
	// Cost は買いの約定代金＋手数料（数量ぶん）。
	Cost decimal.Decimal
	// Proceeds は売りの約定代金−手数料（数量ぶん）。
	Proceeds decimal.Decimal
}

// PnL は 1 往復の損益。
func (t ClosedTrade) PnL() decimal.Decimal { return t.Proceeds.Sub(t.Cost) }

// openLot は未決済の買い持ち（数量と、手数料を株数で按分した実効単価）。
type openLot struct {
	quantity decimal.Decimal
	// unitCost は「単価 + 手数料/株」。あとで数量を掛けるだけで原価が出る。
	unitCost decimal.Decimal
}

// ClosedTrades は約定列を銘柄ごとに FIFO で突き合わせ、閉じた往復にする。
//
// 売りが保有を超えた分（空売り）は対象外。手数料は数量比で按分する——
// 部分約定で建玉が分割されても、1 株あたりの負担が変わらないようにするため。
func ClosedTrades(fills []domain.Fill) []ClosedTrade {
	openLots := make(map[string][]openLot)
	trades := []ClosedTrade{}

	for _, fill := range fills {
		if fill.Quantity.LessThanOrEqual(decimal.Zero) {
			continue
		}
		perShareFee := fill.Fee.Div(fill.Quantity)

		if fill.Side == domain.SideBuy {
			openLots[fill.Symbol] = append(openLots[fill.Symbol],
				openLot{quantity: fill.Quantity, unitCost: fill.Price.Add(perShareFee)})
			continue
		}

		remaining := fill.Quantity
		lots := openLots[fill.Symbol]
		for remaining.GreaterThan(decimal.Zero) && len(lots) > 0 {
			lot := lots[0]
			take := lot.quantity
			if remaining.LessThan(take) {
				take = remaining
			}
			trades = append(trades, ClosedTrade{
				Symbol:   fill.Symbol,
				Quantity: take,
				Cost:     take.Mul(lot.unitCost),
				Proceeds: take.Mul(fill.Price.Sub(perShareFee)),
			})
			remaining = remaining.Sub(take)
			if take.Equal(lot.quantity) {
				lots = lots[1:]
			} else {
				lots[0].quantity = lot.quantity.Sub(take)
			}
		}
		openLots[fill.Symbol] = lots
	}
	return trades
}

// LongestDrawdown は直近の最高値を下回り続けた最長の本数（0 なら常に高値更新）。
func LongestDrawdown(equity []decimal.Decimal) int {
	var peak decimal.Decimal
	started := false
	longest, current := 0, 0
	for _, value := range equity {
		if !started || value.GreaterThanOrEqual(peak) {
			peak = value
			started = true
			current = 0
			continue
		}
		current++
		if current > longest {
			longest = current
		}
	}
	return longest
}

// SharpeRatio は資産曲線の 1 本ごとのリターンから年率シャープレシオ（無リスク金利 0）。
//
// リターンが 2 本未満、または標準偏差が 0 なら ok=false。標本標準偏差（n−1）を使う——
// 資産曲線は母集団ではなく標本なので、n で割ると分散を過小評価する。
func SharpeRatio(equity []decimal.Decimal, periodsPerYear int) (float64, bool) {
	if periodsPerYear <= 0 {
		periodsPerYear = TradingDays
	}
	returns := []float64{}
	for i := 1; i < len(equity); i++ {
		prev := equity[i-1]
		if prev.LessThanOrEqual(decimal.Zero) {
			continue
		}
		returns = append(returns, equity[i].Div(prev).InexactFloat64()-1)
	}
	if len(returns) < 2 {
		return 0, false
	}
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))

	variance := 0.0
	for _, r := range returns {
		variance += (r - mean) * (r - mean)
	}
	variance /= float64(len(returns) - 1)
	if variance <= 0 {
		return 0, false
	}
	return mean / math.Sqrt(variance) * math.Sqrt(float64(periodsPerYear)), true
}

// TradeStats は決済済みトレードの勝ち負けと勝率。
//
// BacktestStats の WinningTrades / LosingTrades / WinRate を埋めるための入口。
// 損益ちょうど 0 の往復はどちらにも数えない（勝率の分母には入る）。
func TradeStats(fills []domain.Fill) (winning, losing int, winRate float64) {
	trades := ClosedTrades(fills)
	for _, t := range trades {
		switch {
		case t.PnL().GreaterThan(decimal.Zero):
			winning++
		case t.PnL().LessThan(decimal.Zero):
			losing++
		}
	}
	if len(trades) > 0 {
		winRate = float64(winning) / float64(len(trades))
	}
	return winning, losing, winRate
}

// Analyze は表示用の平らな辞書。値の無いものは "-"。
func Analyze(equity []decimal.Decimal, fills []domain.Fill) map[string]string {
	trades := ClosedTrades(fills)
	won := 0
	total := decimal.Zero
	for _, t := range trades {
		if t.PnL().GreaterThan(decimal.Zero) {
			won++
		}
		total = total.Add(t.PnL())
	}

	winRate, average := "-", "-"
	if len(trades) > 0 {
		n := decimal.NewFromInt(int64(len(trades)))
		winRate = fmt.Sprintf("%.1f%%", float64(won)/float64(len(trades))*100)
		average = total.Div(n).StringFixed(2)
	}
	sharpe := "-"
	if value, ok := SharpeRatio(equity, TradingDays); ok {
		sharpe = fmt.Sprintf("%.2f", value)
	}

	return map[string]string{
		"最長ドローダウン期間 (本)": fmt.Sprintf("%d", LongestDrawdown(equity)),
		"決済トレード数":        fmt.Sprintf("%d", len(trades)),
		"勝率":             winRate,
		"平均損益/トレード":      average,
		"シャープレシオ (年率)":   sharpe,
	}
}
