// Package digest は日次ダイジェスト——AI が最初に読む 1 ファイル。
//
// なぜ要るか:
// 運用の記録は JSONL のログ（state/logs/<app>-<env>.jsonl）に全部あるが、1 日ぶんが
// 数 MB になる。「今日ちゃんと動いたか」を知るためだけに、AI がその全部を読むのは
// 無駄が大きい。一方で「読まなくてよい行」を後から見分けるのは難しい——動いただけの
// 行と、判断した行が同じ形で並んでいるから。
//
// そこでダイジェストは逆向きに作る。各実行が終わるときに、その実行を 1 行に畳んで
// state/digest/<env>-<日付>.jsonl に足す。AI はまずこれを読み、異常（anomalies）が
// 載っている実行だけ run_id でログに降りる。
//
//	# 今日の全実行（1 日あたり 50KB 程度）
//	cat state/digest/prod-2026-09-03.jsonl
//
//	# 異常があったものだけ
//	jq 'select(.anomalies)' state/digest/prod-2026-09-03.jsonl
//
// なぜ 1 実行 1 行の追記なのか:
// cron のジョブは互いに時刻をずらしてあるが、重ならない保証は無い（flock は同じ
// ジョブ同士にしか効かない）。1 日 1 個の JSON を読んで書き換える形にすると、
// 重なった瞬間に片方の記録が消える。1 行の追記なら、POSIX の追記はアトミックなので
// 競合しても壊れない。
//
// 項目:
// ts_utc / app / env / command / run_id / outcome / dur_ms は必ず付く。それ以外は
// Note で足したものがそのまま平らに並ぶ（入れ子にしないのは、読む側の絞り込みを
// 簡単にするため）。
package digest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// Schema はダイジェストの形式の版。項目を変えたら上げる。
const Schema = "wbjp.digest.v1"

// run はいま走っている実行の集計。
type run struct {
	app       string
	env       string
	command   string
	runID     string
	path      string
	started   time.Time
	outcome   string
	fields    map[string]any
	anomalies []string
	written   bool
}

var (
	mu      sync.Mutex
	current *run
)

// Path はその日のダイジェストの置き場。アプリを分けないのは、AI が
// 「今日どう動いたか」を 1 ファイルで読めるようにするため。
func Path(stateDir, env, day string) string {
	return filepath.Join(stateDir, "digest", fmt.Sprintf("%s-%s.jsonl", env, day))
}

// StartOptions は StartRun の引数。
type StartOptions struct {
	App      string
	Env      string
	Command  string
	RunID    string
	StateDir string
	// Day は書き出し先の日付（YYYY-MM-DD）。空なら UTC の今日。
	Day string
}

// StartRun は実行の記録を始める。CLI の入口から 1 回だけ呼ぶ。
//
// Python 版は atexit で書き出していた。Go には atexit が無いので、呼び出し側が
// 入口で defer digest.Flush() を置く（コマンドごとの出口に散らすと、早期 return の
// 経路を必ず取りこぼす）。
func StartRun(opts StartOptions) {
	day := opts.Day
	if day == "" {
		day = clock.NowUTC().Format("2006-01-02")
	}
	mu.Lock()
	defer mu.Unlock()
	current = &run{
		app:     opts.App,
		env:     opts.Env,
		command: opts.Command,
		runID:   opts.RunID,
		path:    Path(opts.StateDir, opts.Env, day),
		started: time.Now(),
		outcome: "ok",
		fields:  map[string]any{},
	}
}

// Note はこの実行の集計に項目を足す（同じ鍵は上書き）。
//
// 数える対象は「後から見て意味のあるもの」だけにする。候補の件数、発注した件数、
// 見送った件数など。全部を載せるとログの写しになる。
func Note(fields map[string]any) {
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return
	}
	for key, value := range fields {
		current.fields[key] = value
	}
}

// Add は数える項目を足し込む（Note と違い加算）。
func Add(counters map[string]int) {
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return
	}
	for key, value := range counters {
		existing, _ := current.fields[key].(int)
		current.fields[key] = existing + value
	}
}

// Anomaly は異常を記録する。ここに載った実行だけを AI が深掘りする。
//
// 「いつもと違う」ことだけを載せる。時間帯の外で何もしなかった、同期して変化が
// 無かった——といった正常な結果は載せない。
func Anomaly(code, detail string) {
	mu.Lock()
	defer mu.Unlock()
	appendAnomaly(code, detail)
}

// appendAnomaly は mu を保持したまま呼ぶこと。
func appendAnomaly(code, detail string) {
	if current == nil {
		return
	}
	if detail != "" {
		current.anomalies = append(current.anomalies, code+": "+detail)
		return
	}
	current.anomalies = append(current.anomalies, code)
}

// Fail は実行が失敗したことを記録し、outcome を error にする。
func Fail(code, detail string) {
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return
	}
	current.outcome = "error"
	appendAnomaly(code, detail)
}

// Skipped は何もしなかった実行（休日・時間帯の外）。異常ではない。
func Skipped(reason string) {
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return
	}
	current.outcome = "skip"
	current.fields["reason"] = reason
}

// Flush は 1 行にして書き出す。2 回目以降は何もしない。
//
// ダイジェストは記録の付帯物なので、書き出しに失敗しても実行は落とさない
// （エラーは返すが、呼び出し側はログに落とすだけでよい）。
func Flush() error {
	mu.Lock()
	if current == nil || current.written {
		mu.Unlock()
		return nil
	}
	current.written = true
	record := map[string]any{
		"schema":  Schema,
		"ts_utc":  clock.StampISO(clock.UTC),
		"app":     current.app,
		"env":     current.env,
		"command": current.command,
		"run_id":  current.runID,
		"outcome": current.outcome,
		"dur_ms":  time.Since(current.started).Milliseconds(),
	}
	for key, value := range current.fields {
		record[key] = value
	}
	if len(current.anomalies) > 0 {
		record["anomalies"] = current.anomalies
	}
	path := current.path
	mu.Unlock()

	// encoding/json はマップの鍵を辞書順に書くので、Python 版の sort_keys と同じ
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// O_APPEND の 1 回の write は、他の実行と重なっても行が混ざらない
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Reset はテスト用。実行中の記録を捨てる。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	current = nil
}

// Active は記録中かどうか（テストと、呼び出し順の検査用）。
func Active() bool {
	mu.Lock()
	defer mu.Unlock()
	return current != nil && !current.written
}
