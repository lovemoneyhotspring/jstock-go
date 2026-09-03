package broker

import (
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
