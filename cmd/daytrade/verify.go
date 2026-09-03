package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/spf13/cobra"
)

func newVerifyCmd() *cobra.Command {
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "引け後: 手仕舞いが全部約定したかを照会し、未約定（持ち越し）なら通知する",
		Long: "引け後: 今日の手仕舞いが全部約定したかをブローカーに照会し、未約定（持ち越し）なら\n" +
			"通知する。ロング（買い→売り）もショート（売建→返済買い）も脚ごとに突き合わせる。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return crash("手仕舞いの検証", "daytrade.crash", runVerify(dateFlag))
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

func runVerify(date string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	day, err := dayOrToday(date, clock.NowUTC())
	if err != nil {
		return err
	}
	logConfig(cfg, "verify", map[string]any{"day": day.Format(DateLayout)})

	led, err := dtledger.Open(appSettings.DaytradeDBPath())
	if err != nil {
		return err
	}
	defer led.Close()
	defer func() {
		if err := execution.Flush(historyStore(), day); err != nil {
			logWarn("daytrade.execution", "実行品質の記録に失敗", map[string]any{"error": err.Error()})
		}
	}()

	env := execute.Env{Cfg: cfg, Ledger: led, Day: day, Report: run, Out: os.Stdout}
	entries, _, err := execute.LiveOrders(env)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("今日の本発注はありません")
		logInfo("daytrade.skip", "検証の対象なし", map[string]any{"reason": "no_buys", "phase": "verify"})
		return nil
	}

	b, err := connectBroker(cfg)
	if err != nil {
		return err
	}
	if err := resolvePending(env, b); err != nil {
		return err
	}
	// 判定で台帳が変わりうるので読み直す
	var exits []dtledger.Order
	if entries, exits, err = execute.LiveOrders(env); err != nil {
		return err
	}

	// 照会できなかった注文は数えておく。verify の役目は持ち越しの検出なので、
	// 「確かめられなかった」を「持ち越し無し」として通すと役目を果たさない
	result := execute.Verify(env, b, entries, exits)
	carried, unconfirmed := result.Carried, result.Unconfirmed

	if len(carried) > 0 {
		logError("daytrade.carry", "持ち越し",
			map[string]any{"day": day.Format(DateLayout), "positions": carried})
		digest.Anomaly("daytrade.carry", fmt.Sprintf("%d 銘柄が未返済のまま", len(carried)))
		alert("デイトレ: 売れ残りがあります（持ち越し）。翌朝に手で売ってください",
			strings.Join(carried, "\n"))
		return nil
	}

	// 照会できなかった注文があるなら「持ち越しなし」とは言えない。
	// ここで正常終了すると、確かめられなかっただけの日と本当に無事な日が
	// ログの上で区別できなくなる。
	if len(unconfirmed) > 0 {
		logError("daytrade.unconfirmed", "照会できず持ち越しを判定できません",
			map[string]any{"day": day.Format(DateLayout), "orders": unconfirmed})
		digest.Anomaly("daytrade.unconfirmed",
			fmt.Sprintf("%d 件を照会できず持ち越しを判定できません", len(unconfirmed)))
		alert("デイトレ: 注文を照会できず持ち越しを判定できません。口座を確認してください",
			strings.Join(unconfirmed, "\n"))
		return fmt.Errorf("%d 件の注文を照会できませんでした", len(unconfirmed))
	}

	fmt.Println("手仕舞いを確認しました（持ち越しなし）")
	logInfo("daytrade.run", "手仕舞いを確認",
		map[string]any{"phase": "verify", "live": true, "carried": 0})
	return nil
}
