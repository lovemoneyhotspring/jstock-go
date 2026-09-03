package main

import (
	"fmt"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "設定銘柄（日本株および判定用指数）の最新日足を同期する",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}

			runID := fmt.Sprintf("accum-sync-%d", time.Now().Unix())
			logger, _ := logging.NewLogger("accum", string(appSettings.Env), runID, "sync", appSettings.LogDir)
			defer logger.Close()

			barStore := data.NewBarStore(appSettings.BarsDir())
			fredClient := data.NewFREDProvider(15 * time.Second)

			var jqClient *data.JQuantsClient
			if apiKey, err := credentials.LoadAPIKey("WBJP_JQUANTS_API_KEY", appSettings.DotenvMap); err == nil && apiKey != "" {
				jqClient = data.NewJQuantsClient(apiKey)
			}

			// 同期対象銘柄を収集
			targets := make(map[string]struct{})
			for _, t := range cfg.Tactics {
				if !t.IsEnabled() {
					continue
				}
				for _, s := range t.Symbols {
					targets[s] = struct{}{}
				}
				if t.SignalSymbol != "" {
					targets[t.SignalSymbol] = struct{}{}
				}
			}

			fmt.Printf("日足データを同期中（全 %d 銘柄）...\n", len(targets))
			for sym := range targets {
				if err := data.SyncSymbolBars(sym, barStore, jqClient, fredClient, logger); err != nil {
					fmt.Printf("[エラー] %s: %v\n", sym, err)
				} else {
					fmt.Printf("[完了] %s\n", sym)
				}
			}
			return nil
		},
	}
}
