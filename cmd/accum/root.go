package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

var (
	appSettings   = settings.LoadAppSettings()
	configDirFlag string
	logLevelFlag  string
	jsonLogsFlag  bool
	// run はこの実行（run_id・ログ・ダイジェスト・通知）。入口で 1 回だけ起こす
	run *cli.Run
	// runCtx は入口で束ねた実行コンテキスト。履歴の run_id をログと揃えるために使う。
	runCtx = context.Background()
)

// levelRank はログ水準の重み。--log-level はこれ以上の行だけを端末に出す。
var levelRank = map[string]int{"debug": 10, "info": 20, "warn": 30, "warning": 30, "error": 40}

// terminalWriter は端末に出るログ行を --log-level で絞り、--json-logs なら JSON に直す。
//
// wbcore/logging の Logger は水準での絞り込みも端末の JSON 出力も持たず、
// 端末には "HH:MM:SS [level] [code] メッセージ" の 1 行を書く。ファイルには常に
// JSON が積まれるので、ここで整えるのは人が見る側だけでよい。
type terminalWriter struct {
	out       io.Writer
	threshold int
	asJSON    bool
}

func (w *terminalWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		stamp, level, code, message := splitLogLine(line)
		if levelRank[level] < w.threshold {
			continue
		}
		if !w.asJSON {
			fmt.Fprintln(w.out, line)
			continue
		}
		encoded, err := json.Marshal(map[string]any{
			"ts": stamp, "level": level, "code": code, "msg": message,
		})
		if err != nil {
			fmt.Fprintln(w.out, line)
			continue
		}
		fmt.Fprintln(w.out, string(encoded))
	}
	return len(p), nil
}

// splitLogLine は端末向けの 1 行を分解する。形が違えば水準を info とみなす
// （落とすより出しすぎる方が安全）。
func splitLogLine(line string) (stamp, level, code, message string) {
	level = "info"
	message = line
	open := strings.Index(line, "[")
	if open < 0 {
		return "", level, "", message
	}
	stamp = strings.TrimSpace(line[:open])
	rest := line[open:]
	if end := strings.Index(rest, "]"); end > 0 {
		level = strings.ToLower(rest[1:end])
		rest = strings.TrimSpace(rest[end+1:])
	}
	if strings.HasPrefix(rest, "[") {
		if end := strings.Index(rest, "]"); end > 0 {
			code = rest[1:end]
			rest = strings.TrimSpace(rest[end+1:])
		}
	}
	return stamp, level, code, rest
}

// newRunLogger は入口で開いたロガーに --log-level / --json-logs を反映して返す。
// run_id は入口で束ねたものなので、ログと履歴（Parquet）が突き合わせられる。
// 閉じるのは run.Finish の役目（ここで defer Close しない）。
func newRunLogger(command string) (*logging.Logger, error) {
	if run == nil || run.Logger == nil {
		return nil, fmt.Errorf("%s: 実行の記録が起きていません（入口を通っていない）", command)
	}
	logger := run.Logger
	threshold, ok := levelRank[strings.ToLower(logLevelFlag)]
	if !ok {
		threshold = levelRank["info"]
	}
	logger.SetOutput(&terminalWriter{out: os.Stderr, threshold: threshold, asJSON: jsonLogsFlag})
	if err := logger.SetTimezone(appSettings.Timezone); err != nil {
		// 時間帯が読めなくてもログは出す（表示の時刻がずれるだけ）
		logger.Warn("accum.timezone", fmt.Sprintf("WBJP_TIMEZONE=%q を解釈できません（表示は UTC）: %v", appSettings.Timezone, err))
	}
	return logger, nil
}

// parseDay は --from / --to の日付を確かめる。空なら空のまま通す。
func parseDay(value, flag string) (string, error) {
	if _, err := cli.ParseDay(value); err != nil {
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	return value, nil
}

// mustDay は検証済みの "YYYY-MM-DD" を time.Time にする。
func mustDay(day string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// yen は桁の大きい金額を万に丸める。80 桁の端末で表が潰れるのを避ける。
func yen(value float64) string {
	if value >= 100_000 || value <= -100_000 {
		return fmt.Sprintf("%.0f万", value/10_000)
	}
	return fmt.Sprintf("%.0f", value)
}
