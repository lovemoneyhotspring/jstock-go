package main

import (
	"fmt"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "今日の候補と台帳の注文を表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			day, err := dayOrToday(dateFlag, clock.NowUTC())
			if err != nil {
				return err
			}
			state := "有効"
			if !cfg.Capital.Enabled {
				state = "無効（capital.enabled = false）"
			}
			suffix := ""
			if cfg.Capital.Positions() == 0 {
				suffix = "（資金 0: 買わない）"
			}
			fmt.Printf("%s: %s  資金 %s 円 → N=%d、%s%s\n",
				cfg.StrategyName(), state, yen(cfg.Capital.MaxCapital),
				cfg.Capital.Positions(), execute.LongBudgetText(cfg.Capital, cfg.Capital.Positions(), cfg.Capital.BudgetPerOrder()), suffix)

			p, ok, err := dtplan.Load(appSettings.DaytradeDir(), day)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Printf("%s の候補がありません（daytrade plan を実行）\n", day.Format(DateLayout))
			} else {
				printPlan(p, cfg)
			}

			led, err := dtledger.Open(appSettings.DaytradeDBPath())
			if err != nil {
				return err
			}
			defer led.Close()
			orders, err := led.OrdersOn(day, nil)
			if err != nil {
				return err
			}
			if len(orders) == 0 {
				fmt.Println("今日の注文はありません")
				printPositions(cfg, day, orders)
				return nil
			}
			fmt.Printf("\n%s の注文\n", day.Format(DateLayout))
			fmt.Printf("  %-25s %-6s %-4s %10s %10s %10s %12s %s\n",
				"時刻", "銘柄", "売買", "株数", "約定", "価格", "約定単価", "状態")
			verifying, unqueried := 0, 0
			for _, o := range orders {
				// 実機検証の注文は成績に数えないので、一覧でも見分けが付くようにする
				status := o.Status
				if o.Verify {
					status += "（検証）"
					verifying++
				}
				// 台帳の約定はブローカーに照会したときだけ書き換わる（close / verify / 翌朝の持ち越し）。
				// 未確定の行の「約定 0」は約定していないという意味ではない
				if o.IsOpen() {
					status += "（未照会）"
					unqueried++
				}
				fmt.Printf("  %-25s %-6s %-4s %10s %10s %10s %12s %s\n",
					clock.FmtISO(o.PlacedAt, clock.MustZone(appSettings.Timezone)),
					o.Symbol, string(o.Side), yen(o.Quantity), yen(o.FilledQuantity),
					yenPtr(o.Price), yenPtr(o.AvgFillPrice), status)
			}
			if unqueried > 0 {
				fmt.Printf("\n  うち %d 件は未照会。台帳の約定は 15:20 の close（と引け後の verify）でブローカーに照会して書き込むので、\n"+
					"  それまでは約定していても「約定 0」と表示されます\n", unqueried)
			}
			if verifying > 0 {
				fmt.Printf("\n  うち %d 件は実機検証（--broker-verify）。成績の集計と資産曲線のゲートからは外れます\n",
					verifying)
			}
			printPositions(cfg, day, orders)
			return nil
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

// printPositions はブローカーに信用建玉を照会して、実際に建った値段を出す。
//
// 台帳の約定単価は 15:20 の close まで書き込まれない（それまで「約定 0（未照会）」）。
// 場中に「いくらで建ったか」を見る手段がここに無いと、寄付の成行が判断時の気配から
// どれだけ離れたかを当日のうちに確かめられない。2026-09-18 の寄付は 4 銘柄とも
// 始値より 65〜135 bp 高く約定していた（成行が売り板を上から食い上がるぶん）。
//
// 照会できなくても黙って戻る——status は台帳を見るのが主で、建玉は付け足し。
// 通信できない場所で台帳だけ確かめたいときに、ここで落ちては困る。
func printPositions(cfg dtconfig.Config, day time.Time, orders []dtledger.Order) {
	// ブローカーが返すのは今の建玉だけ。過去日を指定した status で照会しても当日の玉が出る
	today, err := dayOrToday("", clock.NowUTC())
	if err != nil || !day.Equal(today) {
		return
	}
	b, err := connectBroker(cfg)
	if err != nil {
		fmt.Printf("\n建玉を照会できません: %v\n", err)
		return
	}
	legs := broker.PositionsByLeg(b)
	if err := legs.Err(true); err != nil {
		fmt.Printf("\n信用建玉を照会できません: %v\n", err)
		return
	}

	decided := decidedPrices(orders)
	var pnlTotal, slipYen, refYen decimal.Decimal
	rows := 0
	for _, leg := range legs.Legs() {
		if !leg.Margin {
			continue // 現物は積立の保有。デイトレの建玉ではない
		}
		pos, ok := legs.At(leg)
		if !ok || pos.Quantity.IsZero() {
			continue
		}
		if rows == 0 {
			fmt.Printf("\n信用建玉（ブローカー照会）\n")
			fmt.Printf("  %-6s %-6s %8s %12s %12s %12s %12s %12s\n",
				"銘柄", "脚", "株数", "建値", "現在値", "評価損益", "判断時", "ずれ")
		}
		rows++

		side := "買建"
		if leg.Short {
			side = "売建"
		}
		pnl := pos.LastPrice.Sub(pos.CostPrice).Mul(pos.Quantity)
		if leg.Short {
			pnl = pnl.Neg()
		}
		pnlTotal = pnlTotal.Add(pnl)

		refStr, gapStr := "—", "—"
		if ref, has := decided[leg]; has && ref.IsPositive() {
			// 不利なら正。買建は高く買ったぶん、売建は安く売ったぶんが損になる
			diff := pos.CostPrice.Sub(ref)
			if leg.Short {
				diff = diff.Neg()
			}
			refStr = priceStr(ref)
			gapStr = fmt.Sprintf("%+.1f bp", diff.Div(ref).Mul(decimal.NewFromInt(10000)).InexactFloat64())
			slipYen = slipYen.Add(diff.Mul(pos.Quantity))
			refYen = refYen.Add(ref.Mul(pos.Quantity))
		}
		fmt.Printf("  %-6s %-6s %8s %12s %12s %12s %12s %12s\n",
			leg.Symbol, side, yen(pos.Quantity), priceStr(pos.CostPrice), priceStr(pos.LastPrice),
			yen(pnl), refStr, gapStr)
	}
	if rows == 0 {
		fmt.Println("\n信用建玉はありません")
		return
	}
	fmt.Printf("\n  評価損益 合計 %s 円\n", yen(pnlTotal))
	if refYen.IsPositive() {
		bp := slipYen.Div(refYen).Mul(decimal.NewFromInt(10000))
		fmt.Printf("  執行のずれ 合計 %s 円（金額加重 %+.1f bp）。判断時（9:00 の気配）と実際の建値の差で、\n"+
			"  成行が板を食い上がるぶんです。台帳の約定単価は 15:20 の close まで入りません\n",
			yen(slipYen.Neg()), bp.InexactFloat64())
	}
}

// decidedPrices は台帳の新規注文から、脚ごとの判断時の価格（株数の加重平均）を作る。
//
// 返済の注文は建値の比べに使わない——手仕舞いの値段なので、建てた値段とは別物。
func decidedPrices(orders []dtledger.Order) map[broker.PositionLeg]decimal.Decimal {
	sum := map[broker.PositionLeg]decimal.Decimal{}
	qty := map[broker.PositionLeg]decimal.Decimal{}
	for _, o := range orders {
		if o.IsDryRun() || o.Price == nil || o.Trade != domain.TradeTypeMarginOpen {
			continue
		}
		leg := broker.LegOf(o.Symbol, o.Trade, o.Side == domain.SideSell)
		sum[leg] = sum[leg].Add(o.Price.Mul(o.Quantity))
		qty[leg] = qty[leg].Add(o.Quantity)
	}
	out := make(map[broker.PositionLeg]decimal.Decimal, len(qty))
	for leg, q := range qty {
		if q.IsPositive() {
			out[leg] = sum[leg].Div(q)
		}
	}
	return out
}

// priceStr は値段の整形。yen は円未満を丸めるので、建玉の加重平均（2,868.17）が潰れる。
func priceStr(v decimal.Decimal) string { return cli.AddCommas(v.Round(2).String()) }
