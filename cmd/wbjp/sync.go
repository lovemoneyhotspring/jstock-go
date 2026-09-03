package main

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "ユニバース銘柄の最新日足を J-Quants から同期する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}

			runID := fmt.Sprintf("wbjp-sync-%d", time.Now().Unix())
			logger, _ := logging.NewLogger("wbjp", string(appSettings.Env), runID, "sync", appSettings.LogDir)
			defer logger.Close()

			barStore := data.NewBarStore(appSettings.BarsDir())
			fredClient := data.NewFREDProvider(15 * time.Second)

			var jqClient *data.JQuantsClient
			if apiKey, err := credentials.LoadAPIKey("WBJP_JQUANTS_API_KEY", appSettings.DotenvMap); err == nil && apiKey != "" {
				jqClient = data.NewJQuantsClient(apiKey)
			}

			fmt.Printf("ユニバース銘柄の日足を同期中（全 %d 銘柄）...\n", len(setCfg.Universe.Symbols))
			for _, sym := range setCfg.Universe.Symbols {
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
