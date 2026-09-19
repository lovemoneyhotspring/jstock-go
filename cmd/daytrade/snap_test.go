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

// ETF・指数は plan の銘柄の前に置く（締め切りで切れても地合いは残す）。重複は重ねない。
func TestMergeExtraSymbols(t *testing.T) {
	plan := []string{"1332", "1605", "1321"}
	got, added := mergeExtraSymbols([]string{"1321", "101", " ", "101"}, plan)
	if want := []string{"1321", "101", "1332", "1605"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
	if added != 2 {
		t.Errorf("足した数 = %d, want 2", added)
	}
	// 追加が無ければ plan のまま
	if got, added := mergeExtraSymbols(nil, plan); added != 0 || !reflect.DeepEqual(got, plan) {
		t.Errorf("追加なし: %v / %d", got, added)
	}
}
