// rate はグレイル NET のレーティング一覧を定期的に取り、
// 「どの証券会社が何時ごろ出すか」を測れる形で溜めるコマンド。
//
// 一覧ページに時刻は載っていないので、短い間隔で取りに行き、
// 行を初めて見た時刻（first_seen_at）を掲載時刻の代わりに使う。
// 取りに行く間隔がそのまま時刻の分解能になる。
package main

import (
	"fmt"
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/spf13/cobra"
)

var (
	appSettings = settings.LoadAppSettings()
	dbPathFlag  string
	urlFlag     string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "rate",
		Short: "証券会社のレーティング情報を集めて溜める",
	}
	rootCmd.PersistentFlags().StringVar(&dbPathFlag, "db", appSettings.RateDBPath(), "記録簿（SQLite）の場所")
	rootCmd.PersistentFlags().StringVar(&urlFlag, "url", "", "取得元 URL（既定は一覧ページ）")

	rootCmd.AddCommand(newFetchCmd())
	rootCmd.AddCommand(newSyncCmd())
	rootCmd.AddCommand(newHistoryCmd())
	rootCmd.AddCommand(newDirectionCmd())
	rootCmd.AddCommand(newLatestCmd())
	rootCmd.AddCommand(newTimingCmd())
	rootCmd.AddCommand(newQueryCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
