// Package cli は 4 つのコマンド（wbjp / accum / daytrade / jquants）が共有する入口の部品。
//
// 実行の記録（run_id・ログ・ダイジェスト・異常終了の通知）、ブローカーへの接続、
// 本番発注前の確認、金額や日付の整形をここに集める。コマンドごとに書くと
// 「daytrade には通知があるが wbjp には無い」のような抜けが起きるため。
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
	"golang.org/x/term"
)

// DateLayout は CLI が受け取る日付の形。
const DateLayout = "2006-01-02"

// Run は CLI 1 回の実行。run_id をログ・履歴・ダイジェストで共有する。
type Run struct {
	App     string
	Command string
	RunID   string
	// Ctx は run_id を束ねた context。context を受け取る経路に渡す。
	Ctx context.Context
	// Logger は JSONL のロガー。ファイルを開けなかったときは端末だけに出す（nil にはならない）。
	Logger *logging.Logger
	// Alerter は運用通知の送り先。既定は notify.Alert。テストで差し替える。
	Alerter func(title, body string, logger *logging.Logger) bool
}

// StartRun は run_id を発行し、ログとダイジェストを起こす。コマンドの入口で 1 回だけ呼ぶ。
func StartRun(app string, s *settings.AppSettings, command string) *Run {
	ctx, runID := logging.BindRunContext(context.Background(), map[string]any{
		"app": app, "env": string(s.Env), "command": command,
	})
	logger, err := logging.NewLoggerForRun(ctx, app, string(s.Env), command, s.ResolvedLogDir())
	if err != nil {
		// ログが書けなくても売買は止めない。ただし黙らない。端末だけのロガーに落とす
		fmt.Fprintf(os.Stderr, "[warn] ログを開けません（ファイルには残りません）: %v\n", err)
		logger, _ = logging.NewLoggerForRun(ctx, app, string(s.Env), command, "")
	}
	digest.StartRun(digest.StartOptions{
		App: app, Env: string(s.Env), Command: command, RunID: runID, StateDir: s.StateDir,
	})
	return &Run{App: app, Command: command, RunID: runID, Ctx: ctx, Logger: logger, Alerter: notify.Alert}
}

// Finish はダイジェストを書き出してログを閉じる。err があれば失敗として記録する。
// os.Exit は defer を飛ばすので、main の最後で必ず呼ぶ。
func (r *Run) Finish(err error) {
	if r == nil {
		return
	}
	if err != nil {
		digest.Fail(r.App+".command_failed", err.Error())
	}
	if ferr := digest.Flush(); ferr != nil {
		fmt.Fprintf(os.Stderr, "[warn] ダイジェストを書けません: %v\n", ferr)
	}
	if r.Logger != nil {
		_ = r.Logger.Close()
	}
}

func (r *Run) Info(code, msg string, extra ...map[string]any) {
	if r != nil && r.Logger != nil {
		r.Logger.Info(code, msg, extra...)
	}
}

func (r *Run) Warn(code, msg string, extra ...map[string]any) {
	if r != nil && r.Logger != nil {
		r.Logger.Warn(code, msg, extra...)
	}
}

func (r *Run) Error(code, msg string, extra ...map[string]any) {
	if r != nil && r.Logger != nil {
		r.Logger.Error(code, msg, extra...)
	}
}

// Alert は運用通知。Webhook が未設定なら警告ログだけ。
func (r *Run) Alert(title, body string) {
	if r == nil {
		return
	}
	alerter := r.Alerter
	if alerter == nil {
		alerter = notify.Alert
	}
	alerter(title, body, r.Logger)
}

// Crash は cron で誰も端末を見ていないときのために、落ちた理由を記録・通知してから返す。
// RunE の戻りを包む: `return run.Crash("寄付の買い", "daytrade.crash", runOpen(...))`
func (r *Run) Crash(title, code string, err error) error {
	if err == nil {
		return nil
	}
	r.Error(code, title+"が異常終了", map[string]any{"error": err.Error()})
	digest.Fail(code, err.Error())
	r.Alert(fmt.Sprintf("%s: %sが異常終了", r.App, title), err.Error())
	return err
}

// ConnectBroker は設定名のブローカーに繋ぐ。paper はメモリ上の模型、tachibana は実機。
func ConnectBroker(name string, s *settings.AppSettings) (broker.Broker, error) {
	switch name {
	case "paper":
		return broker.NewPaperBroker(decimal.Zero, "open"), nil
	case "tachibana", "":
		creds, err := credentials.LoadTachibanaCredentials(s.Env, s.DotenvMap)
		if err != nil {
			return nil, err
		}
		return broker.NewTachibanaBroker(s.Env, creds, s.StateDir)
	default:
		return nil, fmt.Errorf("未知の broker: %q（paper / tachibana）", name)
	}
}

// ConfirmLive は本番発注の前に人に確かめる。
//
// 非対話（cron・パイプ）では確認を取れない。黙って通すと意図しない本番発注に
// なるので、明示的に --yes を求める。allowed が偽（dry-run）や本番でなければ何も聞かない。
func ConfirmLive(s *settings.AppSettings, allowed, yes bool) error {
	if !allowed || !s.Env.IsProduction() || yes {
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

// ParseDay は YYYY-MM-DD を UTC の 0 時にする。空ならゼロ値（＝指定なし）。
func ParseDay(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	day, err := time.ParseInLocation(DateLayout, text, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("日付は YYYY-MM-DD で指定してください: %q", text)
	}
	return day, nil
}

// Yen は金額を 3 桁区切りの整数にする。decimal / float64 / int を受ける。
func Yen(v any) string {
	switch value := v.(type) {
	case decimal.Decimal:
		return AddCommas(value.Round(0).String())
	case *decimal.Decimal:
		if value == nil {
			return ""
		}
		return AddCommas(value.Round(0).String())
	case float64:
		return AddCommas(decimal.NewFromFloat(value).Round(0).String())
	case int:
		return AddCommas(fmt.Sprint(value))
	case int64:
		return AddCommas(fmt.Sprint(value))
	default:
		return fmt.Sprint(v)
	}
}

// AddCommas は 3 桁区切り。金額は必ずこれを通す（画面とログで見た目を揃える）。
func AddCommas(text string) string {
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

// Pct は割合を符号付きの % にする（0.0123 → +1.23%）。
func Pct(v float64) string { return fmt.Sprintf("%+.2f%%", v*100) }

// Dash は空文字を「—」に置き換える（表の見た目を揃えるため）。
func Dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
