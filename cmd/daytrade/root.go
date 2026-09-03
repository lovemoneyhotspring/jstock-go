package main

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/reconcile"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

var (
	appSettings   = settings.LoadAppSettings()
	configDirFlag string
	// run はこの実行（run_id・ログ・ダイジェスト・通知）。入口で 1 回だけ起こす
	run *cli.Run
)

// DateLayout は CLI が受け取る日付の形。
const DateLayout = cli.DateLayout

// sampleSize はログに残す銘柄名の見本の上限（全部は長い。件数は別に残す）。
const sampleSize = 20

// watchRows は様子見モード（資金 0）で見せる候補の数。
const watchRows = 5

// rankingExtra は順位表に残す次点の数（N の先）。
const rankingExtra = 5

// jst は東証の時間帯。判定はすべて JST で行う。
var jst = clock.Tokyo

func logInfo(code, msg string, extra map[string]any)  { run.Info(code, msg, extra) }
func logWarn(code, msg string, extra map[string]any)  { run.Warn(code, msg, extra) }
func logError(code, msg string, extra map[string]any) { run.Error(code, msg, extra) }

// runID はこの実行の識別子（履歴の run_id をログと揃える）。
func runID() string {
	if run == nil {
		return ""
	}
	return run.RunID
}

func loadConfig() (dtconfig.Config, error) {
	dir := configDirFlag
	if dir == "" {
		dir = dtconfig.DefaultConfigDir
	}
	return dtconfig.Load(dir)
}

func openArchive() *archive.Archive {
	return archive.NewArchive(appSettings.JQuantsArchiveDir())
}

func historyStore() *history.Store { return dthistory.StoreFor(appSettings) }

// appendHistory は履歴に 1 ファイル足す。記録の失敗で売買を止めない。
func appendHistory(kind string, frame history.Frame, day time.Time) string {
	path, err := historyStore().Append(kind, frame, day, history.AppendOptions{RunID: runID()})
	if err != nil {
		logWarn("daytrade.history", "履歴の追記に失敗", map[string]any{"kind": kind, "error": err.Error()})
		return ""
	}
	return path
}

func parseDate(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	d, err := time.Parse(DateLayout, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("日付の形式が不正です: %s（YYYY-MM-DD）", text)
	}
	return d, nil
}

// todayJST は JST で見た今日（判定日の既定）。
func todayJST(now time.Time) time.Time {
	local := clock.ToZone(now, jst)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// dayOrToday は --date の指定、無ければ今日（JST）。
func dayOrToday(text string, now time.Time) (time.Time, error) {
	day, err := parseDate(text)
	if err != nil {
		return time.Time{}, err
	}
	if day.IsZero() {
		return todayJST(now), nil
	}
	return day, nil
}

// inWindow は今が entry / exit の時間帯か（JST）。
func inWindow(cfg dtconfig.Config, name string, now time.Time) bool {
	sh, sm, eh, em, err := cfg.Execution.Window(name)
	if err != nil {
		return false
	}
	local := clock.ToZone(now, jst)
	minutes := local.Hour()*60 + local.Minute()
	return sh*60+sm <= minutes && minutes <= eh*60+em
}

func describeWindow(cfg dtconfig.Config, name string) string {
	sh, sm, eh, em, err := cfg.Execution.Window(name)
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%02d:%02d〜%02d:%02d JST", sh, sm, eh, em)
}

// skipHoliday は休場日なら真（何もしない）。
//
// 気配が取れずに毎回アラートを飛ばさないため、休場の判定を発注経路より前に置く。
func skipHoliday(day time.Time, phase string) bool {
	if calendar.FromArchive(openArchive()).IsTradingDay(day) {
		return false
	}
	fmt.Printf("%s は休場日。何もしません\n", day.Format(DateLayout))
	logInfo("daytrade.skip", "休場日", map[string]any{"reason": "holiday", "phase": phase, "day": day.Format(DateLayout)})
	digest.Skipped("holiday")
	return true
}

// connectBroker は設定のブローカーに繋ぐ（部品は wbcore/cli）。
// デイトレは実機だけ——paper を通すと --live が模型に向いて「発注した」ことになる。
func connectBroker(cfg dtconfig.Config) (broker.Broker, error) {
	if cfg.Execution.Broker != "tachibana" {
		return nil, fmt.Errorf("未知の broker: %q（デイトレは tachibana のみ）", cfg.Execution.Broker)
	}
	return cli.ConnectBroker(cfg.Execution.Broker, appSettings)
}

// confirmLive は本番発注の前に人に確かめる（部品は wbcore/cli）。
func confirmLive(allowed, yes bool) error { return cli.ConfirmLive(appSettings, allowed, yes) }

// crash は cron で誰も端末を見ていないときのために、落ちた理由を通知してから返す。
func crash(title, code string, err error) error { return run.Crash(title, code, err) }

func alert(title, body string) { run.Alert(title, body) }

// 金額・割合の整形は wbcore/cli に集めてある。呼び出しが多いので短い別名を残す。
func yen(v any) string                 { return cli.Yen(v) }
func pct(v float64) string             { return cli.Pct(v) }
func yenPtr(v *decimal.Decimal) string { return cli.Yen(v) }

// resolvePending は送信結果不明（PENDING）の注文をプログラムで判定し、結果をダイジェストに残す。
//
// 誰も端末を見ていない前提なので、人に「確かめてください」とは言わない。判定できたものは
// 台帳を直し、決められないものだけを異常としてダイジェストに載せる（AI が読んで扱う）。
// 一覧を照会できなければエラー——判定できないまま実弾を出さない。
func resolvePending(env execute.Env, b broker.Broker) error {
	summary, err := execute.ResolvePending(env, b, reconcile.DefaultGrace)
	if err != nil {
		logError("daytrade.pending_unresolved", "送信結果不明の注文を判定できません", map[string]any{"error": err.Error()})
		digest.Anomaly("daytrade.pending_unresolved", err.Error())
		return err
	}
	if summary.Attributed+summary.NotSent+summary.Ambiguous+summary.TooRecent == 0 {
		return nil
	}
	digest.Note(summary.Fields("pending"))
	for _, line := range summary.Details {
		fmt.Println("  送信結果不明の注文: " + line)
	}
	if summary.Ambiguous > 0 {
		digest.Anomaly("daytrade.pending_ambiguous",
			fmt.Sprintf("%d 件の送信結果不明の注文を自動で決められません", summary.Ambiguous))
	}
	return nil
}
