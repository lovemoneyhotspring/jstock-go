package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/spf13/cobra"
)

func newGuardCmd() *cobra.Command {
	var liveFlag, yesFlag, ignoreWindowFlag bool
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "場中: TOB などの材料が出た今日の売建を取消・返済する（既定は dry-run）",
		Long: "場中: ニュースの記録簿で TOB・MBO など価格の行き先が決まる材料が出た銘柄を調べ、今日の売建を処置する。\n" +
			"未約定なら取消、一部約定なら残りを取消して約定分を成行で返済買い、全部約定なら返済買い。\n" +
			"open が外せなかった材料（8:52 の取り込みの後や場中の公表）で張り付く前に手仕舞うため。既定は dry-run。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return crash("材料の出た売建の処置", "daytrade.crash", runGuard(liveFlag, yesFlag, ignoreWindowFlag, dateFlag))
		},
	}
	cmd.Flags().BoolVar(&liveFlag, "live", false, "取消・返済を送る。無ければ対象を示すだけ")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番の確認を省く（cron 用）")
	cmd.Flags().BoolVar(&ignoreWindowFlag, "ignore-window", false, "時間帯の外でも送る")
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

func runGuard(live, yes, ignoreWindow bool, date string) error {
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
	if skipHolidayFor(day, "guard", live) {
		return nil
	}
	if !cfg.Margin.Enabled || !cfg.Margin.CancelOnCorpEvent {
		fmt.Println("信用売りの脚か margin.cancel_on_corp_event が無効。何もしません")
		logInfo("daytrade.skip", "材料の点検は無効", map[string]any{"reason": "disabled", "phase": "guard"})
		return nil
	}
	if live && !ignoreWindow && !cfg.Execution.InWindow("guard", now, jst) {
		fmt.Printf("材料の点検の時間帯の外（%s）。何もしません\n", describeWindow(cfg, "guard"))
		logInfo("daytrade.skip", "材料の点検の時間帯の外",
			map[string]any{"reason": "window", "phase": "guard", "window": describeWindow(cfg, "guard")})
		digest.Skipped("window")
		return nil
	}
	allowed, reason := appSettings.CanExecuteLive(live, cfg.Execution.KillSwitch)
	started := now
	deadline := cfg.Execution.RunDeadline("guard", now, live && !ignoreWindow, jst)
	logConfig(cfg, "guard", map[string]any{
		"day": day.Format(DateLayout), "live": live,
		"deadline": deadlineText(deadline), "max_run_seconds": cfg.Execution.MaxRunSeconds,
	})

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
	env := execute.Env{
		Cfg: cfg, Ledger: led, Day: day, Report: run, Out: os.Stdout,
		RetryWait: execute.DefaultRetryWait, Deadline: deadline,
	}

	// 売建が無ければブローカーにも記録簿にも触らない（10 分おきに回るので、ログインを増やさない）
	entries, _, err := execute.LiveEntries(env)
	if err != nil {
		return err
	}
	shorts := map[string]bool{}
	for _, o := range entries {
		if execute.IsShortEntry(o) && !o.IsDead() {
			shorts[o.Symbol] = true
		}
	}
	if len(shorts) == 0 {
		fmt.Println("今日の売建はありません")
		logInfo("daytrade.skip", "材料の点検の対象なし", map[string]any{"reason": "no_shorts", "phase": "guard"})
		return nil
	}

	ev, err := loadCorpEvents(context.Background(), cfg.Margin, day, now)
	if err != nil {
		logError("daytrade.news_stale", "ニュースの記録簿を読めず売建の材料を点検できない", map[string]any{"error": err.Error()})
		digest.Anomaly("daytrade.news_stale", "売建の材料を点検できない: "+err.Error())
		alert("デイトレ: ニュースの記録簿を読めず、売建の材料（TOB など）を点検できません", err.Error())
		return err
	}
	if age := now.Sub(ev.lastFetched); ev.lastFetched.IsZero() ||
		age > time.Duration(cfg.Margin.CorpEventMaxStalenessMinutes)*time.Minute {
		// 古くても手元の分では点検する。取り込み（news sync）が止まっていることは知らせる
		detail := "取り込みに成功した日が 1 日も無い"
		if !ev.lastFetched.IsZero() {
			detail = fmt.Sprintf("最後の取り込みが %s（%d 分前）", clock.ToZone(ev.lastFetched, jst).Format("01-02 15:04"), int(age.Minutes()))
		}
		fmt.Println("ニュースの記録簿が古いまま点検します: " + detail)
		logWarn("daytrade.news_stale", "ニュースの記録簿が古いまま売建を点検", map[string]any{"reason": detail, "phase": "guard"})
		digest.Anomaly("daytrade.news_stale", "売建の点検の記録簿が古い: "+detail)
	}

	marks := map[string]string{}
	for symbol := range shorts {
		m, ok := ev.marks[symbol]
		if !ok {
			continue
		}
		marks[symbol] = m.Kind
		fmt.Printf("材料の出た売建: %s（%s）%s %s\n", symbol, m.Kind, m.At, m.Headline)
		logInfo("daytrade.corp_guard", "材料の出た売建", map[string]any{
			"symbol": symbol, "kind": m.Kind, "at": m.At, "headline": m.Headline,
		})
	}
	if len(marks) == 0 {
		fmt.Printf("材料の出た売建はありません（%d 銘柄を点検）\n", len(shorts))
		logInfo("daytrade.run", "材料の点検を終了", map[string]any{
			"phase": "guard", "live": allowed, "shorts": len(shorts), "marked": 0,
			"elapsed_ms": clock.NowUTC().Sub(started).Milliseconds(),
		})
		return nil
	}
	// 取消・返済の済んだ売建だけなら接続しない（10 分ごとにログインしない）
	pending, err := execute.GuardPending(env, marks)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Printf("材料の出た売建 %d 銘柄は処置済み（取消・返済が済んでいるか返済が生きている）\n", len(marks))
		logInfo("daytrade.run", "材料の点検を終了", map[string]any{
			"phase": "guard", "live": allowed, "shorts": len(shorts), "marked": len(marks), "pending": 0,
			"elapsed_ms": clock.NowUTC().Sub(started).Milliseconds(),
		})
		return nil
	}

	var b broker.Broker
	if allowed {
		if b, err = connectBroker(cfg); err != nil {
			alert("デイトレ: 材料（TOB など）の出た売建があるのに証券会社に接続できません。口座を確認してください",
				strings.Join(sortedKeys(marks), "、")+": "+err.Error())
			return err
		}
		broker.SetDeadline(b, deadline)
		// 送信結果の分からない注文を先に判定する。判定できなくても処置は続ける——判定できなかった
		// 注文は照会で「分からない」になり、取消も返済もしない（GuardCorpEvents）
		if err := resolvePending(env, b); err != nil {
			logError("daytrade.pending_unresolved", "送信結果不明の注文を判定できず材料の処置を続ける",
				map[string]any{"error": err.Error()})
		}
	}
	if err := confirmLive(allowed, yes); err != nil {
		return err
	}

	actions, err := execute.GuardCorpEvents(env, b, marks)
	if err != nil {
		return err
	}
	var acted, failed []string
	cancelled, returned := 0, 0
	for _, a := range actions {
		m := ev.marks[a.Symbol]
		line := fmt.Sprintf("%s（%s %s）売建 %s 株: %s", a.Symbol, a.Kind, m.At, a.Quantity, a.Result)
		fields := map[string]any{
			"symbol": a.Symbol, "client_order_id": a.ClientOrderID, "kind": a.Kind, "headline": m.Headline,
			"quantity": a.Quantity.String(), "filled": a.Filled.String(),
			"cancelled": a.Cancelled, "returned": a.Returned.String(), "result": a.Result,
		}
		if a.Err != nil {
			line = fmt.Sprintf("%s（%s %s）売建 %s 株（約定 %s）: %v", a.Symbol, a.Kind, m.At, a.Quantity, a.Filled, a.Err)
			fields["error"] = a.Err.Error()
			failed = append(failed, line)
			logError("daytrade.corp_guard", "材料の出た売建を処置できない", fields)
		} else {
			logWarn("daytrade.corp_guard", "材料の出た売建を処置", fields)
		}
		fmt.Println("  " + line)
		if a.Cancelled {
			cancelled++
		}
		if a.Returned.IsPositive() {
			returned++
		}
		if a.Acted() && a.Err == nil {
			acted = append(acted, line+"\n  "+m.Headline)
		}
	}
	if allowed && len(acted) > 0 {
		alert(fmt.Sprintf("デイトレ: 材料（TOB など）の出た売建 %d 件を取消・返済しました", len(acted)),
			strings.Join(acted, "\n"))
	}
	if len(failed) > 0 {
		digest.Anomaly("daytrade.corp_guard_failed", fmt.Sprintf("材料の出た売建 %d 件を処置できず", len(failed)))
		if allowed {
			alert("デイトレ: 材料（TOB など）の出た売建を取消・返済できません。口座を確認してください",
				strings.Join(failed, "\n"))
		}
	}
	logInfo("daytrade.run", "材料の点検を終了", map[string]any{
		"phase": "guard", "live": allowed, "reason": reason,
		"shorts": len(shorts), "marked": len(marks), "cancelled": cancelled, "returned": returned,
		"failures": len(failed), "elapsed_ms": clock.NowUTC().Sub(started).Milliseconds(),
		"deadline": deadlineText(deadline),
	})
	if len(failed) > 0 {
		return fmt.Errorf("材料の出た売建 %d 件を処置できませんでした（口座を確認してください）", len(failed))
	}
	return nil
}
