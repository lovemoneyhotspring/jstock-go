package main

import (
	"reflect"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
)

// 対象（ロング・ショート）が先、残りが後。それぞれの中は plan の並びのまま。
func TestOrderSnapSymbols(t *testing.T) {
	cands := []universe.Candidate{
		{Symbol: "1301"},
		{Symbol: "1332", Eligible: true},
		{Symbol: ""},
		{Symbol: "1605", ShortEligible: true},
		{Symbol: "1721"},
		{Symbol: "1801", Eligible: true, ShortEligible: true},
	}
	if got, want := orderSnapSymbols(cands, false), []string{"1332", "1605", "1801", "1301", "1721"}; !reflect.DeepEqual(got, want) {
		t.Errorf("all: %v, want %v", got, want)
	}
	if got, want := orderSnapSymbols(cands, true), []string{"1332", "1605", "1801"}; !reflect.DeepEqual(got, want) {
		t.Errorf("universe: %v, want %v", got, want)
	}
}
