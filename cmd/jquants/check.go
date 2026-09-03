package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/spf13/cobra"
)

func newCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "直近営業日のデータが揃っているか確認する",
		RunE: func(cmd *cobra.Command, args []string) error {
			arch, err := archive.OpenArchive(jquantsDir)
			if err != nil {
				return err
			}
			defer arch.Close()

			fmt.Println("直近データの整合性を確認中...")
			allGood := true
			for _, ep := range archive.StandardEndpoints {
				_, latest, count, _ := arch.EndpointStatus(ep.Name)
				if count == 0 {
					fmt.Printf("[未取得] %s\n", ep.Name)
					allGood = false
				} else {
					fmt.Printf("[正常] %s: 最新 %s (%d件)\n", ep.Name, latest, count)
				}
			}
			if !allGood {
				fmt.Println("\n未取得の端点があります。'jquants sync' または 'jquants backfill' を実行してください。")
			}
			return nil
		},
	}
}
