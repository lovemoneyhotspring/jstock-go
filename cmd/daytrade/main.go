// daytrade はデイトレード（寄付で買い、大引で売る）のコマンドライン。
//
// 安全の原則は wbjp / accum と同じ: open / close は既定で **dry-run**。実際に発注するには
// --live が要り、本番口座ではさらに WBJP_ENV=prod が要る。
//
// 1 日の流れ（すべて JST）:
//
//	20:30  daytrade plan          前夜。アーカイブから翌営業日の母集団を作る
//	09:00  daytrade open --live   気配でギャップ下位 N 銘柄を選び、成行で買う
//	15:20  daytrade close --live  当日買った分を成行で売る（15:25 以降は引け値）
//	16:00  daytrade verify        持ち越しが無いかブローカーに照会する
package main

import (
	"os"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "daytrade",
		Short: "デイトレ（寄付で買い、大引で売る）",
		// 実行の記録（ログ・ダイジェスト）はどのサブコマンドでも要るので、
		// 入口で 1 回だけ起こして出口で 1 回だけ畳む。
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			setupRun(cmd.Name())
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			teardownRun()
		},
	}

	rootCmd.PersistentFlags().StringVar(&configDirFlag, "config-dir", dtconfig.DefaultConfigDir, "設定ディレクトリ")

	rootCmd.AddCommand(newPlanCmd())
	rootCmd.AddCommand(newOpenCmd())
	rootCmd.AddCommand(newCloseCmd())
	rootCmd.AddCommand(newVerifyCmd())
	rootCmd.AddCommand(newHistoryCmd())
	rootCmd.AddCommand(newEvaluateCmd())
	rootCmd.AddCommand(newReviewCmd())
	rootCmd.AddCommand(newStatusCmd())
	rootCmd.AddCommand(newQuotesCmd())
	rootCmd.AddCommand(newBacktestCmd())

	if err := rootCmd.Execute(); err != nil {
		teardownRun()
		os.Exit(1)
	}
}
