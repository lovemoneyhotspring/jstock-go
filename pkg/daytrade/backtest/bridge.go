package backtest

import (
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/shopspring/decimal"
)

// パネルの 1 行を本番（前夜の plan と 9:00 の気配）と同じ形に写す。
//
// 検証の選定は selection.Rank / RankShort / PickFrom をそのまま呼ぶ。ここが唯一の
// 変換点で、順位付け・2 段階選定・業種上限・配分・株数の規則は backtest には無い。

// candidateOf はパネルの 1 行を前夜の plan と同じ候補にする（selection.Rank の入力）。
// パネルに銘柄名は無いので Name は空、発注記号は 5 桁コードのまま（気配の鍵にしか使わない）。
func candidateOf(r Row) universe.Candidate {
	return universe.Candidate{
		Code:          r.Code,
		Symbol:        r.Code,
		PrevClose:     r.PrevClose,
		Vol20:         r.Vol20,
		EarnYield:     r.EarnYield,
		Sector:        r.Sector,
		ShortInterest: r.ShortInterest,
		Eligible:      r.Eligible,
		ShortEligible: r.ShortEligible,
	}
}

// quoteOf はその日の寄付を 9:00 の気配と同じ形にする。PrevClose も渡す（本番は取得元の
// 基準値段を優先する。パネルでは候補の前日終値と同じ値）。
func quoteOf(r Row) selection.Quote {
	return selection.Quote{
		Symbol:    r.Code,
		Price:     decimal.NewFromFloat(r.Open),
		PrevClose: decimal.NewFromFloat(r.PrevClose),
	}
}
