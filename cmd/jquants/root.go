package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

var (
	appSettings = settings.LoadAppSettings()
	jquantsDir  = filepath.Join(appSettings.DataDir, "jquants")
)

// session はコマンド 1 回ぶんの土台（保管庫・台帳・ロガー・取り込み役）。
type session struct {
	ingestor *archive.Ingestor
	ledger   *archive.Ledger
	logger   *logging.Logger
	run      *cli.Run
	once     sync.Once
}

// close は台帳を閉じ、ダイジェストを書き出す。何度呼んでも 1 回しか畳まない。
func (s *session) close() {
	s.once.Do(func() {
		if s.ledger != nil {
			_ = s.ledger.Close()
		}
		s.run.Finish(nil)
	})
}

// newSession は保管庫・台帳・（要れば）API クライアントを組み立てる。
// status / check のように手元だけ見るコマンドは API キーが無くても動く。
func newSession(command string, needClient bool) (*session, error) {
	run := cli.StartRun("jquants", appSettings, command)
	runID, logger := run.RunID, run.Logger

	arch := archive.NewArchive(jquantsDir)
	ledger, err := archive.OpenLedger(arch.LedgerPath())
	if err != nil {
		run.Finish(err)
		return nil, err
	}
	var client archive.Client
	if needClient {
		jq, err := data.NewJQuantsClientFromEnv(appSettings.DotenvMap)
		if err != nil {
			_ = ledger.Close()
			run.Finish(err)
			return nil, err
		}
		client = jq
	}
	return &session{
		ingestor: archive.NewIngestor(client, arch, ledger, runID, logger),
		ledger:   ledger,
		logger:   logger,
		run:      run,
	}, nil
}

// resolveEndpoints は --only を端点に解決する。空なら全端点。
func resolveEndpoints(only []string) ([]archive.Endpoint, error) {
	if len(only) == 0 {
		return archive.StandardEndpoints, nil
	}
	out := make([]archive.Endpoint, 0, len(only))
	for _, name := range only {
		ep, err := archive.LookupEndpoint(name)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, nil
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
