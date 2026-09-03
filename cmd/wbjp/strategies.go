package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newStrategiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "strategies",
		Short: "使える戦略（ストラテジー）の一覧",
		Run: func(cmd *cobra.Command, args []string) {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "名前\t説明")
			fmt.Fprintln(w, "sma_cross\t移動平均クロス（順張りトレンドフォロー）")
			fmt.Fprintln(w, "rsi_reversion\tRSI 逆張り（レンジ相場・買われすぎ売られすぎ反転）")
			fmt.Fprintln(w, "atr_breakout\tドンチャンチャネル上抜け/下抜け ＋ ATR ボラティリティフィルタ")
			fmt.Fprintln(w, "trend_pullback\t長期上昇トレンド ＋ 出来高枯渇押し目 ＋ 反発ブレイクアウト")
			fmt.Fprintln(w, "rsi_pullback\t長期上昇トレンド ＋ RSI(3) 短期売られすぎ反発（勝率重視）")
			fmt.Fprintln(w, "momentum_rank\t過去6ヶ月・12ヶ月の中期モメンタム順位（損益比重視）")
			w.Flush()
		},
	}
}
