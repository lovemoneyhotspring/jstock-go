package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/basket"
	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/spf13/cobra"
)

func newBasketCmd() *cobra.Command {
	var from, to string
	var showWeights bool

	cmd := &cobra.Command{
		Use:   "basket",
		Short: "バスケット（複数銘柄への配分）で積み立てた結果を基準銘柄と比べる",
		Long:  "足は accum sync で先に取る。設定は accum.toml の [[baskets]]。",
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
			active := cfg.ActiveBaskets()
			if len(active) == 0 {
				return fmt.Errorf("有効なバスケットがありません（accum.toml の [[baskets]]）")
			}
			barStore := data.NewBarStore(appSettings.BarsDir())

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "=== バスケット積立（万） ===")
			fmt.Fprintln(w, "id\t開始\t投入\t期末\tXIRR\tDD\t基準期末\t基準XIRR\t基準DD")

			var last *basket.BasketResult
			for i := range active {
				entry := &active[i]
				schedule, err := entry.BuildSchedule()
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v\n", err)
					continue
				}
				tactic, err := entry.BuildTactic()
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v\n", err)
					continue
				}
				tilt, err := entry.BuildTilt()
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v\n", err)
					continue
				}

				// 構成銘柄の足。足が無い銘柄は除いて、残りの間で比率を正規化する
				bars := map[string][]domain.Bar{}
				var missing []string
				for _, symbol := range schedule.Symbols() {
					frame, err := barStore.Read(symbol, start, end)
					if err != nil || len(frame) == 0 {
						missing = append(missing, symbol)
						continue
					}
					bars[symbol] = frame
				}
				if len(missing) > 0 {
					sort.Strings(missing)
					fmt.Fprintf(os.Stderr, "足データがありません: %v（accum sync を実行してください）\n", missing)
				}
				if len(bars) == 0 {
					continue
				}

				var benchmark []domain.Bar
				if entry.Benchmark != "" {
					benchmark, _ = barStore.Read(entry.Benchmark, start, end)
				}

				if showWeights {
					weights := schedule.At(clock.TodayUTC().Format("2006-01-02"))
					symbols := make([]string, 0, len(weights))
					for symbol := range weights {
						symbols = append(symbols, symbol)
					}
					sort.Slice(symbols, func(i, j int) bool { return weights[symbols[i]] > weights[symbols[j]] })
					parts := make([]string, 0, len(symbols))
					for _, symbol := range symbols {
						parts = append(parts, fmt.Sprintf("%s %.1f%%", symbol, weights[symbol]*100))
					}
					fmt.Printf("%s いま有効な配分: %v\n", entry.ID, parts)
				}

				plans, err := basket.BuildBasketPlan(bars, basket.BasketSettings{
					MonthlyBudget: entry.MonthlyBudget,
					Schedule:      schedule,
					Tactic:        tactic,
					Tilt:          tilt,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", entry.ID, err)
					continue
				}
				result, err := basket.SimulateBasket(bars, plans, benchmark)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", entry.ID, err)
					continue
				}
				last = result

				benchTerminal, benchXIRR, benchDD := "—", "—", "—"
				if result.Benchmark != nil {
					benchTerminal = yen(result.Benchmark.TerminalValue)
					benchXIRR = fmt.Sprintf("%+.1f%%", result.Benchmark.XIRR*100)
					benchDD = fmt.Sprintf("%.0f%%", result.Benchmark.MaxDrawdown*100)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%+.1f%%\t%.0f%%\t%s\t%s\t%s\n",
					entry.ID, result.Start[:7],
					yen(result.Basket.Contributed), yen(result.Basket.TerminalValue),
					result.Basket.XIRR*100, result.Basket.MaxDrawdown*100,
					benchTerminal, benchXIRR, benchDD)
			}
			w.Flush()

			if last == nil {
				return nil
			}
			fmt.Printf("\n※ 終了 %s。基準＝同じ日に同じ額を benchmark に投じた場合。XIRR は年率の内部収益率。\n", last.End)
			fmt.Println("※ 最大DD は時間加重の評価額指数から。投下額の増減による見かけの変動は含まない。")
			fmt.Println("※ 足の無い銘柄は除いて正規化する。")
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&to, "to", "", "終了日 YYYY-MM-DD")
	cmd.Flags().BoolVar(&showWeights, "weights", false, "いま有効な配分を表示する")
	return cmd
}
