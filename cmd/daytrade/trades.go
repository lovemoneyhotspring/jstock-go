package main

import (
	"fmt"
	"strings"
	"time"

	dtevaluate "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/evaluate"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/spf13/cobra"
)

func newTradesCmd() *cobra.Command {
	var fromFlag, toFlag, csvFlag, sideFlag string
	var daysFlag int
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "trades",
		Short: "建てた 1 銘柄ずつの明細（銘柄名・株数・建値・手仕舞い値・bp・損益・勝敗の主因）",
		Long: "建てた銘柄を 1 行 1 取引で並べる。review が日ごとの平均で「規則が効いているか」を\n" +
			"見るのに対し、こちらは「何を、いくらで建てて、いくらで手仕舞い、なぜ勝った（負けた）か」。\n\n" +
			"勝敗の主因は、その日その脚の候補全体の平均（day_all_bp）と比べて決める。\n" +
			"同じ向きなら「地合い」、逆なら「選定」。材料は daytrade evaluate が積んだ履歴。",
		RunE: func(cmd *cobra.Command, args []string) error {
			store := historyStore()
			first, err := parseDate(fromFlag)
			if err != nil {
				return err
			}
			last, err := parseDate(toFlag)
			if err != nil {
				return err
			}
			if first.IsZero() && last.IsZero() {
				known := store.Days(dthistory.KindEvaluation)
				if len(known) == 0 {
					if jsonFlag {
						return output.EmitJSON(map[string]any{"ok": true, "rows": []any{}, "totals": []any{}})
					}
					fmt.Println("評価の履歴がまだありません（daytrade evaluate を回す）")
					return nil
				}
				first = known[0]
				if len(known) >= daysFlag {
					first = known[len(known)-daysFlag]
				}
			}
			evaluations, err := store.Read(dthistory.KindEvaluation, history.Range{Start: first, End: last})
			if err != nil {
				return err
			}
			table := dtevaluate.Trades(evaluations)
			if sideFlag != "" {
				table = table.Filter(func(row map[string]any) bool {
					v, _ := row["side"].(string)
					return v == sideFlag
				})
			}
			totals := dtevaluate.TradeTotals(table)

			if jsonFlag {
				return output.EmitJSON(map[string]any{
					"ok": true, "from": dayText(first), "to": dayText(last),
					"rows": output.RowsOf(table), "totals": output.RowsOf(totals),
				})
			}
			if table.Height() == 0 {
				fmt.Println("期間内に建てた取引がありません")
				return nil
			}
			if csvFlag != "" {
				if err := history.WriteCSV(csvFlag, table); err != nil {
					return err
				}
				fmt.Printf("%d 行を %s に書き出しました\n", table.Height(), csvFlag)
			}
			printTradeDetail(table, totals)
			return nil
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&toFlag, "to", "", "終了日 YYYY-MM-DD")
	cmd.Flags().IntVar(&daysFlag, "days", 20, "期間を省いたとき、直近の評価日数")
	cmd.Flags().StringVar(&sideFlag, "side", "", "脚で絞る（BUY / SELL）")
	cmd.Flags().StringVar(&csvFlag, "csv", "", "明細をこのファイルに書き出す")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}

func printTradeDetail(table, totals history.Frame) {
	fmt.Println("明細（建値・手仕舞い値は「実」＝台帳の約定単価、無ければ日足の始値・終値）")
	fmt.Printf("  %-11s %-3s %-6s %-16s %7s %9s %9s %4s %8s %8s %12s %-7s %s\n",
		"日付", "脚", "銘柄", "名称", "株数", "建値", "手仕舞", "価格", "ギャップ", "net bp", "損益", "主因", "注意")
	for _, row := range table.Rows {
		day := ""
		if t, ok := row["day"].(time.Time); ok {
			day = t.Format(DateLayout)
		}
		priced := "想定"
		if strOf(row["priced"]) == dtevaluate.PricedActual {
			priced = "実"
		}
		fmt.Printf("  %-11s %-3s %-6s %-16s %7s %9s %9s %4s %8s %8s %12s %-7s %s\n",
			day, legLabel(strOf(row["side"])), strOf(row["code"]),
			clip(strOf(row["name"]), 16),
			yenOrBlank(row["quantity"]), yenOrBlank(row["entry"]), yenOrBlank(row["exit"]),
			priced, gapText(row["gap"]), bpText(row["net_bp"]), pnlText(row["pnl"]),
			strOf(row["cause"]), strOf(row["note"]))
	}
	if totals.Height() == 0 {
		return
	}
	fmt.Println("\n脚ごとの成績")
	for _, row := range totals.Rows {
		fmt.Printf("  %s: 取引 %d 件・勝ち %d 件（勝率 %s）  平均 %s bp  損益 %s 円\n",
			legLabel(strOf(row["side"])), iOf(row["trades"]), iOf(row["wins"]),
			rateText(row["win_rate"]), bpText(row["avg_net_bp"]), pnlText(row["pnl"]))
		// 勝率だけでは判断できない。同じ勝率でもペイオフ次第で期待値の符号が変わる
		fmt.Printf("      平均利益 %s bp / 平均損失 %s bp（ペイオフ %s）  1 取引の期待値 %s bp\n",
			bpText(row["avg_win_bp"]), bpText(row["avg_loss_bp"]),
			ratioText(row["payoff"]), bpText(row["expectancy_bp"]))
		// 1 発頼みでないか。上位 1 件で稼ぎの大半なら、その 1 件は再現しないかもしれない
		fmt.Printf("      最良 %s（%s bp）  最悪 %s（%s bp）  利益の集中: 上位 1 件 %s / 上位 3 件 %s\n",
			strOf(row["best_code"]), bpText(row["best_bp"]),
			strOf(row["worst_code"]), bpText(row["worst_bp"]),
			rateText(row["top1_share"]), rateText(row["top3_share"]))
		// 選定勝ちが選定負けを上回っていれば順位付けが価値を足している。
		// 地合いの勝ち負けは相場に乗っただけで、規則の証拠にはならない
		fmt.Printf("      勝敗 × 主因: 選定勝ち %d / 選定負け %d ｜ 地合い勝ち %d / 地合い負け %d\n",
			iOf(row["selection_win"]), iOf(row["selection_loss"]),
			iOf(row["market_win"]), iOf(row["market_loss"]))
		fmt.Printf("      実発注 %d 件（執行の乖離 %s bp）  張り付き %d 件\n",
			iOf(row["traded"]), orDash(bpText(row["slippage_bp"])), iOf(row["pinned"]))
	}
	fmt.Println("\n※ 取引数が少ないうちは平均も勝率も揺れる。20 取引に満たない期間の数字で規則を変えない")
}

func legLabel(side string) string {
	switch side {
	case "BUY":
		return "買"
	case "SELL":
		return "売"
	default:
		return side
	}
}

// ratioText は倍率（ペイオフレシオ）。
func ratioText(v any) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", fOf(v))
}

// orDash は空を「—」にする（値が無いことと 0 を見分ける）。
func orDash(text string) string {
	if text == "" {
		return "—"
	}
	return text
}

// clip は表示幅で切る（全角を 2 と数える）。名称が長い銘柄で列が崩れないため。
func clip(text string, width int) string {
	used := 0
	for i, r := range text {
		w := 1
		if r > 0x7f {
			w = 2
		}
		if used+w > width {
			return text[:i]
		}
		used += w
	}
	return text + strings.Repeat(" ", width-used)
}
