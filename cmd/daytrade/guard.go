package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
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

// guardState は runGuard の 1 回の実行で、段（準備・点検の対象・接続・処置）をまたいで持ち回る値。
type guardState struct {
	// live は --live、yes は本番の確認を省く（--yes）
	live, yes bool
	cfg       dtconfig.Config
	// now は開始時に 1 回だけ読んだ時刻。判定日・時間帯・締め切り・記録簿の鮮度はこれで見る
	now     time.Time
	day     time.Time
	started time.Time
	cal     *calendar.Calendar
	// allowed は実際に送るか（live かつ本番口座かつ kill_switch でない）。reason はその理由
	allowed bool
	reason  string
	// deadline は時間帯の終わり（live で --ignore-window でないとき）と開始 + max_run_seconds の早い方
	deadline time.Time

	env execute.Env
	// shorts は今日の売建（生きているか約定のある）の銘柄、ev は記録簿から読んだ材料、
	// marks は材料の出た売建（銘柄 → 材料の種類）
	shorts map[string]bool
	ev     corpEvents
	marks  map[string]string
	// b は実際に送るとき（allowed）だけ繋ぐ（dry-run は nil）
	b broker.Broker
}

// runGuard は材料の点検の 1 回。段の順（準備 → 台帳 → 今日の売建 → 材料の印 → 接続 → 処置）は変えない
// （testdata/guard_flow の流れのテストが見張る）。台帳の Close と実行品質の書き出しの defer はここに置く
// （どの段で抜けても走る）。
func runGuard(live, yes, ignoreWindow bool, date string) error {
	s, done, err := prepareGuard(live, yes, ignoreWindow, date)
	if done || err != nil {
		return err
	}
	led, err := dtledger.Open(appSettings.DaytradeDBPath())
	if err != nil {
		return err
	}
	defer led.Close()
	defer flushExecution(s.day)
	// with-lock.sh の打ち切り（SIGTERM）でも記録・保留した通知・ダイジェストを残す
	defer flushOnSignal(s.day)()
	// 運用通知は注文を出し切ってから送る（同期の HTTP を発注の前に挟まない。run.DeferAlerts）
	if s.live {
		run.DeferAlerts()
	}
	s.env = execute.Env{
		Cfg: s.cfg, Ledger: led, Day: s.day, Report: run, Out: os.Stdout,
		RetryWait: execute.DefaultRetryWait, Deadline: s.deadline,
	}

	if done, err := s.findShorts(); done || err != nil {
		return err
	}
	if done, err := s.markShorts(); done || err != nil {
		return err
	}
	if done, err := s.connect(); done || err != nil {
		return err
	}
	return s.act()
}

// prepareGuard は設定を読み、判定日と締め切りを決める。休場日・材料の点検が無効・時間帯の外なら
// 見送りを記録して done を返す（err は nil）。
func prepareGuard(live, yes, ignoreWindow bool, date string) (s *guardState, done bool, err error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, false, err
	}
	fmt.Println(appSettings.DescribeMode(live, cfg.Execution.KillSwitch))
	now := clock.NowUTC()
	day, err := dayOrToday(date, now)
	if err != nil {
		return nil, false, err
	}
	cal, holiday := holidayCalendar(day, "guard", live)
	if holiday {
		return nil, true, nil
	}
	if !cfg.Margin.Enabled || !cfg.Margin.CancelOnCorpEvent {
		fmt.Println("信用売りの脚か margin.cancel_on_corp_event が無効。何もしません")
		logInfo("daytrade.skip", "材料の点検は無効", map[string]any{"reason": "disabled", "phase": "guard"})
		return nil, true, nil
	}
	if live && !ignoreWindow && !cfg.Execution.InWindow("guard", now, jst) {
		fmt.Printf("材料の点検の時間帯の外（%s）。何もしません\n", describeWindow(cfg, "guard"))
		logInfo("daytrade.skip", "材料の点検の時間帯の外",
			map[string]any{"reason": "window", "phase": "guard", "window": describeWindow(cfg, "guard")})
		digest.Skipped("window")
		return nil, true, nil
	}
	allowed, reason := appSettings.CanExecuteLive(live, cfg.Execution.KillSwitch)
	deadline := cfg.Execution.RunDeadline("guard", now, live && !ignoreWindow, jst)
	logConfig(cfg, "guard", map[string]any{
		"day": day.Format(DateLayout), "live": live,
		"deadline": deadlineText(deadline), "max_run_seconds": cfg.Execution.MaxRunSeconds,
	})
	return &guardState{
		live: live, yes: yes, cfg: cfg, now: now, day: day, started: now, cal: cal,
		allowed: allowed, reason: reason, deadline: deadline,
	}, false, nil
}

// findShorts は今日の売建（約定なしで終わったものを除く）を集める。無ければ done を返す
// ——ブローカーにも記録簿にも触らない（10 分おきに回るので、ログインを増やさない）。
func (s *guardState) findShorts() (done bool, err error) {
	entries, _, err := execute.LiveEntries(s.env)
	if err != nil {
		return false, err
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
		return true, nil
	}
	s.shorts = shorts
	return false, nil
}

// markShorts は記録簿を読み、材料（TOB・MBO など）の出た売建に印を付ける。記録簿を読めなければ
// 知らせてエラー、古ければ知らせたうえで手元の分で点検する。印が 1 つも無ければ done を返す。
func (s *guardState) markShorts() (done bool, err error) {
	ev, err := loadCorpEvents(context.Background(), s.cfg.Margin, s.day, s.now, s.cal.Closed)
	if err != nil {
		logError("daytrade.news_stale", "ニュースの記録簿を読めず売建の材料を点検できない", map[string]any{"error": err.Error()})
		digest.Anomaly("daytrade.news_stale", "売建の材料を点検できない: "+err.Error())
		alert("デイトレ: ニュースの記録簿を読めず、売建の材料（TOB など）を点検できません", err.Error())
		return false, err
	}
	if detail := ev.staleness(s.now, s.cfg.Margin.CorpEventMaxStalenessMinutes); detail != "" {
		// 古くても手元の分では点検する。取り込み（news sync）が止まっていることは知らせる
		fmt.Println("ニュースの記録簿が古いまま点検します: " + detail)
		logWarn("daytrade.news_stale", "ニュースの記録簿が古いまま売建を点検", map[string]any{"reason": detail, "phase": "guard"})
		digest.Anomaly("daytrade.news_stale", "売建の点検の記録簿が古い: "+detail)
	}

	marks := map[string]string{}
	// 銘柄の順に出す（map の順だと表示とログの並びが実行ごとに入れ替わる）
	for _, symbol := range slices.Sorted(maps.Keys(s.shorts)) {
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
		fmt.Printf("材料の出た売建はありません（%d 銘柄を点検）\n", len(s.shorts))
		logInfo("daytrade.run", "材料の点検を終了", map[string]any{
			"phase": "guard", "live": s.allowed, "shorts": len(s.shorts), "marked": 0,
			"elapsed_ms": clock.NowUTC().Sub(s.started).Milliseconds(),
		})
		return true, nil
	}
	s.ev, s.marks = ev, marks
	return false, nil
}

// connect は処置の残る売建があるかを台帳だけで確かめ（無ければ done。取消・返済の済んだ売建で
// 10 分ごとにログインしない）、live ならブローカーに繋いで送信結果不明の注文を判定する。
// 最後に本番の確認を取る。
func (s *guardState) connect() (done bool, err error) {
	pending, err := execute.GuardPending(s.env, s.marks)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 {
		fmt.Printf("材料の出た売建 %d 銘柄は処置済み（取消・返済が済んでいるか返済が生きている）\n", len(s.marks))
		logInfo("daytrade.run", "材料の点検を終了", map[string]any{
			"phase": "guard", "live": s.allowed, "shorts": len(s.shorts), "marked": len(s.marks), "pending": 0,
			"elapsed_ms": clock.NowUTC().Sub(s.started).Milliseconds(),
		})
		return true, nil
	}

	if s.allowed {
		if s.b, err = openBroker(s.cfg); err != nil {
			alert("デイトレ: 材料（TOB など）の出た売建があるのに証券会社に接続できません。口座を確認してください",
				strings.Join(sortedKeys(s.marks), "、")+": "+err.Error())
			return false, err
		}
		broker.SetDeadline(s.b, s.deadline)
		// 送信結果の分からない注文を先に判定する。判定できなくても処置は続ける——判定できなかった
		// 注文は照会で「分からない」になり、取消も返済もしない（GuardCorpEvents）
		if err := resolvePending(s.env, s.b); err != nil {
			logError("daytrade.pending_unresolved", "送信結果不明の注文を判定できず材料の処置を続ける",
				map[string]any{"error": err.Error()})
		}
	}
	if err := confirmLive(s.allowed, s.yes); err != nil {
		return false, err
	}
	return false, nil
}

// act は材料の出た売建を処置し（dry-run は示すだけ）、保留した通知を送ってから結果を報告する。
func (s *guardState) act() error {
	actions, err := execute.GuardCorpEvents(s.env, s.b, s.marks)
	run.FlushAlerts()
	if err != nil {
		return err
	}
	return s.report(actions)
}

// report は処置の 1 件ずつを表示・ログに残し、取消・返済した分と処置できなかった分を知らせて、
// 点検の終わりを記録する。処置できなかった売建があればエラー（人が口座を見る）。
func (s *guardState) report(actions []execute.GuardAction) error {
	var acted, failed []string
	cancelled, returned := 0, 0
	for _, a := range actions {
		m := s.ev.marks[a.Symbol]
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
	if s.allowed && len(acted) > 0 {
		alert(fmt.Sprintf("デイトレ: 材料（TOB など）の出た売建 %d 件を取消・返済しました", len(acted)),
			strings.Join(acted, "\n"))
	}
	if len(failed) > 0 {
		digest.Anomaly("daytrade.corp_guard_failed", fmt.Sprintf("材料の出た売建 %d 件を処置できず", len(failed)))
		if s.allowed {
			alert("デイトレ: 材料（TOB など）の出た売建を取消・返済できません。口座を確認してください",
				strings.Join(failed, "\n"))
		}
	}
	logInfo("daytrade.run", "材料の点検を終了", map[string]any{
		"phase": "guard", "live": s.allowed, "reason": s.reason,
		"shorts": len(s.shorts), "marked": len(s.marks), "cancelled": cancelled, "returned": returned,
		"failures": len(failed), "elapsed_ms": clock.NowUTC().Sub(s.started).Milliseconds(),
		"deadline": deadlineText(s.deadline),
	})
	if len(failed) > 0 {
		return fmt.Errorf("材料の出た売建 %d 件を処置できませんでした（口座を確認してください）", len(failed))
	}
	return nil
}
