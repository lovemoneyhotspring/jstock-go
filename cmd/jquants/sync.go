package main

import (
	"fmt"
	"os"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "J-Quants から必要な端点の最新日次データを取り込む",
		RunE: func(cmd *cobra.Command, args []string) error {
			arch, err := archive.OpenArchive(jquantsDir)
			if err != nil {
				return err
			}
			defer arch.Close()

			apiKey := os.Getenv("WBJP_JQUANTS_API_KEY")
			if apiKey == "" {
				if v, ok := appSettings.DotenvMap["WBJP_JQUANTS_API_KEY"]; ok {
					apiKey = v
				}
			}
			if apiKey == "" {
				return fmt.Errorf("J-Quants APIキーが設定されていません（WBJP_JQUANTS_API_KEY）")
			}

			client := data.NewJQuantsClient(apiKey)
			fmt.Println("J-Quants 端点の差分同期を開始します...")

			for _, ep := range archive.StandardEndpoints {
				fmt.Printf("[%s] 同期中 (%s)...\n", ep.Name, ep.Path)
				resp, err := client.Get(ep.Path, nil)
				if err != nil {
					fmt.Printf("  エラー: %v\n", err)
					continue
				}

				rec := archive.IngestRecord{
					Endpoint: ep.Name,
					Target:   time.Now().Format("2006-01-02"),
					Source:   "daily",
					Rows:     len(resp.Data),
					Changed:  len(resp.Data),
					Digest:   "-",
				}
				_ = arch.RecordIngest(rec)
				fmt.Printf("  完了: %d 件取得\n", len(resp.Data))
			}
			return nil
		},
	}
}
