package main

import (
	"fmt"
	"strconv"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
)

// marginEndpoint は信用残（週次、銘柄別）のアーカイブ端点。
const marginEndpoint = "markets_margin_interest"

// needsMargin は有効な戦略の中に信用残を使うものがあるか。
//
// 無いのに読むと、毎朝の run が 200 万行の走査を無駄に払う。
func needsMargin(stratCfg *wbjpcfg.StrategiesConfig) bool {
	for _, sc := range stratCfg.Strategies {
		if sc.Name == "margin_balance" && (sc.Enabled == nil || *sc.Enabled) {
			return true
		}
	}
	return false
}

// loadMarginBook は J-Quants アーカイブから symbols の信用残（週次）を読む。
//
// アーカイブが無い・端点が空なら nil を返し、戦略は意見を出さない（黙る）。
// 東証の株式以外（指数・米国）は信用残が無いので飛ばす。
func loadMarginBook(symbols []string) (*strategy.MarginBook, error) {
	return loadMarginBookWithLag(symbols, strategy.MarginPublicationLag)
}

// loadMarginBookWithLag は公表までの遅れを変えて読む（backtest --margin-lag-days の検証用）。
func loadMarginBookWithLag(symbols []string, lagDays int) (*strategy.MarginBook, error) {
	arch := archive.NewArchive(appSettings.JQuantsArchiveDir())
	ep, err := archive.LookupEndpoint(marginEndpoint)
	if err != nil {
		return nil, err
	}

	// アーカイブのコードは 5 桁（72030）。銘柄コードから引けるように控える。
	want := make(map[string]string, len(symbols))
	for _, sym := range symbols {
		code, isIndex, err := data.ToJQuantsCode(sym)
		if err != nil || isIndex {
			continue
		}
		if len(code) == 4 {
			code += "0"
		}
		want[code] = sym
	}
	if len(want) == 0 {
		return nil, nil
	}

	frame, err := arch.ReadWhere(ep, archive.ReadOptions{
		Columns: []string{"Code", "LongVol", "ShrtVol"},
		Keep: func(row archive.RowView) bool {
			_, ok := want[row.Text("Code")]
			return ok
		},
	})
	if err != nil {
		return nil, fmt.Errorf("信用残を読めません: %w", err)
	}
	if frame == nil || frame.Height() == 0 {
		return nil, nil
	}

	records := make(map[string][]strategy.MarginRecord)
	for i := 0; i < frame.Height(); i++ {
		sym, ok := want[text(frame.Get(i, "Code"))]
		if !ok {
			continue
		}
		date := text(frame.Get(i, ep.DateColumn))
		long, err1 := strconv.ParseFloat(text(frame.Get(i, "LongVol")), 64)
		short, err2 := strconv.ParseFloat(text(frame.Get(i, "ShrtVol")), 64)
		if date == "" || err1 != nil || err2 != nil {
			continue
		}
		records[sym] = append(records[sym], strategy.MarginRecord{Date: date, Long: long, Short: short})
	}
	if len(records) == 0 {
		return nil, nil
	}
	return strategy.NewMarginBookWithLag(records, lagDays), nil
}

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
