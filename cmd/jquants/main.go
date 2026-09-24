package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/spf13/cobra"
)

func main() {
	os.Exit(execute(os.Args[1:]))
}

func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "jquants",
		Short: "J-Quants データの蓄積と横断クエリツール",
		// エラーは execute が出す（exitError は RunE の中で表示済みなので重ねない）
		SilenceErrors: true,
	}

	rootCmd.AddCommand(newStatusCmd())
	rootCmd.AddCommand(newCheckCmd())
	rootCmd.AddCommand(newRepairCmd())
	rootCmd.AddCommand(newSyncCmd())
	rootCmd.AddCommand(newBackfillCmd())
	rootCmd.AddCommand(newPruneCmd())
	rootCmd.AddCommand(newQueryCmd())
	return rootCmd
}

// execute はコマンドを 1 回走らせ、終了コードを返す（試験から呼べるよう main と分ける）。
func execute(args []string) int {
	run = nil
	rootCmd := newRootCmd()
	rootCmd.SetArgs(args)
	// panic も記録・通知してから終わる（cli.Guarded）。失敗した回はダイジェストでも error にする
	// （os.Exit は defer を飛ばすので、Finish はここで必ず呼ぶ）
	panicked, err := cli.Guarded("jquants", &run, rootCmd.Execute)
	run.Finish(err)
	if panicked {
		return cli.ExitPanic
	}
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}
