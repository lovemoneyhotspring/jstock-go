package main

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/spf13/cobra"
)

// defaultSyncDays は既定の取得期間。指標のウォームアップと 20 営業日後の
// 事後検証に足りる長さ。
const defaultSyncDays = 900

// runSyncDays は run が判断の直前に足を更新するときの期間。
// 長い履歴は保存済みなので、直近ぶんだけ取り直せば足りる。
const runSyncDays = 30

// newSyncCmd はトップレベルの sync。`wbjp data sync` と同じものを指す。
func newSyncCmd() *cobra.Command {
	cmd := newDataSyncCmd()
	cmd.Short = "ユニバース銘柄の最新日足を J-Quants から同期する"
	return cmd
}

func newDataSyncCmd() *cobra.Command {
	var days int
	var force bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "日足を更新する（保存済みの続きだけ取得）",
		Long: "日足を更新する。既に保存済みの銘柄は「最終日以降」しか取りに行かない。\n" +
			"あとから期間を伸ばしたいときは --force を付けて取り直す。",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}
			if days < 1 {
				return fmt.Errorf("--days は 1 以上で指定してください: %d", days)
			}

			logger := run.Logger

			failures := syncUniverseBars(setCfg, logger, days, force, true)
			if failures > 0 {
				// cron の中で黙って落ちると、足が止まっていることに気づくのが遅れる
				notify.Alert("足の取り込みに失敗",
					fmt.Sprintf("%s: %d 銘柄", appSettings.ConfigDir, failures), logger)
				return fmt.Errorf("%d 銘柄の同期に失敗しました", failures)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", defaultSyncDays, "何日ぶん遡って取得するか")
	cmd.Flags().BoolVar(&force, "force", false, "保存済みを無視して取り直す")
	return cmd
}

// syncSymbols は同期する銘柄。ユニバースに加え、地合い判定の指数と
// 待機資金の利回り系列も取る——これらが欠けると regime が「判断できない日」に
// なり、バックテストでは利息が付かなくなる。
func syncSymbols(setCfg *wbjpcfg.SettingsFile) []string {
	symbols := append([]string(nil), setCfg.Universe.Symbols...)
	if !setCfg.Regime.Enabled {
		return symbols
	}
	seen := make(map[string]struct{}, len(symbols))
	for _, s := range symbols {
		seen[s] = struct{}{}
	}
	for _, extra := range []string{setCfg.Regime.Benchmark, setCfg.Regime.CashYieldSymbol} {
		if extra == "" {
			continue
		}
		if _, ok := seen[extra]; ok {
			continue
		}
		seen[extra] = struct{}{}
		symbols = append(symbols, extra)
	}
	return symbols
}

// syncUniverseBars は日足を取り直して BarStore に保存し、失敗した銘柄数を返す。
//
// verbose は銘柄ごとの結果を標準出力に出すかどうか。run から呼ぶときは
// 落としておき、判断そのものの出力を埋もれさせない。
// 個々の銘柄の失敗では止めない——1 銘柄が取れないだけで当日の判断を
// 丸ごと諦める理由はなく、古い足のままなら後段の検査で分かる。
func syncUniverseBars(
	setCfg *wbjpcfg.SettingsFile,
	logger *logging.Logger,
	days int,
	force bool,
	verbose bool,
) int {
	barStore := data.NewBarStore(appSettings.BarsDir())
	fredClient := data.NewFREDProvider(15 * time.Second)

	var jqClient *data.JQuantsClient
	if apiKey, err := credentials.LoadAPIKey("WBJP_JQUANTS_API_KEY", appSettings.DotenvMap); err == nil && apiKey != "" {
		jqClient = data.NewJQuantsClient(apiKey)
	}

	since := clock.TodayUTC().AddDate(0, 0, -days).Format("2006-01-02")
	symbols := syncSymbols(setCfg)
	if verbose {
		fmt.Printf("ユニバース銘柄の日足を同期中（全 %d 銘柄、%s 以降）...\n", len(symbols), since)
	}

	failures := 0
	for _, sym := range symbols {
		err := data.SyncSymbolBarsSince(sym, barStore, jqClient, fredClient, logger, since, force)
		if err != nil {
			failures++
			if verbose {
				fmt.Printf("[エラー] %s: %v\n", sym, err)
			} else {
				logger.Warn("sync.failed", fmt.Sprintf("%s: %v", sym, err))
			}
			continue
		}
		if verbose {
			fmt.Printf("[完了] %s\n", sym)
		}
	}
	if failures > 0 && !verbose {
		fmt.Printf("足の更新に %d 銘柄で失敗しました（保存済みの足で続けます）\n", failures)
	}
	return failures
}
