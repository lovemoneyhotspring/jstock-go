package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/engine"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newBacktestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backtest",
		Short: "過去データを用いてスイング戦略のバックテストを実行する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}
			stratCfg, err := wbjpcfg.LoadStrategiesConfig(configDirFlag)
			if err != nil {
				return err
			}

			barStore := data.NewBarStore(appSettings.BarsDir())
			allBars := make(map[string][]domain.Bar)
			for _, sym := range setCfg.Universe.Symbols {
				bars, err := barStore.Read(sym, "", "")
				if err == nil && len(bars) > 0 {
					allBars[sym] = bars
				}
			}

			if len(allBars) == 0 {
				return fmt.Errorf("バックテスト用の足データがありません。先に 'wbjp sync' を実行してください")
			}

			strats, weights := buildStrategies(stratCfg)
			combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

			stats, err := engine.RunBacktest(setCfg, stratCfg, strats, weights, combineFunc, allBars, decimal.NewFromInt(1000000))
			if err != nil {
				return err
			}

			fmt.Println("=== スイング売買 バックテスト結果 ===")
			fmt.Printf("初期資金: %s 円\n", stats.InitialEquity)
			fmt.Printf("最終資産: %s 円\n", stats.FinalEquity.Round(0))
			fmt.Printf("総リターン: %.2f%%\n", stats.TotalReturn.Mul(decimal.NewFromInt(100)).InexactFloat64())
			fmt.Printf("最大ドローダウン: %.2f%%\n", stats.MaxDrawdown.Mul(decimal.NewFromInt(100)).InexactFloat64())
			fmt.Printf("総約定数: %d 回\n", stats.TotalFills)
			return nil
		},
	}
}
