package logging

import (
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
	Schema    int            `json:"schema"`
	TSUTC     string         `json:"ts_utc"`
	RunID     string         `json:"run_id"`
	App       string         `json:"app"`
	Env       string         `json:"env"`
	Command   string         `json:"command"`
	Level     string         `json:"level"`
	Code      string         `json:"code,omitempty"`
	Msg       string         `json:"msg"`
	Extra     map[string]any `json:"extra,omitempty"`
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
}

// NewLogger は新しい構造化ロガーを作成する。
func NewLogger(app, env, runID, command, logDir string) (*Logger, error) {
	var f *os.File
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create log dir: %w", err)
		}
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s.jsonl", app, env))
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
		timestamp := clock.Fmt(time.Now(), clock.Tokyo, true)
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
