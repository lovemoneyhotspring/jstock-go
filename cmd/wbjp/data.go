package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/spf13/cobra"
)

// newDataCmd は足データの取得と確認をまとめるサブグループ。
//
// トップレベルの sync は残したまま、check / status をここに置く（Python 版の
// data サブコマンドと同じ並びにして、運用手順書をそのまま使えるようにする）。
func newDataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "data",
		Short: "足データの取得と確認",
	}
	cmd.AddCommand(newDataCheckCmd())
	cmd.AddCommand(newDataStatusCmd())
	return cmd
}

func newDataCheckCmd() *cobra.Command {
	var shouldNotify bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "日足の蓄積が止まっていないかを調べる。問題があれば異常終了する",
		Long: "日足の蓄積が止まっていないかを調べる。問題があれば異常終了する。\n\n" +
			"cron で data sync の後に回し、--notify で Slack 等に知らせる。",
		// 使い方の誤りではなく検査の結果で失敗するので、使い方は出さない
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}
			if len(setCfg.Universe.Symbols) == 0 {
				return fmt.Errorf("universe.symbols が空です")
			}

			results, err := data.Check(appSettings.BarsDir(), setCfg.Universe.Symbols, time.Time{})
			if err != nil {
				return err
			}

			var problems []data.Coverage
			fmt.Printf("足の蓄積状況 (%s)\n", appSettings.ConfigDir)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "銘柄\t本数\t最初\t最終\t状態")
			for _, c := range results {
				if !c.Healthy() {
					problems = append(problems, c)
				}
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n",
					c.Symbol, c.Bars, dash(c.First), dash(c.Last), c.Describe())
			}
			w.Flush()

			if len(problems) == 0 {
				fmt.Println("すべて正常")
				return nil
			}

			lines := make([]string, 0, len(problems))
			for _, c := range problems {
				lines = append(lines, fmt.Sprintf("%s: %s", c.Symbol, c.Describe()))
			}
			fmt.Printf("\n%d 件の問題\n", len(problems))
			if shouldNotify {
				// cron の中で黙って落ちると、足が止まっていることに気づくのが遅れる
				notify.Alert(fmt.Sprintf("足の蓄積に %d 件の問題（%s）", len(problems), appSettings.ConfigDir),
					strings.Join(lines, "\n"), nil)
			}
			return fmt.Errorf("足の蓄積に %d 件の問題があります", len(problems))
		},
	}

	cmd.Flags().BoolVar(&shouldNotify, "notify", false,
		"問題があれば "+notify.WebhookEnvVar+" に通知する")
	return cmd
}

func newDataStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "保存済みの日足を一覧する",
		RunE: func(cmd *cobra.Command, args []string) error {
			summary, err := data.NewBarStore(appSettings.BarsDir()).Summary()
			if err != nil {
				return err
			}
			if summary.Height() == 0 {
				fmt.Println("保存済みの足はありません")
				return nil
			}

			names := summary.Names()
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, strings.Join(names, "\t"))
			for _, row := range summary.Rows {
				cells := make([]string, len(names))
				for i, name := range names {
					cells[i] = corehistory.Cell(row[name])
				}
				fmt.Fprintln(w, strings.Join(cells, "\t"))
			}
			w.Flush()
			return nil
		},
	}
}

func dash(text string) string {
	if text == "" {
		return "—"
	}
	return text
}
