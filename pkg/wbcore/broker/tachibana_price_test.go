package broker

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/shopspring/decimal"
)

func TestFlatRateCommission(t *testing.T) {
	d := decimal.RequireFromString
	cases := []struct{ dayTotal, want string }{
		{"0", "0"}, {"-100", "0"},
		{"120000", "0"}, {"120001", "176"},
		{"200000", "176"}, {"200001", "253"},
		{"500000", "253"}, {"1000000", "506"},
		{"5000000", "1518"}, {"10000000", "2783"},
		// 最上段を超えたら 100 万ごとに 253 円ずつ（端数も 1 段）
		{"10500000", "3036"}, {"12000000", "3289"},
	}
	for _, c := range cases {
		if got := FlatRateCommission(d(c.dayTotal)); !got.Equal(d(c.want)) {
			t.Errorf("FlatRateCommission(%s) = %s, want %s", c.dayTotal, got, c.want)
		}
	}
}

func TestMarginalFlatRateCommission(t *testing.T) {
	d := decimal.RequireFromString
	// すでに 10 万円約定していて、さらに 5 万円 → 段が 0 円から 176 円に上がる
	if got := MarginalFlatRateCommission(d("100000"), d("50000")); !got.Equal(d("176")) {
		t.Errorf("差分 = %s, want 176", got)
	}
	// 同じ段の中で増えるだけなら差分 0
	if got := MarginalFlatRateCommission(d("300000"), d("50000")); !got.IsZero() {
		t.Errorf("同じ段での差分 = %s, want 0", got)
	}
}

func TestPriceDecimal(t *testing.T) {
	// 立花証券は「値無し」を * で返す
	for _, v := range []any{nil, "", "*", "abc"} {
		if got := priceDecimal(v); !got.IsZero() {
			t.Errorf("priceDecimal(%v) = %s, want 0", v, got)
		}
	}
	if got := priceDecimal("2500.5"); !got.Equal(decimal.RequireFromString("2500.5")) {
		t.Errorf("priceDecimal = %s", got)
	}
}

func TestPriceTime(t *testing.T) {
	got := priceTime("09:00:30")
	local := clock.ToZone(got, clock.Tokyo)
	if local.Hour() != 9 || local.Minute() != 0 || local.Second() != 30 {
		t.Errorf("JST に組み立てられていない: %v", local)
	}
	if got.Location() != time.UTC {
		t.Errorf("UTC で返していない: %v", got.Location())
	}
	// 読めない形式は現在時刻（判断を止めないため）
	if priceTime("--").IsZero() {
		t.Error("読めない時刻でゼロ値を返している")
	}
}

// 130 銘柄 → 2 バッチ（120 + 10）。2 本目の先頭は wanted[120]。
func priceTestSymbols() []string {
	out := make([]string, 0, 130)
	for i := 0; i < 130; i++ {
		out = append(out, fmt.Sprintf("%04d", 1000+i))
	}
	return out
}

func countBatches(batches []string, first string) int {
	n := 0
	for _, b := range batches {
		if b == first {
			n++
		}
	}
	return n
}

// 1 周目で落ちたバッチ（postTo の送り直し込みで 2 回失敗）だけを 2 周目で取り直し、全件揃う。
func TestMarketPricesRetriesOnlyFailedBatches(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)
	symbols := priceTestSymbols()
	second := symbols[120]
	fake.priceFail = map[string]int{second: 2}

	rows, failed := b.MarketPricesRawPartial(symbols, "")
	if len(failed) != 0 {
		t.Fatalf("取り直しで揃うはず: %v", failed)
	}
	if len(rows) != 130 {
		t.Fatalf("行数 %d, want 130", len(rows))
	}
	if n := countBatches(fake.priceBatches, symbols[0]); n != 1 {
		t.Errorf("成功したバッチを送り直している: %d 回", n)
	}
	if n := countBatches(fake.priceBatches, second); n != 3 {
		t.Errorf("落ちたバッチは 失敗 2（うち 1 は postTo の送り直し）＋ 取り直し 1 = 3 回のはず: %d", n)
	}
}

// 取り直しても駄目なら、取れた分だけ返して失敗を報告する。MarketPricesRaw は失敗。
func TestMarketPricesKeepsPartialRows(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)
	symbols := priceTestSymbols()
	second := symbols[120]
	fake.priceFail = map[string]int{second: 4}

	rows, failed := b.MarketPricesRawPartial(symbols, "")
	if len(rows) != 120 {
		t.Fatalf("取れた 1 本目の 120 行は返すはず: %d", len(rows))
	}
	if len(failed) != 1 || failed[0].Index != 2 || failed[0].Batches != 2 || len(failed[0].Symbols) != 10 {
		t.Fatalf("2 本目の失敗が報告されるはず: %+v", failed)
	}
	if n := countBatches(fake.priceBatches, second); n != 4 {
		t.Errorf("2 周 × (失敗 + 送り直し) = 4 回のはず: %d", n)
	}

	fake.priceFail = map[string]int{second: 4}
	if _, err := b.MarketPricesRaw(symbols, ""); err == nil {
		t.Fatal("全件揃わないのに MarketPricesRaw が成功した（発注に欠けた気配を渡す）")
	}
}

// 締め切りを過ぎたら、残りのバッチも取り直しも送らない。
func TestMarketPricesStopsAtDeadline(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)
	if _, err := b.postRequest(clmOrderList, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	sent := len(fake.clmIDs)

	b.SetDeadline(time.Now().Add(-time.Second))
	rows, failed := b.MarketPricesRawPartial(priceTestSymbols(), "")
	if len(rows) != 0 || len(failed) != 2 {
		t.Fatalf("締め切り後は 2 バッチとも未送信で失敗のはず: rows %d, failed %d", len(rows), len(failed))
	}
	var deadline *ErrDeadline
	if !errors.As(failed[1].Err, &deadline) {
		t.Errorf("残りのバッチの理由は ErrDeadline のはず: %v", failed[1].Err)
	}
	if len(fake.clmIDs) != sent {
		t.Error("締め切り後に時価問合が送られた")
	}
}
