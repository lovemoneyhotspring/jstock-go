package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
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
//
// ここはアーカイブを読むだけ。コードの対応と行の解釈は strategy 側（テストあり）。
func loadMarginBookWithLag(symbols []string, lagDays int) (*strategy.MarginBook, error) {
	arch := archive.NewArchive(appSettings.JQuantsArchiveDir())
	ep, err := archive.LookupEndpoint(marginEndpoint)
	if err != nil {
		return nil, err
	}

	want := strategy.MarginCodes(symbols)
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

	rows := make([]strategy.MarginRow, 0, frame.Height())
	for i := 0; i < frame.Height(); i++ {
		rows = append(rows, strategy.MarginRow{
			Code:  text(frame.Get(i, "Code")),
			Date:  text(frame.Get(i, ep.DateColumn)),
			Long:  text(frame.Get(i, "LongVol")),
			Short: text(frame.Get(i, "ShrtVol")),
		})
	}
	return strategy.NewMarginBookFromRows(want, rows, lagDays), nil
}

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
