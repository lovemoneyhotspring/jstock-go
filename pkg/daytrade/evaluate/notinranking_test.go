package evaluate_test

import (
	"slices"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/evaluate"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
)

// 台帳の注文の雛形。どれも 100 株の注文で、filled が約定した株数。
func longEntry(symbol, status string, filled int64) dtledger.Order {
	return dtledger.Order{Symbol: symbol, Side: "BUY", Trade: "MARGIN_OPEN", Status: status,
		Quantity: dec(100), FilledQuantity: dec(filled)}
}

func longExit(symbol string) dtledger.Order {
	return dtledger.Order{Symbol: symbol, Side: "SELL", Trade: "MARGIN_CLOSE", Status: "FILLED",
		Quantity: dec(100), FilledQuantity: dec(100)}
}

func shortEntry(symbol, status string, filled int64) dtledger.Order {
	return dtledger.Order{Symbol: symbol, Side: "SELL", Trade: "MARGIN_OPEN", Status: status,
		Quantity: dec(100), FilledQuantity: dec(filled)}
}

func shortExit(symbol string) dtledger.Order {
	return dtledger.Order{Symbol: symbol, Side: "BUY", Trade: "MARGIN_CLOSE", Status: "FILLED",
		Quantity: dec(100), FilledQuantity: dec(100)}
}

func cashBuy(symbol string) dtledger.Order {
	return dtledger.Order{Symbol: symbol, Side: "BUY", Trade: "CASH", Status: "FILLED",
		Quantity: dec(100), FilledQuantity: dec(100)}
}

func cashSell(symbol string) dtledger.Order {
	return dtledger.Order{Symbol: symbol, Side: "SELL", Trade: "CASH", Status: "FILLED",
		Quantity: dec(100), FilledQuantity: dec(100)}
}

func verified(o dtledger.Order) dtledger.Order {
	o.Verify = true
	return o
}

// OrdersNotInRanking は「台帳の約定が順位表に無い」ことを知らせる最後の砦。
// 壊れると評価から約定が黙って落ちるので、数える注文・数えない注文を 1 つずつ固定する。
func TestOrdersNotInRanking(t *testing.T) {
	buyRow := func(symbol string) evaluate.RankingRow { return evaluate.RankingRow{Side: "BUY", Symbol: symbol} }
	sellRow := func(symbol string) evaluate.RankingRow { return evaluate.RankingRow{Side: "SELL", Symbol: symbol} }

	cases := []struct {
		name   string
		rows   []evaluate.RankingRow
		orders []dtledger.Order
		want   []string
	}{
		{
			name:   "約定がすべて順位表にある",
			rows:   []evaluate.RankingRow{buyRow("1000"), buyRow("2000")},
			orders: []dtledger.Order{longEntry("1000", "FILLED", 100), longEntry("2000", "FILLED", 100)},
			want:   nil,
		},
		{
			name:   "順位表に無い約定だけが残る",
			rows:   []evaluate.RankingRow{buyRow("1000")},
			orders: []dtledger.Order{longEntry("1000", "FILLED", 100), longEntry("9999", "FILLED", 100)},
			want:   []string{"9999|long"},
		},
		{
			name:   "順位表が空なら約定はすべて残る（台帳の順）",
			rows:   nil,
			orders: []dtledger.Order{longEntry("2000", "FILLED", 100), shortEntry("1000", "FILLED", 100)},
			want:   []string{"2000|long", "1000|short"},
		},
		{
			name:   "台帳が空",
			rows:   []evaluate.RankingRow{buyRow("1000")},
			orders: nil,
			want:   nil,
		},
		{
			name:   "順位表も台帳も空",
			rows:   nil,
			orders: nil,
			want:   nil,
		},
		{
			name: "ロング・ショート混在: 脚ごとに突き合わせる",
			rows: []evaluate.RankingRow{buyRow("1000"), sellRow("2000")},
			orders: []dtledger.Order{
				longEntry("1000", "FILLED", 100), shortEntry("2000", "FILLED", 100),
				longEntry("3000", "FILLED", 100), shortEntry("4000", "FILLED", 100),
			},
			want: []string{"3000|long", "4000|short"},
		},
		{
			name: "同じ銘柄でも脚が違えば別物",
			rows: []evaluate.RankingRow{buyRow("1000"), sellRow("2000")},
			orders: []dtledger.Order{
				shortEntry("1000", "FILLED", 100), // 順位表は BUY だけ
				longEntry("2000", "FILLED", 100),  // 順位表は SELL だけ
			},
			want: []string{"1000|short", "2000|long"},
		},
		{
			name: "同じ銘柄が両方の脚の順位表にあれば、どちらの約定も一致する",
			rows: []evaluate.RankingRow{buyRow("1000"), sellRow("1000")},
			orders: []dtledger.Order{
				longEntry("1000", "FILLED", 100), shortEntry("1000", "FILLED", 100),
			},
			want: nil,
		},
		{
			name: "再試行で同じ銘柄を 2 回建てても 1 回だけ出る",
			rows: nil,
			orders: []dtledger.Order{
				longEntry("9999", "PARTIALLY_FILLED", 40), longEntry("9999", "FILLED", 60),
			},
			want: []string{"9999|long"},
		},
		{
			name: "手仕舞いは数えない（建てた側だけを見る）",
			rows: nil,
			orders: []dtledger.Order{
				longExit("1000"), shortExit("2000"), cashSell("3000"),
			},
			want: nil,
		},
		{
			name: "約定していない注文は数えない",
			rows: nil,
			orders: []dtledger.Order{
				longEntry("1000", "SUBMITTED", 0),
				longEntry("2000", "CANCELLED", 0),
				longEntry("3000", "REJECTED", 0),
				shortEntry("4000", "EXPIRED", 0),
				shortEntry("5000", "UNSENT", 0),
			},
			want: nil,
		},
		{
			name: "一部約定の後に取り消された注文は建玉が残るので数える",
			rows: nil,
			orders: []dtledger.Order{
				shortEntry("1000", "CANCELLED", 30),
				longEntry("2000", "PARTIALLY_FILLED", 10),
			},
			want: []string{"1000|short", "2000|long"},
		},
		{
			name: "dry-run と実機検証（--verify）の注文は数えない",
			rows: nil,
			orders: []dtledger.Order{
				longEntry("1000", dtledger.DryRunStatus, 100),
				verified(longEntry("2000", "FILLED", 100)),
			},
			want: nil,
		},
		{
			name:   "現物の買いもロングの建てとして突き合わせる",
			rows:   []evaluate.RankingRow{buyRow("1000")},
			orders: []dtledger.Order{cashBuy("1000"), cashBuy("9999")},
			want:   []string{"9999|long"},
		},
		{
			name: "選ばれなかった行（次点）でも順位表にあれば一致とみなす",
			rows: []evaluate.RankingRow{
				{Side: "BUY", Symbol: "1000", Picked: true},
				{Side: "BUY", Symbol: "2000", Picked: false},
			},
			orders: []dtledger.Order{longEntry("2000", "FILLED", 100)},
			want:   nil,
		},
		{
			name:   "Side が空の行はロングとして読む（rankingLeg の既定）",
			rows:   []evaluate.RankingRow{{Symbol: "1000"}},
			orders: []dtledger.Order{longEntry("1000", "FILLED", 100), shortEntry("1000", "FILLED", 100)},
			want:   []string{"1000|short"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := evaluate.OrdersNotInRanking(c.rows, c.orders)
			if !slices.Equal(got, c.want) {
				t.Errorf("OrdersNotInRanking = %v, want %v", got, c.want)
			}
		})
	}
}
