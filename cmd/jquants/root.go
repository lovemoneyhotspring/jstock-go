package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

var (
	appSettings = settings.LoadAppSettings()
	jquantsDir  = filepath.Join(appSettings.DataDir, "jquants")
	// run はこの実行の記録（run_id・ログ・ダイジェスト）。newSession が起こし、main が
	// コマンドの結果を付けて Finish する。cli.Guarded が panic の記録にも使う。
	// 台帳を使わないコマンド（query）では nil のまま（Finish も Guarded も nil で動く）。
	run *cli.Run
)

// session はコマンド 1 回ぶんの土台（保管庫・台帳・ロガー・取り込み役）。
type session struct {
	ingestor *archive.Ingestor
	ledger   *archive.Ledger
	logger   *logging.Logger
}

// close は台帳を閉じる。何度呼んでもよい。ダイジェストは main が結果を付けて書き出す
// （以前はここで Finish(nil) を固定で呼び、失敗した回もダイジェストでは ok になっていた）。
func (s *session) close() {
	if s.ledger != nil {
		_ = s.ledger.Close()
		s.ledger = nil
	}
}

// exitError は終了コードつきの失敗。表示と通知は RunE の中で済んでいるので、main は
// 印字せず、ダイジェストに失敗として残してからその終了コードで終える。
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitWith は終了コード code の失敗を返す。os.Exit を RunE の中で呼ぶと defer と
// ダイジェストの書き出しを飛ばすので、終了は main に任せる。
func exitWith(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

// noteIngests は取り込みと失敗の件数をダイジェストに足す。
func noteIngests(ingests []archive.Ingest, failures []archive.Failure) {
	rows := 0
	for _, r := range ingests {
		rows += r.Rows
	}
	digest.Add(map[string]int{"ingests": len(ingests), "rows": rows, "failures": len(failures)})
}

// failureSummary は失敗の要約（件数と先頭の数件）。ダイジェストと終了時のエラーに使う。
func failureSummary(failures []archive.Failure) string {
	const head = 3
	parts := make([]string, 0, head)
	for i, f := range failures {
		if i == head {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s: %s", f.Endpoint, f.Target, f.Error))
	}
	text := fmt.Sprintf("%d 件の取り込みに失敗（%s", len(failures), strings.Join(parts, " / "))
	if len(failures) > head {
		text += fmt.Sprintf(" ほか %d 件", len(failures)-head)
	}
	return text + "）"
}

// staleRows は古い端点を表の行（端点\t説明）と通知の行（端点: 説明）にする。check と repair で共有。
func staleRows(stale []archive.Stale) (table, lines []string) {
	for _, st := range stale {
		text := fmt.Sprintf("最終取得 %s（%.0f 日を超えて古い）",
			clock.Fmt(st.LastFetched, clock.Tokyo, false), st.Limit.Hours()/24)
		if st.LastFetched.IsZero() {
			text = "一度も取っていません"
		}
		table = append(table, fmt.Sprintf("%s\t%s", st.Endpoint.Path, text))
		lines = append(lines, fmt.Sprintf("%s: %s", st.Endpoint.Path, text))
	}
	return table, lines
}

// newClient は API クライアントを組み立てる。試験ではスタブに差し替える（本番の API を叩かない）。
var newClient = func() (archive.Client, error) {
	return data.NewJQuantsClientFromEnv(appSettings.DotenvMap)
}

// newSession は保管庫・台帳・（要れば）API クライアントを組み立てる。
// status / check のように手元だけ見るコマンドは API キーが無くても動く。
func newSession(command string, needClient bool) (*session, error) {
	// 失敗も execute の Finish がダイジェストに残す（ここで畳まない）
	run = cli.StartRun("jquants", appSettings, command)
	runID, logger := run.RunID, run.Logger

	arch := archive.NewArchive(jquantsDir)
	ledger, err := archive.OpenLedger(arch.LedgerPath())
	if err != nil {
		return nil, err
	}
	var client archive.Client
	if needClient {
		if client, err = newClient(); err != nil {
			_ = ledger.Close()
			return nil, err
		}
	}
	return &session{
		ingestor: archive.NewIngestor(client, arch, ledger, runID, logger),
		ledger:   ledger,
		logger:   logger,
	}, nil
}

// printIngests は取り込み結果を表で出す。
func printIngests(ingests []archive.Ingest, title string) {
	if len(ingests) == 0 {
		fmt.Printf("%s: やることはありません（すべて最新）\n", title)
		return
	}
	fmt.Println(title)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "端点\t対象\t経路\t件数\t変化")
	for _, r := range ingests {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\n", r.Endpoint, r.Target, r.Source, r.Rows, r.Changed)
	}
	w.Flush()
}

// printFailures は失敗を並べる。1 件でもあれば true。
func printFailures(failures []archive.Failure, note string) bool {
	if len(failures) == 0 {
		return false
	}
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "%s %s: %s\n", f.Endpoint, f.Target, f.Error)
	}
	fmt.Fprintf(os.Stderr, "%d 件の取り込みに失敗しました（%s）\n", len(failures), note)
	return true
}
