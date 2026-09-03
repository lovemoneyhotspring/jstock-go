package main

import (
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/usmarket"
)

// recentPnL は資産曲線ゲートの入力——**ロング側**の直近 N 営業日の実現損益。
//
// ショートは含めない（バックテストと同じ定義）。本発注の履歴が 1 日も無ければ nil
// （始めたばかりの口座を「負けている」と誤読して縮めないため）。確定していない日は
// 除いて合計する。
func recentPnL(cfg dtconfig.Config, day time.Time, led *dtledger.Ledger) (*float64, error) {
	cal := calendar.FromArchive(openArchive())
	days, err := cal.PreviousTradingDays(day, cfg.Regime.EquityCurveDays)
	if err != nil {
		return nil, err
	}
	history, err := led.RealizedPnL(days, "long")
	if err != nil {
		return nil, err
	}
	traded := false
	for _, d := range days {
		entries, err := led.EntriesOn(d)
		if err != nil {
			return nil, err
		}
		for _, o := range entries {
			if !o.IsDryRun() && o.Leg() == "long" {
				traded = true
				break
			}
		}
		if traded {
			break
		}
	}
	var incomplete []string
	total := 0.0
	known := 0
	for _, d := range days {
		v := history[d.Format(DateLayout)]
		if v == nil {
			incomplete = append(incomplete, d.Format(DateLayout))
			continue
		}
		total += *v
		known++
	}
	if len(incomplete) > 0 {
		logWarn("daytrade.pnl_incomplete",
			"実現損益が確定していない日があり、その日を除いて資産曲線を評価",
			map[string]any{"days": incomplete})
	}
	if !traded || known == 0 {
		return nil, nil
	}
	return &total, nil
}

// usmarketLatest は前夜の米国セッション（S&P500・VIX）。
func usmarketLatest(day time.Time) (*usmarket.Session, error) {
	return usmarket.LatestBefore(usmarket.NewFredFetcher(), day)
}
