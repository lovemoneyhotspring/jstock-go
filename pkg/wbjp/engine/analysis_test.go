package engine

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func dec(v string) decimal.Decimal {
	return decimal.RequireFromString(v)
}

func fill(symbol string, side domain.Side, qty, price, fee string) domain.Fill {
	return domain.Fill{Symbol: symbol, Side: side, Quantity: dec(qty), Price: dec(price), Fee: dec(fee)}
}

func equityOf(values ...string) []decimal.Decimal {
	out := make([]decimal.Decimal, len(values))
	for i, v := range values {
		out[i] = dec(v)
	}
	return out
}

func TestClosedTradesFIFO(t *testing.T) {
	// 2 回に分けて買い、まとめて売る。古い建玉から順に消える
	fills := []domain.Fill{
		fill("7203", domain.SideBuy, "100", "1000", "0"),
		fill("7203", domain.SideBuy, "100", "1200", "0"),
		fill("7203", domain.SideSell, "150", "1300", "0"),
	}
	trades := ClosedTrades(fills)
	if len(trades) != 2 {
		t.Fatalf("往復は 2 件のはず: %d", len(trades))
	}
	if !trades[0].Quantity.Equal(dec("100")) || !trades[0].PnL().Equal(dec("30000")) {
		t.Errorf("1 件目が想定と違う: %+v", trades[0])
	}
	if !trades[1].Quantity.Equal(dec("50")) || !trades[1].PnL().Equal(dec("5000")) {
		t.Errorf("2 件目が想定と違う: %+v", trades[1])
	}
}

func TestClosedTradesProratesFee(t *testing.T) {
	// 手数料は数量比で按分される（部分決済でも 1 株あたりの負担が変わらない）
	fills := []domain.Fill{
		fill("7203", domain.SideBuy, "100", "1000", "500"),
		fill("7203", domain.SideSell, "50", "1000", "250"),
	}
	trades := ClosedTrades(fills)
	if len(trades) != 1 {
		t.Fatalf("往復は 1 件のはず: %d", len(trades))
	}
	// 原価 50*(1000+5)=50250、売却 50*(1000-5)=49750
	if !trades[0].PnL().Equal(dec("-500")) {
		t.Errorf("手数料の按分が合わない: %s", trades[0].PnL())
	}
}

func TestClosedTradesIgnoresNakedSell(t *testing.T) {
	// 保有を超える売り（空売り）は往復にしない
	fills := []domain.Fill{fill("7203", domain.SideSell, "100", "1000", "0")}
	if got := ClosedTrades(fills); len(got) != 0 {
		t.Errorf("空売りは対象外のはず: %+v", got)
	}
}

func TestLongestDrawdown(t *testing.T) {
	cases := []struct {
		name   string
		equity []decimal.Decimal
		want   int
	}{
		{"常に高値更新", equityOf("100", "110", "120"), 0},
		{"3 本連続で下回る", equityOf("100", "90", "95", "99", "101", "100"), 3},
		{"空", nil, 0},
	}
	for _, c := range cases {
		if got := LongestDrawdown(c.equity); got != c.want {
			t.Errorf("%s: %d != %d", c.name, got, c.want)
		}
	}
}

func TestSharpeRatio(t *testing.T) {
	if _, ok := SharpeRatio(equityOf("100", "110"), TradingDays); ok {
		t.Error("リターンが 1 本ならシャープは出せない")
	}
	if _, ok := SharpeRatio(equityOf("100", "110", "121"), TradingDays); ok {
		t.Error("リターンが一定なら標準偏差 0 で出せない")
	}
	value, ok := SharpeRatio(equityOf("100", "110", "105", "115"), TradingDays)
	if !ok || value <= 0 {
		t.Errorf("上昇基調なら正のシャープが出るはず: %v %v", value, ok)
	}
}

func TestTradeStats(t *testing.T) {
	fills := []domain.Fill{
		fill("7203", domain.SideBuy, "100", "1000", "0"),
		fill("7203", domain.SideSell, "100", "1100", "0"), // 勝ち
		fill("6758", domain.SideBuy, "100", "2000", "0"),
		fill("6758", domain.SideSell, "100", "1900", "0"), // 負け
	}
	winning, losing, rate := TradeStats(fills)
	if winning != 1 || losing != 1 || rate != 0.5 {
		t.Errorf("勝敗が合わない: %d/%d/%v", winning, losing, rate)
	}
}

func TestAnalyzeEmpty(t *testing.T) {
	out := Analyze(nil, nil)
	for _, key := range []string{"勝率", "平均損益/トレード", "シャープレシオ (年率)"} {
		if out[key] != "-" {
			t.Errorf("%s は値が無ければ \"-\": %q", key, out[key])
		}
	}
}
