package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

const Redacted = "***REDACTED***"

var (
	secretMu     sync.RWMutex
	secretValues = make(map[string]struct{})

	sensitiveKeys = []string{
		"x-app-key",
		"x-signature",
		"x-access-token",
		"app_key",
		"appKey",
		"app_secret",
		"appSecret",
		"account_id",
		"accountId",
		"account_number",
		"accountNumber",
		"secret",
		"password",
		"token",
	}

	kvPattern *regexp.Regexp
)

func init() {
	var escaped []string
	for _, k := range sensitiveKeys {
		escaped = append(escaped, regexp.QuoteMeta(k))
	}
	pattern := fmt.Sprintf(`(?i)(["']?(?:%s)["']?\s*[:=]\s*["']?)([^"'\s,}\]&]+)`, strings.Join(escaped, "|"))
	kvPattern = regexp.MustCompile(pattern)
}

// RegisterSecret は秘密の実値を登録し、以後のログ出力で必ずマスクする。
// 8文字未満の値は誤爆防止のため無視する。
func RegisterSecret(values ...string) {
	secretMu.Lock()
	defer secretMu.Unlock()
	for _, v := range values {
		trimmed := strings.TrimSpace(v)
		if len(trimmed) >= 8 {
			secretValues[trimmed] = struct{}{}
		}
	}
}

// ClearSecrets は登録済みの秘密をクリアする（主にテスト用）。
func ClearSecrets() {
	secretMu.Lock()
	defer secretMu.Unlock()
	secretValues = make(map[string]struct{})
}

// Redact は文字列から秘匿情報をマスクして返す。
func Redact(text string) string {
	if text == "" {
		return text
	}

	// パターンマッチによるマスク
	result := kvPattern.ReplaceAllString(text, "${1}"+Redacted)

	// 実値によるマスク（長い文字列から優先して置換）
	secretMu.RLock()
	var secrets []string
	for s := range secretValues {
		secrets = append(secrets, s)
	}
	secretMu.RUnlock()

	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})

	for _, s := range secrets {
		if strings.Contains(result, s) {
			result = strings.ReplaceAll(result, s, Redacted)
		}
	}

	return result
}

// LogRecord は JSON Lines ログの1行の構造。
type LogRecord struct {
	Schema  int            `json:"schema"`
	TSUTC   string         `json:"ts_utc"`
	RunID   string         `json:"run_id"`
	App     string         `json:"app"`
	Env     string         `json:"env"`
	Command string         `json:"command"`
	Level   string         `json:"level"`
	Code    string         `json:"code,omitempty"`
	Msg     string         `json:"msg"`
	Extra   map[string]any `json:"extra,omitempty"`
}

// Logger は構造化ロガー。
type Logger struct {
	mu      sync.Mutex
	app     string
	env     string
	runID   string
	command string
	file    *os.File
	out     io.Writer
	// tz は端末表示の時間帯。ファイルに書く ts_utc は常に UTC。
	tz *time.Location
}

// NewLogger は新しい構造化ロガーを作成する。
func NewLogger(app, env, runID, command, logDir string) (*Logger, error) {
	var f *os.File
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create log dir: %w", err)
		}
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s.jsonl", app, env))

		// 開く前に日付が変わっていれば退避する。cron のプロセスはどれも短命なので、
		// 起動時に 1 度見れば「その日の最初のプロセスが退避する」形になる。
		// 退避に失敗してもログは書きたいので、警告を stderr に出して続ける。
		if err := rotateLog(logPath, time.Now(), RetainDays); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] ログを退避できませんでした: %v\n", err)
		}

		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file %s: %w", logPath, err)
		}
		f = file
	}

	return &Logger{
		app:     app,
		env:     env,
		runID:   runID,
		command: command,
		file:    f,
		out:     os.Stderr,
		tz:      clock.Tokyo,
	}, nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

func (l *Logger) log(level, code, msg string, extra map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	redactedMsg := Redact(msg)
	redactedExtra := make(map[string]any)
	for k, v := range extra {
		switch val := v.(type) {
		case string:
			redactedExtra[k] = Redact(val)
		default:
			redactedExtra[k] = val
		}
	}

	record := LogRecord{
		Schema:  1,
		TSUTC:   clock.NowUTC().Format(time.RFC3339Nano),
		RunID:   l.runID,
		App:     l.app,
		Env:     l.env,
		Command: l.command,
		Level:   level,
		Code:    code,
		Msg:     redactedMsg,
		Extra:   redactedExtra,
	}

	bytes, err := json.Marshal(record)
	if err == nil {
		if l.file != nil {
			_, _ = l.file.Write(bytes)
			_, _ = l.file.WriteString("\n")
		}
	}

	// 端末表示 (簡潔なフォーマット)
	if l.out != nil {
		tz := l.tz
		if tz == nil {
			tz = clock.Tokyo
		}
		timestamp := clock.Fmt(time.Now(), tz, true)
		codeStr := ""
		if code != "" {
			codeStr = "[" + code + "] "
		}
		_, _ = fmt.Fprintf(l.out, "%s [%s] %s%s\n", timestamp, level, codeStr, redactedMsg)
	}
}

func (l *Logger) Info(code, msg string, extra ...map[string]any) {
	var ex map[string]any
	if len(extra) > 0 {
		ex = extra[0]
	}
	l.log("info", code, msg, ex)
}

func (l *Logger) Warn(code, msg string, extra ...map[string]any) {
	var ex map[string]any
	if len(extra) > 0 {
		ex = extra[0]
	}
	l.log("warn", code, msg, ex)
}

func (l *Logger) Error(code, msg string, extra ...map[string]any) {
	var ex map[string]any
	if len(extra) > 0 {
		ex = extra[0]
	}
	l.log("error", code, msg, ex)
}

func (l *Logger) Debug(code, msg string, extra ...map[string]any) {
	var ex map[string]any
	if len(extra) > 0 {
		ex = extra[0]
	}
	l.log("debug", code, msg, ex)
}

// ---------------------------------------------------------------------------
// 実行コンテキスト（run_id）
//
// 1 回の CLI 実行のログを後から 1 本の線として読めるようにするための仕組み。
// Python 版は structlog の contextvars に束ねていた。Go には contextvars が
// 無いので context.Context で持ち回る。ただし記録系（history / execution /
// digest）は context を受け取らない経路からも run_id を必要とするため、
// 「このプロセスの現在の実行」もパッケージ変数に控えておく
// （CLI プロセスの実行は 1 回きりなので、これで取り違えは起きない）。
// ---------------------------------------------------------------------------

// RunContext は 1 回の実行のあいだ全ログに付く項目。
type RunContext struct {
	RunID string
	// Fields は app / env / command / config_dir など、その実行を特定する情報。
	Fields map[string]any
}

type runContextKey struct{}

var (
	runMu      sync.RWMutex
	currentRun *RunContext
)

// NewRunID は実行の識別子を発行する。12 桁の 16 進——ログを目で追うときに
// 短く、1 日の実行回数に対して衝突しない長さ。
func NewRunID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		// 乱数が取れないのは異常事態だが、ログの識別子のために実行を落とさない
		return fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(buf)
}

// BindRunContext はこの実行のあいだ全ログに付く項目を束ね、run_id を発行する。
// 返した context を以降の処理に渡す。
func BindRunContext(ctx context.Context, fields map[string]any) (context.Context, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	copied := make(map[string]any, len(fields))
	for key, value := range fields {
		copied[key] = value
	}
	run := &RunContext{RunID: NewRunID(), Fields: copied}

	runMu.Lock()
	currentRun = run
	runMu.Unlock()

	return context.WithValue(ctx, runContextKey{}, run), run.RunID
}

// RunContextOf は context に束ねられた実行情報を返す。無ければプロセスの現在の実行。
func RunContextOf(ctx context.Context) *RunContext {
	if ctx != nil {
		if run, ok := ctx.Value(runContextKey{}).(*RunContext); ok && run != nil {
			return run
		}
	}
	runMu.RLock()
	defer runMu.RUnlock()
	return currentRun
}

// CurrentRunID はいま束ねている実行の run_id。BindRunContext の前なら空文字。
//
// ログ以外の記録（選定の履歴など）に同じ ID を付け、ログと突き合わせられるようにする。
func CurrentRunID(ctx context.Context) string {
	if run := RunContextOf(ctx); run != nil {
		return run.RunID
	}
	return ""
}

// ResetRunContext はプロセスの現在の実行を捨てる（テスト用）。
func ResetRunContext() {
	runMu.Lock()
	defer runMu.Unlock()
	currentRun = nil
}

// ---------------------------------------------------------------------------
// 時間帯付きのタイムスタンプ
// ---------------------------------------------------------------------------

// Timestamper は指定した時間帯で表示用の時刻文字列を作る関数を返す。
//
// 未知の時間帯名はここで早めに弾く（毎行の出力時に失敗すると気づけない）。
// 保存と演算は常に UTC で、時間帯は表示にだけ効く。
func Timestamper(timezone string) (func() string, error) {
	loc, err := clock.Zone(timezone)
	if err != nil {
		return nil, err
	}
	return func() string { return clock.StampISO(loc) }, nil
}

// SetTimezone は端末表示の時間帯を差し替える。ファイルに書く ts_utc は常に UTC
// のままなので、表示だけが変わる。
func (l *Logger) SetTimezone(timezone string) error {
	loc, err := clock.Zone(timezone)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tz = loc
	return nil
}

// SetOutput は端末表示の書き出し先を差し替える（テスト用。nil なら表示しない）。
func (l *Logger) SetOutput(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = w
}

// RunID はこのロガーが付ける実行の識別子。
func (l *Logger) RunID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runID
}

// NewLoggerForRun は context に束ねた run_id を使ってロガーを作る。
// BindRunContext の直後に呼べば、ログと履歴の run_id が必ず一致する。
func NewLoggerForRun(ctx context.Context, app, env, command, logDir string) (*Logger, error) {
	return NewLogger(app, env, CurrentRunID(ctx), command, logDir)
}
