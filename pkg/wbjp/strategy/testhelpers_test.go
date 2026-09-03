package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// weekdays は営業日っぽい日付列（土日を飛ばす）を n 本作る。
func weekdays(n int, start time.Time) []string {
	out := make([]string, 0, n)
	cur := start
	for len(out) < n {
		if cur.Weekday() != time.Saturday && cur.Weekday() != time.Sunday {
			out = append(out, cur.Format("2006-01-02"))
		}
		cur = cur.AddDate(0, 0, 1)
	}
	return out
}

// monthStartDates は「最終日が月初の営業日」になる日付列。
//
// momentum_rank の月次入れ替えを踏ませるために使う。
func monthStartDates(n int) []string {
	all := weekdays(n+60, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	for i := len(all) - 1; i > 0; i-- {
		if all[i][5:7] != all[i-1][5:7] {
			return all[i-n+1 : i+1]
		}
	}
	panic("月初が見つからない")
}

// mustDate は YYYY-MM-DD を time.Time にする。
func mustDate(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// growth は日率 daily で複利成長する終値列。noise は上下の揺れ（ボラを生む）。
func growth(daily float64, n int, start, noise float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start * math.Pow(1+daily, float64(i)) * (1 + noise*math.Sin(float64(i)/3))
	}
	return out
}

// barsFrom は終値列から日足を組み立てる。始値は前日終値、高安は終値の ±1%。
func barsFrom(symbol string, closes []float64, volume float64, dates []string) []domain.Bar {
	n := len(closes)
	if dates == nil {
		dates = weekdays(n, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	}
	bars := make([]domain.Bar, n)
	for i, c := range closes {
		open := c
		if i > 0 {
			open = closes[i-1]
		}
		high := math.Max(open, c) * 1.01
		low := math.Min(open, c) * 0.99
		bars[i] = domain.Bar{
			Symbol: symbol,
			Date:   dates[i],
			Open:   decimal.NewFromFloat(open),
			High:   decimal.NewFromFloat(high),
			Low:    decimal.NewFromFloat(low),
			Close:  decimal.NewFromFloat(c),
			Volume: decimal.NewFromFloat(volume),
		}
	}
	return bars
}

// barBuilder は OHLCV を1本ずつ足していく組み立て器。
type barBuilder struct {
	symbol string
	rows   [][5]float64
}

func newBars(symbol string) *barBuilder { return &barBuilder{symbol: symbol} }

func (b *barBuilder) add(open, high, low, close, volume float64) *barBuilder {
	b.rows = append(b.rows, [5]float64{open, high, low, close, volume})
	return b
}

// rise は「終値 ±1% を高安にした」上昇の足を n 本足す。
func (b *barBuilder) rise(n int, start, daily, volume float64) *barBuilder {
	price := start
	if len(b.rows) > 0 {
		price = b.lastClose()
	}
	for i := 0; i < n; i++ {
		open := price
		price = price * (1 + daily)
		b.add(open, math.Max(open, price)*1.01, math.Min(open, price)*0.99, price, volume)
	}
	return b
}

func (b *barBuilder) lastClose() float64 { return b.rows[len(b.rows)-1][3] }

func (b *barBuilder) build() []domain.Bar {
	dates := weekdays(len(b.rows), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	bars := make([]domain.Bar, len(b.rows))
	for i, r := range b.rows {
		bars[i] = domain.Bar{
			Symbol: b.symbol,
			Date:   dates[i],
			Open:   decimal.NewFromFloat(r[0]),
			High:   decimal.NewFromFloat(r[1]),
			Low:    decimal.NewFromFloat(r[2]),
			Close:  decimal.NewFromFloat(r[3]),
			Volume: decimal.NewFromFloat(r[4]),
		}
	}
	return bars
}

// heldPosition は数量が正の建玉。
func heldPosition(symbol string, quantity, cost float64) domain.Position {
	return domain.Position{
		Symbol:    symbol,
		Quantity:  decimal.NewFromFloat(quantity),
		CostPrice: decimal.NewFromFloat(cost),
		LastPrice: decimal.NewFromFloat(cost),
	}
}

// signalFor は結果から symbol のシグナルを取り出す。
func signalFor(t *testing.T, signals []domain.Signal, symbol string) (domain.Signal, bool) {
	t.Helper()
	for _, s := range signals {
		if s.Symbol == symbol {
			return s, true
		}
	}
	return domain.Signal{}, false
}

// mustSignal は symbol のシグナルがあることを要求する。
func mustSignal(t *testing.T, signals []domain.Signal, symbol string) domain.Signal {
	t.Helper()
	s, ok := signalFor(t, signals, symbol)
	if !ok {
		t.Fatalf("%s のシグナルがありません（%d 件: %v）", symbol, len(signals), symbolsOf(signals))
	}
	return s
}

func symbolsOf(signals []domain.Signal) []string {
	out := make([]string, len(signals))
	for i, s := range signals {
		out[i] = s.Symbol
	}
	return out
}

// hasReason は不合格理由に部分一致するものがあるか。
func hasReason(reasons []string, substr string) bool {
	for _, r := range reasons {
		if len(substr) > 0 && contains(r, substr) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
