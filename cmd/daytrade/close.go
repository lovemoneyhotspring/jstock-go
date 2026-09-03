package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newCloseCmd() *cobra.Command {
	var liveFlag, yesFlag, ignoreWindowFlag bool
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "close",
		Short: "15:20〜: 今日建てた分を成行で手仕舞う（既定は dry-run）",
		Long: "15:20〜: 今日建てた分を成行で手仕舞う——現物の買いは売却、信用の買建は返済売り、\n" +
			"売建は返済買い（15:25 以降ならクロージング・オークションで引け値）。既定は dry-run。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return crash("引けの売り", "daytrade.crash",
				runClose(liveFlag, yesFlag, ignoreWindowFlag, dateFlag))
		},
	}
	cmd.Flags().BoolVar(&liveFlag, "live", false, "注文を出す。無ければ対象を示すだけ")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番の確認を省く（cron 用）")
	cmd.Flags().BoolVar(&ignoreWindowFlag, "ignore-window", false, "時間帯の外でも売る")
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

func runClose(live, yes, ignoreWindow bool, date string) error {
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
	if skipHoliday(day, "close") {
		return nil
	}
	if live && !ignoreWindow && !inWindow(cfg, "exit", now) {
		fmt.Printf("手仕舞いの時間帯の外（%s）。何もしません\n", describeWindow(cfg, "exit"))
		logInfo("daytrade.skip", "手仕舞いの時間帯の外",
			map[string]any{"reason": "window", "window": describeWindow(cfg, "exit")})
		digest.Skipped("window")
		return nil
	}
	allowed, reason := appSettings.CanExecuteLive(live, cfg.Execution.KillSwitch)
	logConfig(cfg, "close", map[string]any{"day": day.Format(DateLayout), "live": live})

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

	env := execute.Env{Cfg: cfg, Ledger: led, Day: day, Report: run, Out: os.Stdout, RetryWait: execute.DefaultRetryWait}
	var b broker.Broker
	if allowed {
		if b, err = connectBroker(cfg); err != nil {
			return err
		}
		// 朝の建玉や前回の手仕舞いで送信結果が分からなかったものを判定してから、数量を決める
		if err := resolvePending(env, b); err != nil {
			return err
		}
	}
	entries, dryRun, err := execute.LiveEntries(env)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Printf("今日の建玉が台帳にありません（dry-run %d 件）。何もしません\n", dryRun)
		logInfo("daytrade.skip", "売る対象なし", map[string]any{"reason": "no_buys", "dry_run": dryRun})
		if allowed {
			warnUnrecordedPositions(cfg, day)
		}
		return nil
	}

	// 約定数量はブローカーに聞く（部分約定・拒否をそのまま手仕舞いの数量に反映する）。
	// 確かめられなかった銘柄は unconfirmed に積んで、最後に人へ知らせたうえで異常終了する
	targets, unconfirmed, err := execute.RefreshEntries(env, b, entries)
	if err != nil {
		return err
	}
	if len(unconfirmed) > 0 {
		// 建玉が残っている可能性がある。人が板を見て手で処理する必要がある
		alert("デイトレ: 建玉の有無を確かめられません（持ち越しの恐れ）",
			strings.Join(unconfirmed, "\n"))
		digest.Anomaly("daytrade.unconfirmed_entries",
			fmt.Sprintf("%d 件の買い注文を照会できませんでした", len(unconfirmed)))
	}
	if len(targets) == 0 {
		if len(unconfirmed) > 0 {
			return fmt.Errorf("%d 件の買い注文を照会できず、手仕舞いを判断できません（口座を確認してください）",
				len(unconfirmed))
		}
		logInfo("daytrade.skip", "売る対象なし", map[string]any{"reason": "nothing_to_sell"})
		return nil
	}
	if err := confirmLive(allowed, yes); err != nil {
		return err
	}

	failures := execute.PlaceExits(env, b, targets)
	if len(failures) > 0 {
		// 売れ残りは持ち越しになる。人が手で売る必要があるので必ず知らせる
		alert(fmt.Sprintf("デイトレ: %d 件の手仕舞いが通らず（持ち越しの恐れ）", len(failures)),
			strings.Join(failures, "\n"))
		digest.Anomaly("daytrade.exit_failed", fmt.Sprintf("%d 件の手仕舞いが通らず", len(failures)))
	}
	logInfo("daytrade.run", "引けの売りを終了", map[string]any{
		"phase": "close", "live": allowed, "reason": reason,
		"sells": len(targets), "failures": len(failures),
	})
	digest.Note(map[string]any{
		"phase": "close", "live": allowed, "sells": len(targets),
		"failures": len(failures), "unconfirmed": len(unconfirmed),
	})
	if len(unconfirmed) > 0 {
		return fmt.Errorf("%d 件の買い注文を照会できませんでした（建玉が残っていないか口座を確認してください）",
			len(unconfirmed))
	}
	return nil
}

// warnUnrecordedPositions は台帳に今日の買いが無いのに、今日の候補だった銘柄を
// ブローカーが保有していれば知らせる。
//
// 台帳を失う・open が送信後に落ちて記録できない、といったときの保険。自動では売らない
// （他の戦略の保有かもしれない）。人が確かめて手で売る。
func warnUnrecordedPositions(cfg dtconfig.Config, day time.Time) {
	p, ok, err := dtplan.Load(appSettings.DaytradeDir(), day)
	if err != nil || !ok {
		return
	}
	symbols := map[string]struct{}{}
	for _, c := range p.Candidates {
		if c.Eligible || c.ShortEligible {
			symbols[c.Symbol] = struct{}{}
		}
	}
	b, err := connectBroker(cfg)
	if err != nil {
		return
	}
	// 信用建玉も見る。立花の GetPositions は現物しか返さないので、
	// これを使わないと日計りで建てた玉が「無い」ことになる。
	positions, err := broker.PositionsBySymbolIncludingMargin(b)
	if err != nil {
		logWarn("daytrade.reconcile", "建玉を照会できず保険の確認を省略", map[string]any{"error": err.Error()})
		return
	}
	held := map[string]decimal.Decimal{}
	for symbol, position := range positions {
		if _, ok := symbols[symbol]; ok && !position.Quantity.IsZero() {
			held[symbol] = position.Quantity
		}
	}
	if len(held) == 0 {
		return
	}
	var parts []string
	names := make([]string, 0, len(held))
	for symbol := range held {
		names = append(names, symbol)
	}
	sortStrings(names)
	detail := map[string]any{}
	for _, symbol := range names {
		quantity := held[symbol]
		suffix := ""
		if quantity.IsNegative() {
			suffix = "（売建）"
		}
		parts = append(parts, fmt.Sprintf("%s %s 株%s", symbol, yen(quantity), suffix))
		detail[symbol] = quantity.String()
	}
	text := strings.Join(parts, "、")
	fmt.Printf("台帳に無い建玉があります（今日の候補の銘柄）: %s。手で確かめてください\n", text)
	logError("daytrade.reconcile", "台帳に無い建玉",
		map[string]any{"day": day.Format(DateLayout), "held": detail})
	alert("デイトレ: 台帳に無い建玉（持ち越しの恐れ）", text)
}
