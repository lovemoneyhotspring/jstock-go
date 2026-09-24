package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/spf13/cobra"
)

func newPlanCmd() *cobra.Command {
	var days int

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "直近の投下額を銘柄ごとに表示する（今いくら買うか）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			if days < 1 {
				return fmt.Errorf("--days は 1 以上で指定してください: %d", days)
			}
			return printPlan(os.Stdout, cfg, data.NewBarStore(appSettings.BarsDir()), clock.NowUTC(), days)
		},
	}

	cmd.Flags().IntVar(&days, "days", 10, "直近何営業日ぶんを表示するか")
	return cmd
}

// printPlan は有効な戦略の銘柄ごとに直近 days 営業日の投下額を out に書く。
//
// 判定用の足（^IXIC など）が読めない・空の戦略は、run と同じくその戦略の銘柄を
// 「見送り（判定用の足なし）」と出す。BuildPlanWithSignal は判定用が空なら買う銘柄
// 自身の足に倒れるので、黙って計算すると run（A7 で見送る）と違う額を見せてしまう。
func printPlan(out io.Writer, cfg *accumcfg.AccumConfig, barStore *data.BarStore, now time.Time, days int) error {
	total := int64(0)
	var blocked []string
	var missing []string
	var noSignal []string

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "日付\t銘柄\t終値\t倍率\t基本\t増額\t投下額\t理由")

	for _, entry := range cfg.Active() {
		// 設定に書かれたパラメータ（倍率・段表・発注時間帯）をそのまま反映する。
		tactic, err := entry.Build()
		if err != nil {
			return err
		}

		var signalBars []domain.Bar
		if entry.SignalSymbol != "" {
			bars, rerr := barStore.Read(entry.SignalSymbol, "", "")
			if rerr != nil || len(bars) == 0 {
				for _, sym := range entry.Symbols {
					noSignal = append(noSignal, fmt.Sprintf("%s（%s）", sym, entry.SignalSymbol))
				}
				continue
			}
			signalBars = bars
		}

		window := tactic.Window()
		for _, sym := range entry.Symbols {
			bars, err := barStore.Read(sym, "", "")
			if err != nil || len(bars) == 0 {
				missing = append(missing, sym)
				continue
			}
			p, err := plan.BuildPlanWithSignal(bars, signalBars, entry.SignalLags(), tactic, entry.MonthlyBudget)
			if err != nil || len(p.Rows) == 0 {
				continue
			}
			if window.Enabled && !window.Allows(now) {
				blocked = append(blocked, fmt.Sprintf("%s（%s）", sym, window.Describe()))
			}

			start := len(p.Rows) - days
			if start < 0 {
				start = 0
			}
			for i := start; i < len(p.Rows); i++ {
				r := p.Rows[i]
				fmt.Fprintf(w, "%s\t%s\t%s\t%.1f\t%s\t%s\t%s\t%s\n",
					r.Date, sym, r.Close, r.Multiplier, r.Base, r.Extra, r.Amount, r.Reason)
			}
			total += p.Rows[len(p.Rows)-1].Amount.IntPart()
		}
	}
	w.Flush()

	if len(noSignal) > 0 {
		fmt.Fprintf(out, "\n見送り（判定用の足なし）: %s（accum sync を実行してください。run も同じく見送ります）\n",
			strings.Join(noSignal, "、"))
	}
	if len(missing) > 0 {
		fmt.Fprintf(out, "\n足データがありません: %s（accum sync を実行してください）\n", strings.Join(missing, ", "))
	}
	fmt.Fprintf(out, "\n最終日の投下額 合計: %d\n", total)
	if len(blocked) > 0 {
		// 投下額は時間帯で変わらない。日足で決まった金額を「いつ発注してよいか」
		// だけを制御しているので、額の確認自体はいつでもできる
		fmt.Fprintf(out, "いまは発注時間帯の外です（現在 %s）: %s\n",
			clock.Fmt(now, clock.Tokyo, false), strings.Join(blocked, "、"))
		fmt.Fprintln(out, "※ 時間帯は投下額を変えない。日足で決まる金額を、いつ発注してよいかだけを制御する。")
	}
	return nil
}
