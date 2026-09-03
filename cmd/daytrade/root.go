package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
	"golang.org/x/term"
)

var (
	appSettings   = settings.LoadAppSettings()
	configDirFlag string
	runID         string
	logger        *logging.Logger
)

// DateLayout は CLI が受け取る日付の形。
const DateLayout = "2006-01-02"

// sampleSize はログに残す銘柄名の見本の上限（全部は長い。件数は別に残す）。
const sampleSize = 20

// watchRows は様子見モード（資金 0）で見せる候補の数。
const watchRows = 5

// rankingExtra は順位表に残す次点の数（N の先）。
const rankingExtra = 5

// jst は東証の時間帯。判定はすべて JST で行う。
var jst = clock.Tokyo

// setupRun はログとダイジェストを起こす。CLI の入口から 1 回だけ呼ぶ。
func setupRun(command string) {
	runID = logging.NewRunID()
	logger, _ = logging.NewLogger("daytrade", string(appSettings.Env), runID, command, appSettings.ResolvedLogDir())
	digest.StartRun(digest.StartOptions{
		App:      "daytrade",
		Env:      string(appSettings.Env),
		Command:  command,
		RunID:    runID,
		StateDir: appSettings.StateDir,
	})
}

// teardownRun はダイジェストを書き出してログを閉じる。
func teardownRun() {
	_ = digest.Flush()
	if logger != nil {
		_ = logger.Close()
	}
}

func logInfo(code, msg string, extra map[string]any) {
	if logger != nil {
		logger.Info(code, msg, extra)
	}
}

func logWarn(code, msg string, extra map[string]any) {
	if logger != nil {
		logger.Warn(code, msg, extra)
	}
}

func logError(code, msg string, extra map[string]any) {
	if logger != nil {
		logger.Error(code, msg, extra)
	}
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
	path, err := historyStore().Append(kind, frame, day, history.AppendOptions{RunID: runID})
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

// connectBroker は設定のブローカーに繋ぐ。
func connectBroker(cfg dtconfig.Config) (broker.Broker, error) {
	if cfg.Execution.Broker != "tachibana" {
		return nil, fmt.Errorf("未知の broker: %q（tachibana）", cfg.Execution.Broker)
	}
	creds, err := credentials.LoadTachibanaCredentials(appSettings.Env, appSettings.DotenvMap)
	if err != nil {
		return nil, err
	}
	return broker.NewTachibanaBroker(appSettings.Env, creds, appSettings.StateDir)
}

// confirmLive は本番発注の前に人に確かめる。
//
// 非対話（cron・パイプ）では確認を取れない。黙って通すと意図しない本番発注に
// なるので、明示的に --yes を求める。
func confirmLive(allowed, yes bool) error {
	if !allowed || !appSettings.Env.IsProduction() || yes {
		return nil
	}
	fmt.Println("本番環境で実際に発注します")
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("非対話環境では確認を取れません。cron から回すなら --yes を付けてください")
	}
	fmt.Print("続行しますか? [y/N]: ")
	var input string
	_, _ = fmt.Scanln(&input)
	if input != "y" && input != "Y" {
		return fmt.Errorf("中止しました")
	}
	return nil
}

// crash は cron で誰も端末を見ていないときのために、落ちた理由を通知してから返す。
func crash(title, code string, err error) error {
	if err == nil {
		return nil
	}
	logError(code, title+"が異常終了", map[string]any{"error": err.Error()})
	digest.Fail(code, err.Error())
	alert("デイトレ: "+title+"が異常終了", err.Error())
	return err
}

func alert(title, body string) {
	if logger != nil {
		_ = notifyAlert(title, body)
	}
}

func yen(v any) string {
	switch value := v.(type) {
	case decimal.Decimal:
		return addCommas(value.Round(0).String())
	case float64:
		return addCommas(decimal.NewFromFloat(value).Round(0).String())
	case int:
		return addCommas(fmt.Sprint(value))
	default:
		return fmt.Sprint(v)
	}
}

// addCommas は 3 桁区切り。金額は必ずこれを通す（画面とログで見た目を揃える）。
func addCommas(text string) string {
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	var out []byte
	for i, r := range []byte(text) {
		if i > 0 && (len(text)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, r)
	}
	if negative {
		return "-" + string(out)
	}
	return string(out)
}

func pct(v float64) string { return fmt.Sprintf("%+.2f%%", v*100) }

func yenPtr(v *decimal.Decimal) string {
	if v == nil {
		return ""
	}
	return yen(*v)
}
