package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/simulate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/spf13/cobra"
)

func newBacktestCmd() *cobra.Command {
	var from, to string

	cmd := &cobra.Command{
		Use:   "backtest",
		Short: "設定どおりに積み立てた場合の結果を銘柄ごとに出す",
		RunE: func(cmd *cobra.Command, args []string) error {
			start, err := parseDay(from, "--from")
			if err != nil {
				return err
			}
			end, err := parseDay(to, "--to")
			if err != nil {
				return err
			}

			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			barStore := data.NewBarStore(appSettings.BarsDir())

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "=== 積立バックテスト ===")
			fmt.Fprintln(w, "銘柄\t戦略\t投入\t倍率\t単価\t対照比\t期末\tリターン")

			for _, entry := range cfg.Active() {
				// 設定に書かれたパラメータ（倍率・段表）をそのまま反映する。
				tactic, err := entry.Build()
				if err != nil {
					return err
				}

				var signalBars []domain.Bar
				if entry.SignalSymbol != "" {
					// 判定用の足は終了日だけで切る。開始日で切ると移動平均の
					// 助走が足りず、期間の頭で倍率が出なくなる
					signalBars, _ = barStore.Read(entry.SignalSymbol, "", end)
				}

				for _, sym := range entry.Symbols {
					bars, err := barStore.Read(sym, start, end)
					if err != nil || len(bars) == 0 {
						continue
					}
					if len(bars) < tactic.WarmupBars() {
						fmt.Fprintf(os.Stderr, "%s: 足が %d 本しかなく、%d 本必要です。飛ばします\n",
							sym, len(bars), tactic.WarmupBars())
						continue
					}
					p, err := plan.BuildPlanWithSignal(bars, signalBars, entry.SignalLags(), tactic, entry.MonthlyBudget)
					if err != nil || len(p.Rows) == 0 {
						continue
					}

					res, err := simulate.Simulate(bars, p, entry.MonthlyBudget)
					if err != nil {
						fmt.Fprintf(os.Stderr, "[%s] 検証失敗: %v\n", sym, err)
						continue
					}
					contributed, _ := res.Contributed.Float64()
					fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\t%s\t%+.2f%%\t%s\t%+.0f%%\n",
						sym, entry.ID, yen(contributed), res.CapitalMultiple, yen(res.AverageCost),
						res.CostEdge*100, yen(res.TerminalValue), res.TotalReturn*100)
				}
			}
			w.Flush()

			fmt.Println("\n※ 倍率＝基本予算だけの場合に対する総投入額の倍率。単価/投入/期末は万。")
			fmt.Println("※ 対照群＝同じ総投入額を毎月均等に投じた場合。マイナスなら安く買えた。")
			fmt.Println("　 増額分の原資は新規資金（賞与など）を前提としている。積立予算を取り置いて")
			fmt.Println("　 作ると待機が生じ、増額の利益を打ち消す。")
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&to, "to", "", "終了日 YYYY-MM-DD")
	return cmd
}
