// news は立花証券のニュース電文を丸ごと溜めるコマンド。
//
// 電文が遡れるのは 90 日だけで、止めた期間は取り返せない。だから選り分けずに
// 全ジャンルの見出しと本文を残し、何に効くかは溜まってから決める。
// 何が流れているかの棚卸しは
// `~/obsidian-vault/20-research/2026-09-tachibana-news-inventory.md`。
package main

import (
	"fmt"
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/spf13/cobra"
)

var (
	appSettings = settings.LoadAppSettings()
	dbPathFlag  string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "news",
		Short: "立花証券のニュース電文を溜めて引く",
	}
	rootCmd.PersistentFlags().StringVar(&dbPathFlag, "db", appSettings.NewsDBPath(), "記録簿（SQLite）の場所")

	rootCmd.AddCommand(newSyncCmd())
	rootCmd.AddCommand(newStatusCmd())
	rootCmd.AddCommand(newListCmd())
	rootCmd.AddCommand(newEventsCmd())

	// panic も記録・通知してから終わる（cli.Guarded）。このコマンドは実行の記録（cli.Run）を
	// 起こさないので run は nil のまま渡す（通知は notify.Alert に直接送る）
	var run *cli.Run
	panicked, err := cli.Guarded("news", &run, rootCmd.Execute)
	if panicked {
		os.Exit(cli.ExitPanic)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
