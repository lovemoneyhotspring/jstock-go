package main

import (
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/shopspring/decimal"
)

// trendValues は銘柄ごとのトレンド判定値（trend_exit_kind の線）を返す。
//
// 残り玉（ランナー）をどこで手仕舞うかの基準。trend_exit_sma が未設定なら
// 判定しないので空を返す。線の計算は risk.TrendValue（テストあり）。
func trendValues(barStore *data.BarStore, symbols []string, stops wbjpcfg.StopsConfig) map[string]decimal.Decimal {
	if stops.TrendExitSMA == nil || *stops.TrendExitSMA <= 0 {
		return nil
	}
	out := make(map[string]decimal.Decimal, len(symbols))
	for _, sym := range symbols {
		bars, err := barStore.Read(sym, "", "")
		if err != nil {
			continue
		}
		if v, ok := risk.TrendValue(bars, stops); ok {
			out[sym] = v
		}
	}
	return out
}

// regimeInput は地合い判定に使う指数の直近値を組み立てる。
//
// 指数の足が読めなければ空のまま返す（RegimeExposure 側が弱気として扱う）。
// 指標の計算は risk.RegimeInputFromBars（テストあり）。
func regimeInput(barStore *data.BarStore, cfg wbjpcfg.RegimeConfig) risk.RegimeInput {
	if cfg.Benchmark == "" {
		return risk.RegimeInput{}
	}
	bars, err := barStore.Read(cfg.Benchmark, "", "")
	if err != nil {
		return risk.RegimeInput{}
	}
	return risk.RegimeInputFromBars(bars, cfg)
}

// positionList は建玉を銘柄順の並びにする（記録の並びを実行ごとに揃える）。
func positionList(positions map[string]domain.Position) []domain.Position {
	out := make([]domain.Position, 0, len(positions))
	for _, pos := range positions {
		out = append(out, pos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// symbolSet は銘柄の並びを集合にする。
func symbolSet(symbols []string) map[string]struct{} {
	out := make(map[string]struct{}, len(symbols))
	for _, sym := range symbols {
		out[sym] = struct{}{}
	}
	return out
}

// quantitiesOf は建玉の数量だけを取り出す。
func quantitiesOf(positions map[string]domain.Position) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(positions))
	for sym, pos := range positions {
		out[sym] = pos.Quantity
	}
	return out
}
