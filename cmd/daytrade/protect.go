package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/spf13/cobra"
)

func newProtectCmd() *cobra.Command {
	var liveFlag, yesFlag, ignoreWindowFlag bool
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "protect",
		Short: "場中: 建てた玉に保険の手仕舞い（執行条件「引け」）をブローカーへ置く（execution.protect_exit）",
		Long: "建てた玉のうち約定したぶんに、執行条件「引け」の返済・売りを先にブローカーへ置く。注文は立花に残るので、\n" +
			"cron・マシン・ネットが止まっても引けで手仕舞われる（人が気づく前提にしない安全網）。\n" +
			"ふだんの手仕舞いは 15:20 の成行で、close が保険の注文を取り消してから出す（引け値は成行より不利）。\n" +
			"execution.protect_exit = false（既定）なら何もしない。何度回しても重ならない。既定は dry-run。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return crash("保険の引け注文", "daytrade.crash", runProtect(liveFlag, yesFlag, ignoreWindowFlag, dateFlag))
		},
	}
	cmd.Flags().BoolVar(&liveFlag, "live", false, "注文を送る。無ければ何もしない")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番の確認を省く（cron 用）")
	cmd.Flags().BoolVar(&ignoreWindowFlag, "ignore-window", false, "時間帯の外でも送る（後場寄り 12:30 より前は付けても送らない。前引けで約定するため）")
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

func runProtect(live, yes, ignoreWindow bool, date string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fmt.Println(appSettings.DescribeMode(live, cfg.Execution.KillSwitch))
	now := clock.NowUTC()
	day, err := dayOrToday(date, now)
	if err != nil {
		return err
	}
	if skipHolidayFor(day, "protect", live) {
		return nil
	}
	if !cfg.Execution.ProtectExit {
		fmt.Println("execution.protect_exit が無効。何もしません")
		logInfo("daytrade.skip", "保険の引け注文は無効", map[string]any{"reason": "disabled", "phase": "protect"})
		digest.Skipped("disabled")
		return nil
	}
	// 後場（12:30〜15:20 の close の前）だけ。前場に置いた「引け」は前引けで約定し、close の後に置くと
	// close が出した成行と重なる
	if live && !ignoreWindow && !cfg.Execution.InWindow("protect", now, jst) {
		fmt.Printf("保険を置く時間帯の外（%s）。何もしません\n", describeWindow(cfg, "protect"))
		logInfo("daytrade.skip", "保険を置く時間帯の外",
			map[string]any{"reason": "window", "phase": "protect", "window": describeWindow(cfg, "protect")})
		digest.Skipped("window")
		return nil
	}
	// --ignore-window でも後場寄りより前には置かない（前引けで約定し、建玉が昼に手仕舞われる）
	if local := now.In(jst); live && local.Before(time.Date(local.Year(), local.Month(), local.Day(),
		dtconfig.ProtectEarliestHour, dtconfig.ProtectEarliestMinute, 0, 0, jst)) {
		fmt.Printf("後場寄り（%02d:%02d）より前。前場に置いた「引け」は前引けで約定するので置きません\n",
			dtconfig.ProtectEarliestHour, dtconfig.ProtectEarliestMinute)
		logInfo("daytrade.skip", "保険を置く時間帯の外（前場）",
			map[string]any{"reason": "morning", "phase": "protect", "window": describeWindow(cfg, "protect")})
		digest.Skipped("window")
		return nil
	}
	allowed, reason := appSettings.CanExecuteLive(live, cfg.Execution.KillSwitch)
	started := now
	deadline := cfg.Execution.RunDeadline("protect", now, live && !ignoreWindow, jst)
	logConfig(cfg, "protect", map[string]any{
		"day": day.Format(DateLayout), "live": live,
		"deadline": deadlineText(deadline), "max_run_seconds": cfg.Execution.MaxRunSeconds,
	})

	led, err := dtledger.Open(appSettings.DaytradeDBPath())
	if err != nil {
		return err
	}
	defer led.Close()
	defer flushExecution(day)
	defer flushOnSignal(day)()
	if live {
		run.DeferAlerts()
	}
	env := execute.Env{
		Cfg: cfg, Ledger: led, Day: day, Report: run, Out: os.Stdout,
		RetryWait: execute.DefaultRetryWait, Deadline: deadline,
	}

	// 建玉が無ければブローカーに触らない（間隔をおいて数回回るので、ログインを増やさない）
	entries, _, err := execute.LiveEntries(env)
	if err != nil {
		return err
	}
	live0 := 0
	for _, o := range entries {
		if !o.IsDead() {
			live0++
		}
	}
	if live0 == 0 {
		fmt.Println("今日の建玉はありません")
		logInfo("daytrade.skip", "保険の対象なし", map[string]any{"reason": "no_entries", "phase": "protect"})
		return nil
	}
	if !allowed {
		fmt.Printf("dry-run: 今日の建玉 %d 件に保険の引け注文を置く（--live で送る）\n", live0)
		return nil
	}

	b, err := connectBroker(cfg)
	if err != nil {
		return err
	}
	broker.SetDeadline(b, deadline)
	if err := resolvePending(env, b); err != nil {
		logError("daytrade.pending_unresolved", "送信結果不明の注文を判定できず保険を続ける",
			map[string]any{"error": err.Error()})
	}
	if err := confirmLive(allowed, yes); err != nil {
		return err
	}
	actions, err := execute.ProtectEntries(env, b)
	if err != nil {
		return err
	}
	var failures []string
	placed := 0
	for _, a := range actions {
		if a.Err != nil {
			failures = append(failures, fmt.Sprintf("%s（%s 株）: %v", a.Symbol, a.Quantity, a.Err))
		} else if a.Outcome == "発注" {
			placed++
		}
	}
	if len(failures) > 0 {
		// 置けなくても売買は止まらない（close が 15:20 に従来どおり手仕舞う）。ただし安全網が
		// 効いていない——実機で通らない条件なら、その日の分を人が見直す材料になる
		alert(fmt.Sprintf("デイトレ: 保険の引け注文 %d 件が通らず（15:20 の手仕舞いは通常どおり）", len(failures)),
			strings.Join(failures, "\n"))
		digest.Anomaly("daytrade.protect_failed", fmt.Sprintf("%d 件の保険の引け注文が通らず", len(failures)))
	}
	logInfo("daytrade.run", "保険の引け注文を終了", map[string]any{
		"phase": "protect", "live": allowed, "reason": reason, "placed": placed, "failures": len(failures),
		"elapsed_ms": clock.NowUTC().Sub(started).Milliseconds(), "deadline": deadlineText(deadline),
	})
	digest.Note(map[string]any{"phase": "protect", "live": allowed, "placed": placed, "failures": len(failures)})
	return nil
}
