package marketrules

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestPriceLimitTableMatchesWidthFunction(t *testing.T) {
	table := PriceLimitTable()
	if len(table) != len(priceLimits)+1 {
		t.Fatalf("表の長さ = %d", len(table))
	}
	// 表を上から見て最初に「未満」を満たす区分が、関数の答えと一致すること
	for _, price := range []int64{50, 100, 999, 1000, 3000000, 99999999} {
		base := decimal.NewFromInt(price)
		want, err := PriceLimitWidth(base)
		if err != nil {
			t.Fatal(err)
		}
		var got decimal.Decimal
		for _, entry := range table {
			if base.LessThan(entry[0]) {
				got = entry[1]
				break
			}
		}
		if !got.Equal(want) {
			t.Errorf("基準値段 %s: 表 = %s, 関数 = %s", base, got, want)
		}
	}
}

func TestRulesForOnlyJapan(t *testing.T) {
	rules, err := RulesFor(domain.MarketJP, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rules.Market() != domain.MarketJP || rules.Currency() != "JPY" {
		t.Errorf("rules = %v / %s", rules.Market(), rules.Currency())
	}
	if !rules.BlocksSameDaySale() {
		t.Error("東証の現物は差金決済を避けるため当日売却を止める")
	}
	if !rules.DefaultLotSize().Equal(decimal.NewFromInt(100)) {
		t.Errorf("単元 = %s", rules.DefaultLotSize())
	}
	if _, err := RulesFor(domain.MarketUS, nil); err == nil {
		t.Fatal("米国市場は売買に対応していない")
	}
}

func TestJpRulesUsesTopix500TickTable(t *testing.T) {
	rules := NewJpMarketRules([]string{"7203"})
	if !rules.IsTopix500("7203") || rules.IsTopix500("9999") {
		t.Fatal("TOPIX500 の判定がおかしい")
	}

	// TOPIX500 構成銘柄は 1000 円以下で 0.1 円刻み
	price := decimal.RequireFromString("999.45")
	got, err := rules.SnapToTick(price, domain.SideBuy, RoundingConservative, "7203")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(decimal.RequireFromString("999.4")) {
		t.Errorf("TOPIX500 の呼値 = %s, want 999.4", got)
	}

	// 構成銘柄でなければ 1 円刻み
	got, err = rules.SnapToTick(price, domain.SideBuy, RoundingConservative, "9999")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(decimal.NewFromInt(999)) {
		t.Errorf("通常銘柄の呼値 = %s, want 999", got)
	}
}

func TestJpRulesRoundToLotDefaults(t *testing.T) {
	rules := NewJpMarketRules(nil)
	got, err := rules.RoundToLot(decimal.NewFromInt(250), decimal.Zero)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(decimal.NewFromInt(200)) {
		t.Errorf("単元丸め = %s, want 200", got)
	}
}

func TestJpRulesPriceLimit(t *testing.T) {
	rules := NewJpMarketRules(nil)
	base := decimal.NewFromInt(1000)
	inside, err := rules.IsWithinPriceLimit(decimal.NewFromInt(1100), base)
	if err != nil || !inside {
		t.Fatalf("内側のはず: %v, %v", inside, err)
	}
	outside, err := rules.IsWithinPriceLimit(decimal.NewFromInt(2000), base)
	if err != nil || outside {
		t.Fatalf("外側のはず: %v, %v", outside, err)
	}
}
