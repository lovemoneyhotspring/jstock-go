package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/spf13/cobra"
)

func newCredentialsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "APIキーの管理",
	}
	cmd.AddCommand(newCredentialsCheckCmd())
	return cmd
}

func newCredentialsCheckCmd() *cobra.Command {
	var env string

	cmd := &cobra.Command{
		Use:   "check",
		Short: "立花証券の認証情報が解決できるか確認する（秘密は表示しない）",
		Long: "立花証券の認証情報（認証ID・秘密鍵・第二暗証番号）が解決できるか確認する。秘密は表示しない。\n\n" +
			"置き場所は環境変数 → .env（0600）→ OS キーチェーンの順（docs/DEPLOY.md「立花証券 e支店」）。",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := settings.Environment(env)
			if target != settings.EnvUAT && target != settings.EnvProd {
				return fmt.Errorf("環境は uat か prod で指定してください: %s", env)
			}

			creds, err := credentials.LoadTachibanaCredentials(target, appSettings.DotenvMap)
			if err != nil {
				return err
			}
			fmt.Printf("%s: 認証情報を解決できました\n", target)
			fmt.Printf("  %s\n", creds)
			fmt.Printf("  取得元: %s\n", credentials.TachibanaCredentialSource(target, appSettings.DotenvMap))
			return nil
		},
	}

	cmd.Flags().StringVar(&env, "env", string(settings.EnvUAT), "対象環境（uat / prod）")
	return cmd
}
