package backtest

import (
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/usmarket"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// SignalsFor は危険信号の材料を集める。
//
// 有効にしていないゲートの材料は取りに行かない（FRED への通信は us_skip_high を
// 使うときだけ）。取れなかった材料は Simulate 側でエラーにする。
func SignalsFor(arch *archive.Archive, cfg config.Config, panel *Panel, us usmarket.Fetcher, cachePath string) (*Inputs, error) {
	inputs := &Inputs{
		IVPrev: map[string]*float64{},
		Drift:  map[string]*float64{},
		UsRet:  map[string]*float64{},
		Vix:    map[string]*float64{},
	}
	if len(panel.Days) == 0 {
		return inputs, nil
	}
	start, end := panel.Days[0], panel.Days[len(panel.Days)-1]

	if iv, err := regime.IVByDay(arch, start, end); err == nil {
		for _, p := range iv {
			inputs.IVPrev[p.Date.Format(dayLayout)] = p.IVPrev
		}
	}
	if drift, err := regime.TopixDriftSeries(arch, start, end, cfg.Regime.DriftDays); err == nil {
		for _, p := range drift {
			inputs.Drift[p.Date.Format(dayLayout)] = p.Drift
		}
	}
	if cfg.Regime.UsSkipHigh != nil && us != nil {
		sessions, err := usmarket.History(us, cachePath, start, end)
		if err != nil {
			return nil, err
		}
		for day, session := range usmarket.AsOf(sessions, panel.Days) {
			ret := session.SpxRet
			inputs.UsRet[day] = &ret
			// VIX が取れていない日に 0 を渡すと「VIX が低い」と読まれてしまう
			if session.Vix > 0 {
				vix := session.Vix
				inputs.Vix[day] = &vix
			}
		}
	}
	return inputs, nil
}

// Run はアーカイブ（＋米国市場のキャッシュ）から検証する。
func Run(arch *archive.Archive, cfg config.Config, start, end time.Time, us usmarket.Fetcher, cachePath string) (*Result, error) {
	panel, err := LoadPanel(arch, start, end, cfg)
	if err != nil {
		return nil, err
	}
	signals, err := SignalsFor(arch, cfg, panel, us, cachePath)
	if err != nil {
		return nil, err
	}
	return Simulate(panel, cfg, signals)
}

// RunMargin は Run のロング＋ショート版。
func RunMargin(arch *archive.Archive, cfg config.Config, start, end time.Time, us usmarket.Fetcher, cachePath string) (*MarginResult, error) {
	panel, err := LoadPanel(arch, start, end, cfg)
	if err != nil {
		return nil, err
	}
	signals, err := SignalsFor(arch, cfg, panel, us, cachePath)
	if err != nil {
		return nil, err
	}
	return SimulateMargin(panel, cfg, signals)
}
