package broker

import (
	"errors"
	"fmt"
	"strings"
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

// 1 周目（ずらして送る。その場の送り直しはしない）で落ちたバッチだけを 2 周目の直列で取り直し、全件揃う。
func TestMarketPricesRetriesOnlyFailedBatches(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)
	symbols := priceTestSymbols()
	second := symbols[120]
	fake.priceFail = map[string]int{second: 2}

	before := clock.NowUTC()
	rows, received, failed := b.MarketPricesRawPartialAt(symbols, "")
	if len(failed) != 0 {
		t.Fatalf("取り直しで揃うはず: %v", failed)
	}
	if len(rows) != 130 {
		t.Fatalf("行数 %d, want 130", len(rows))
	}
	// 受信時刻は行と同じ数だけあり、バッチの中では同じ値。取り直した 2 本目は 1 本目より後
	if len(received) != len(rows) {
		t.Fatalf("受信時刻 %d 個、行 %d", len(received), len(rows))
	}
	if received[0].Before(before) || !received[0].Equal(received[119]) || received[120].Before(received[0]) {
		t.Errorf("受信時刻がおかしい: 先頭 %v / 120 行目 %v / 121 行目 %v", received[0], received[119], received[120])
	}
	for _, r := range rows {
		for k := range r {
			if strings.HasPrefix(k, "_") {
				t.Errorf("応答の行に余計な鍵 %q が入った", k)
			}
		}
	}
	if n := countBatches(fake.priceBatches, symbols[0]); n != 1 {
		t.Errorf("成功したバッチを送り直している: %d 回", n)
	}
	if n := countBatches(fake.priceBatches, second); n != 3 {
		t.Errorf("落ちたバッチは 1 周目 1 回（失敗）＋ 2 周目 2 回（失敗 + postTo の送り直しで成功）= 3 回のはず: %d", n)
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
	// 1 周目はずらして送る経路で、その場の送り直しはしない（2 周目の直列に任せる）
	if n := countBatches(fake.priceBatches, second); n != 3 {
		t.Errorf("1 周目 1 回 + 2 周目 (失敗 + 送り直し) = 3 回のはず: %d", n)
	}

	// TACHIBANA_PRICE_STAGGER_MS=0 なら従来の直列——1 周目も postTo がその場で送り直す
	t.Setenv("TACHIBANA_PRICE_STAGGER_MS", "0")
	fake.priceFail = map[string]int{second: 4}
	fake.priceBatches = nil
	if _, failed := b.MarketPricesRawPartial(symbols, ""); len(failed) != 1 {
		t.Fatalf("直列でも 2 本目の失敗が報告されるはず: %+v", failed)
	}
	if n := countBatches(fake.priceBatches, second); n != 4 {
		t.Errorf("直列は 2 周 × (失敗 + 送り直し) = 4 回のはず: %d", n)
	}
	t.Setenv("TACHIBANA_PRICE_STAGGER_MS", "")

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

// ずらして送っても、届く順に p_no が増えている（実機は増えていないと p_errno=6 で弾く）。
func TestMarketPricesPipelinedKeepsPNoOrder(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	fake.enforcePNo = true
	b := newSessionTestBroker(t, fake, keyPath, dir)
	symbols := make([]string, 0, 600)
	for i := 0; i < 600; i++ {
		symbols = append(symbols, fmt.Sprintf("%04d", 1000+i))
	}

	rows, at, failed := b.MarketPricesRawPartialAt(symbols, "")
	if len(failed) != 0 || len(rows) != 600 || len(at) != 600 {
		t.Fatalf("5 バッチとも取れるはず: rows %d, at %d, failed %+v", len(rows), len(at), failed)
	}
	if len(fake.priceBatches) != 5 {
		t.Errorf("取り直しなしの 5 回のはず: %v", fake.priceBatches)
	}
	for i := 1; i < len(fake.pNos); i++ {
		if fake.pNos[i] <= fake.pNos[i-1] {
			t.Errorf("p_no が届いた順に増えていない: %v", fake.pNos)
		}
	}
	// 後続の電文（発注・照会）の番号は、ずらして送ったぶんより大きい
	if _, err := b.postRequest(clmOrderList, map[string]any{}); err != nil {
		t.Fatalf("時価問合のあとの照会が弾かれた: %v", err)
	}
}

// 到着が入れ替わって p_errno=6 で弾かれたバッチは、2 周目の直列で取り直す。セッションは捨てない。
func TestMarketPricesPipelinedRetriesPNoOrder(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	fake.enforcePNo = true
	b := newSessionTestBroker(t, fake, keyPath, dir)
	symbols := priceTestSymbols()
	second := symbols[120]
	fake.priceRejectPNo = map[string]int{second: 1}

	rows, err := b.MarketPricesRaw(symbols, "")
	if err != nil || len(rows) != 130 {
		t.Fatalf("弾かれた 2 本目も取り直して 130 行のはず: rows %d, err %v", len(rows), err)
	}
	if n := countBatches(fake.priceBatches, second); n != 2 {
		t.Errorf("2 本目は 1 周目（弾かれる）+ 2 周目の 2 回のはず: %d", n)
	}
	if fake.logins != 1 {
		t.Errorf("p_errno=6 でセッションを捨ててログインし直している: logins %d", fake.logins)
	}
}

// 1 周目のバッチが失効（p_errno=2）で返ったら、セッションを捨てる。2 周目の直列がログインし直して取り直す。
func TestMarketPricesPipelinedReloginsOnSessionLost(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	b := newSessionTestBroker(t, fake, keyPath, dir)
	symbols := priceTestSymbols()
	second := symbols[120]
	fake.priceSessionLost = map[string]int{second: 1}

	rows, err := b.MarketPricesRaw(symbols, "")
	if err != nil || len(rows) != 130 {
		t.Fatalf("失効した 2 本目もログインし直して 130 行のはず: rows %d, err %v", len(rows), err)
	}
	if fake.logins != 2 {
		t.Errorf("失効ならセッションを捨ててログインし直すはず: logins %d", fake.logins)
	}
}

// TACHIBANA_PRICE_STAGGER_MS を読めないときは既定の間隔のまま、値を返して警告させる（直列に戻したつもり、を見逃さない）。
func TestMarketPriceStaggerEnv(t *testing.T) {
	for _, c := range []struct {
		raw     string
		want    time.Duration
		invalid bool
	}{{"", 50 * time.Millisecond, false}, {"0", 0, false}, {"100", 100 * time.Millisecond, false}, {"O", 50 * time.Millisecond, true}, {"0ms", 50 * time.Millisecond, true}, {"2000", 50 * time.Millisecond, true}} {
		t.Setenv("TACHIBANA_PRICE_STAGGER_MS", c.raw)
		got, invalid := marketPriceStagger()
		if got != c.want || (invalid != "") != c.invalid {
			t.Errorf("%q: got %v invalid %q", c.raw, got, invalid)
		}
	}
}
