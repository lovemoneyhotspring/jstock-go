package main

import (
	"fmt"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/simulate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/spf13/cobra"
)

func newBacktestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backtest",
		Short: "登録された積立戦略の過去検証を実行する",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			barStore := data.NewBarStore(appSettings.BarsDir())

			for _, entry := range cfg.Tactics {
				if !entry.IsEnabled() {
					continue
				}
				var tactic tactics.Tactic = &tactics.Constant{}
				switch entry.Tactic {
				case "bear_stack":
					tactic = tactics.NewBearStack(entry.Multiplier, entry.Fast, entry.Mid, entry.Slow)
				case "stack_ladder":
					tactic = tactics.NewStackLadder(nil, entry.Fast, entry.Mid, entry.Slow)
				case "drawdown_ladder":
					tactic = tactics.NewDrawdownLadder(nil, nil, true, 200)
				}

				var signalBars []domain.Bar
				if entry.SignalSymbol != "" {
					signalBars, _ = barStore.Read(entry.SignalSymbol, "", "")
				}

				for _, sym := range entry.Symbols {
					bars, err := barStore.Read(sym, "", "")
					if err != nil || len(bars) == 0 {
						continue
					}
					p, err := plan.BuildPlanWithSignal(bars, signalBars, false, tactic, entry.MonthlyBudget)
					if err != nil || len(p.Rows) == 0 {
						continue
					}

					res, err := simulate.Simulate(bars, p, entry.MonthlyBudget)
					if err != nil {
						fmt.Printf("[%s] 検証失敗: %v\n", sym, err)
						continue
					}

					fmt.Printf("=== 積立バックテスト: %s (%s) ===\n", entry.ID, sym)
					fmt.Printf("期間: %s 〜 %s\n", res.StartDate, res.EndDate)
					fmt.Printf("総投入額: %s 円 (基本予算の %.2f 倍)\n", res.Contributed, res.CapitalMultiple)
					fmt.Printf("平均取得単価: %.2f 円 (対照群比: %+.2f%%)\n", res.AverageCost, res.CostEdge*100)
					fmt.Printf("期末評価額: %.0f 円 (総リターン: %+.2f%%)\n", res.TerminalValue, res.TotalReturn*100)
					fmt.Printf("増額発動日数: %d 日\n\n", res.BoostedDays)
				}
			}
			return nil
		},
	}
}
