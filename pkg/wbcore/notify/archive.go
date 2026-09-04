package notify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// Discord に送ったものをローカルにも残す。
//
// Discord は後から読み返せるが、エージェント（claude）や CLI からは読めない。
// 「昨日の異常通知は何だった？」に答えるには手元に控えが要る。送れなかったときも
// 残す——配達に失敗したものこそ後から拾いたい。
//
// 置き場は state/notify/<YYYY-MM-DD>.jsonl（1 投稿 1 行、UTC の日付で 1 ファイル）。
// ログ（90 日）より短い 30 日で消す。日次レポートの本文そのものは
// state/reports/daily-<日付>.md にも残る（deploy/daily-report.sh が書く）。

// ArchiveRetainDays は控えを残す日数。
const ArchiveRetainDays = 30

// 控えの種類。
const (
	KindAlert  = "alert"
	KindReport = "report"
)

// archiveDirEnvVar は state の置き場（settings と同じ既定）。
// notify は settings を import しない（循環する）ので環境変数を直接見る。
const archiveDirEnvVar = "WBJP_STATE_DIR"

// ArchiveDir は控えの置き場。
func ArchiveDir() string {
	stateDir := os.Getenv(archiveDirEnvVar)
	if stateDir == "" {
		stateDir = "state"
	}
	return filepath.Join(stateDir, "notify")
}

// Record は控え 1 件。
type Record struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	ChannelID string    `json:"channel_id"`
	ThreadID  string    `json:"thread_id,omitempty"`
	// OK は Discord に届いたか。Error は届かなかった理由。
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// archive は控えを 1 行書き、古いファイルを消す。
//
// 失敗しても通知そのものは成功扱いにする（控えが書けないことで運用は止めない）。
// 呼び出し側が結果を使わないので、理由は標準エラーに出すだけ。
func archive(rec Record) {
	if rec.At.IsZero() {
		rec.At = clock.NowUTC()
	}
	dir := ArchiveDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "通知の控えを置けません: %v\n", err)
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "通知の控えを組み立てられません: %v\n", err)
		return
	}
	path := filepath.Join(dir, rec.At.UTC().Format(archiveDayLayout)+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "通知の控えを書けません: %v\n", err)
		return
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "通知の控えを書けません: %v\n", err)
		return
	}
	pruneArchive(dir, rec.At)
}

const archiveDayLayout = "2006-01-02"

// pruneArchive は保持日数を過ぎた控えを消す。
// 日付として読めない名前のファイルは触らない（人が置いたものかもしれない）。
func pruneArchive(dir string, now time.Time) {
	cutoff := now.UTC().AddDate(0, 0, -ArchiveRetainDays).Format(archiveDayLayout)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(name, ".jsonl")
		if _, err := time.Parse(archiveDayLayout, day); err != nil {
			continue
		}
		if day < cutoff {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// ReadArchive は日付（UTC、両端を含む）の範囲の控えを古い順に読む。
// 空文字は端を切らない。壊れた行は飛ばす（1 行の破損で全部読めなくしない）。
func ReadArchive(from, to string) ([]Record, error) {
	dir := ArchiveDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var days []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(name, ".jsonl")
		if _, err := time.Parse(archiveDayLayout, day); err != nil {
			continue
		}
		if (from != "" && day < from) || (to != "" && day > to) {
			continue
		}
		days = append(days, day)
	}
	slices.Sort(days)

	var out []Record
	for _, day := range days {
		raw, err := os.ReadFile(filepath.Join(dir, day+".jsonl"))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec Record
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			out = append(out, rec)
		}
	}
	return out, nil
}
