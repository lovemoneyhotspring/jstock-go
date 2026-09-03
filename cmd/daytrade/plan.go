package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/spf13/cobra"
)

func newPlanCmd() *cobra.Command {
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "翌営業日の母集団を作る（前夜に cron で回す）。9:00 の open はこれを読む",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if !cfg.Capital.Enabled {
				fmt.Println("jp_gap_fade は無効（capital.enabled = false）。何もしません")
				logInfo("daytrade.skip", "戦略が無効", map[string]any{"reason": "disabled"})
				digest.Skipped("disabled")
				return nil
			}
			day, err := planDay(dateFlag, clock.NowUTC())
			if err != nil {
				return err
			}
			logConfig(cfg, "plan", map[string]any{"day": day.Format(DateLayout)})
			p, err := buildPlan(cfg, day)
			if err != nil {
				return crash("候補の作成", "daytrade.crash", err)
			}
			printPlan(p, cfg)
			return nil
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日／次の営業日）")
	return cmd
}

// planDay は判定日。指定が無ければ「引け前なら今日、引け後なら次の営業日」。
func planDay(text string, now time.Time) (time.Time, error) {
	day, err := parseDate(text)
	if err != nil {
		return time.Time{}, err
	}
	if !day.IsZero() {
		return day, nil
	}
	cal := calendar.FromArchive(openArchive())
	local := clock.ToZone(now, jst)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	// 15:30 は東証の引け。引ける前なら「今日ぶん」を作り直す余地がある
	if local.Hour()*60+local.Minute() < 15*60+30 {
		return cal.NextTradingDay(today, true)
	}
	return cal.NextTradingDay(today, false)
}

// buildPlan は候補を作り、「最新」（open が読む）と履歴（振り返り用）の両方に残す。
func buildPlan(cfg dtconfig.Config, day time.Time) (dtplan.Plan, error) {
	arch := openArchive()
	p, err := dtplan.Build(arch, cfg, day, calendar.FromArchive(arch), clock.NowUTC())
	if err != nil {
		return dtplan.Plan{}, err
	}
	parquetPath, _, err := dtplan.Save(p, appSettings.DaytradeDir())
	if err != nil {
		return dtplan.Plan{}, err
	}
	frame, metaFrame := dthistory.PlanFrames(p)
	appendHistory(dthistory.KindPlan, frame, p.Day())
	appendHistory(dthistory.KindPlanMeta, metaFrame, p.Day())

	logInfo("daytrade.plan", "候補を作成", map[string]any{
		"day": p.Meta.Day, "prev_day": p.Meta.PrevDay,
		"candidates": p.Meta.Candidates, "eligible": p.Meta.Eligible,
		"short_eligible": p.Meta.ShortEligible,
		"positions":      p.Meta.Positions, "budget": p.Meta.BudgetPerOrder,
		"iv_prev": p.Meta.IVPrev, "path": parquetPath,
		"history": appSettings.DaytradeHistoryDir(),
	})
	digest.Note(map[string]any{
		"candidates": p.Meta.Candidates,
		"eligible":   p.Meta.Eligible,
		"positions":  p.Meta.Positions,
	})
	return p, nil
}

// printPlan は候補の概要。N と予算は plan 時点ではなく**今の設定**（資金を変えたら即反映）。
func printPlan(p dtplan.Plan, cfg dtconfig.Config) {
	n := cfg.Capital.Positions()
	budget := "—（資金 0）"
	if n > 0 {
		budget = yen(cfg.Capital.BudgetPerOrder()) + " 円"
	}
	fmt.Printf("判定日 %s（前営業日 %s）  候補 %d / %d 銘柄  N=%d  1 注文 %s\n",
		p.Meta.Day, p.Meta.PrevDay, p.Meta.Eligible, p.Meta.Candidates, n, budget)

	var signals []string
	if p.Meta.IVPrev != nil {
		signals = append(signals, fmt.Sprintf("IV %.1f", *p.Meta.IVPrev))
	}
	if p.Meta.Drift != nil {
		signals = append(signals, fmt.Sprintf("市場の日中ドリフト %+.1f bp/日", *p.Meta.Drift*1e4))
	}
	if len(signals) > 0 {
		fmt.Println("信号: " + strings.Join(signals, "  "))
	}

	excluded := map[string]int{}
	earnPrev, discToday, alert, jsfStop := 0, 0, 0, 0
	for _, c := range p.Candidates {
		if !c.Eligible {
			excluded[c.Segment]++
		}
		if c.EarnPrev {
			earnPrev++
		}
		if c.DiscToday {
			discToday++
		}
		if c.Alert {
			alert++
		}
		if c.JsfStop {
			jsfStop++
		}
	}
	var parts []string
	for _, segment := range []string{"growth", "other", "prime", "standard"} {
		if n := excluded[segment]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", segment, n))
		}
	}
	fmt.Printf("除外: %s  決算(前日引け後) %d  決算(当日予定) %d  日々公表 %d\n",
		strings.Join(parts, "、"), earnPrev, discToday, alert)

	if cfg.Margin.Enabled {
		minGap, _ := cfg.Margin.MinGap.Float64()
		fmt.Printf("ショート: 対象 %d 銘柄（%s・貸借・ギャップ ≥ %.0f%%）  売り禁 %d  N=%d  1 注文 %s 円\n",
			len(p.ShortEligible()), strings.Join(cfg.Margin.Segments, "/"), minGap*100,
			jsfStop, cfg.Margin.Positions(), yen(cfg.Margin.BudgetPerOrder()))
	}
	fmt.Printf("保存先 %s\n", appSettings.DaytradeDir())
}

// refreshIV は前夜の plan に IV が無ければ取り直す。
//
// オプションの足は 27:00 頃の更新なので、20:30 の plan には前日の IV がまだ無い。
// 朝の sync で入っていればここで拾う。それでも無ければゲートは効かせず取引する
// （低 IV の日は期待値がほぼ 0 で、負ではない）。
func refreshIV(cfg dtconfig.Config, p dtplan.Plan) dtplan.Plan {
	if cfg.Regime.IVGate.LessThanOrEqual(decimalZero) || p.Meta.IVPrev != nil {
		return p
	}
	prevDay, err := time.Parse(DateLayout, p.Meta.PrevDay)
	if err != nil {
		return p
	}
	value, err := regime.IVOn(openArchive(), prevDay)
	if err != nil || value == nil {
		logWarn("daytrade.iv_missing", "前日の IV がアーカイブに無いためゲート無しで進行",
			map[string]any{"prev_day": p.Meta.PrevDay})
		return p
	}
	p.Meta.IVPrev = value
	return p
}

// logConfig は実行時の設定を 1 レコードに残す。
// 不具合の再現には「そのとき何が有効だったか」が要る。
func logConfig(cfg dtconfig.Config, phase string, extra map[string]any) {
	fields := map[string]any{
		"strategy":             cfg.StrategyName(),
		"env":                  string(appSettings.Env),
		"phase":                phase,
		"enabled":              cfg.Capital.Enabled,
		"max_capital":          cfg.Capital.MaxCapital.String(),
		"order_budget":         cfg.Capital.OrderBudget.String(),
		"positions":            cfg.Capital.Positions(),
		"budget_per_order":     cfg.Capital.BudgetPerOrder().String(),
		"segments":             cfg.Universe.Segments,
		"min_turnover":         cfg.Universe.MinTurnover.String(),
		"exclude_cap_terciles": cfg.Universe.ExcludeCapTerciles,
		"max_gap":              cfg.Signal.MaxGap.String(),
		"min_gap":              cfg.Signal.MinGap.String(),
		"skip_months":          cfg.Regime.SkipMonths,
		"iv_gate":              cfg.Regime.IVGate.String(),
		"equity_curve_days":    cfg.Regime.EquityCurveDays,
		"equity_curve_scale":   cfg.Regime.EquityCurveScale.String(),
		"weighting":            cfg.Capital.Weighting,
		"us_vix_override":      cfg.Regime.UsVixOverride.String(),
		"quote_source":         cfg.Execution.QuoteSource,
		"entry_window":         cfg.Execution.EntryWindow,
		"exit_window":          cfg.Execution.ExitWindow,
		"max_quote_age":        cfg.Execution.MaxQuoteAge,
		"kill_switch":          cfg.Execution.KillSwitch,
		"state_dir":            appSettings.StateDir,
		"data_dir":             appSettings.DataDir,
	}
	if cfg.Regime.DriftGate != nil {
		fields["drift_gate"] = cfg.Regime.DriftGate.String()
	}
	if cfg.Regime.UsSkipHigh != nil {
		fields["us_skip"] = []string{cfg.Regime.UsSkipLow.String(), cfg.Regime.UsSkipHigh.String()}
	}
	for k, v := range extra {
		fields[k] = v
	}
	logInfo("daytrade.config", "実行時の設定", fields)
}
