package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

// failingClient は API をすべて失敗させるスタブ（本番の API を叩かない）。
type failingClient struct{}

func (failingClient) GetAll(string, map[string]string) ([]map[string]any, error) {
	return nil, errors.New("わざと失敗")
}
func (failingClient) BulkList(string) ([]map[string]any, error) {
	return nil, errors.New("わざと一覧で失敗")
}
func (failingClient) BulkDownload(string) ([]byte, error) { return nil, errors.New("わざと失敗") }

// sandbox は保管庫・state・ログを一時ディレクトリに向け、API をスタブに差し替える。
func sandbox(t *testing.T, client archive.Client) (stateDir string) {
	t.Helper()
	stateDir = t.TempDir()
	oldState, oldLog, oldDir, oldClient := appSettings.StateDir, appSettings.LogDir, jquantsDir, newClient
	appSettings.StateDir, appSettings.LogDir = stateDir, filepath.Join(stateDir, "logs")
	jquantsDir = filepath.Join(t.TempDir(), "jquants")
	newClient = func() (archive.Client, error) { return client, nil }
	t.Setenv("WBJP_ALERT_CHANNEL_ID", "") // 通知は送らない
	t.Setenv("WBJP_STATE_DIR", stateDir)  // 通知の控えも一時ディレクトリへ
	t.Cleanup(func() {
		appSettings.StateDir, appSettings.LogDir, jquantsDir, newClient = oldState, oldLog, oldDir, oldClient
		digest.Reset()
		logging.ResetRunContext()
	})
	return stateDir
}

// readDigest はダイジェストの最後の行を読む。
func readDigest(t *testing.T, stateDir string) map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(stateDir, "digest", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("ダイジェスト = %v, want 1 ファイル", files)
	}
	f, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var last map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		last = nil
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			t.Fatal(err)
		}
	}
	return last
}

// 取り込みに失敗した sync は終了コード 1 で、ダイジェストでも error になる。
// 以前は session.close が Finish(nil) 固定で、失敗した回もダイジェストでは ok だった（レビュー N3）。
func TestSyncFailureIsRecordedInDigest(t *testing.T) {
	stateDir := sandbox(t, failingClient{})
	if code := execute([]string{"sync", "--only", "markets_calendar"}); code != 1 {
		t.Fatalf("終了コード = %d, want 1", code)
	}
	rec := readDigest(t, stateDir)
	if rec["outcome"] != "error" || rec["command"] != "sync" {
		t.Fatalf("ダイジェスト = %v, want outcome=error", rec)
	}
	anomalies, _ := rec["anomalies"].([]any)
	if len(anomalies) == 0 || !strings.Contains(anomalies[0].(string), "1 件の取り込みに失敗") {
		t.Errorf("anomalies = %v", rec["anomalies"])
	}
	if rec["failures"] != float64(1) {
		t.Errorf("failures = %v, want 1", rec["failures"])
	}
}

// 失敗の無い回は ok のまま。
func TestStatusIsOK(t *testing.T) {
	stateDir := sandbox(t, failingClient{})
	if code := execute([]string{"status"}); code != 0 {
		t.Fatalf("status の終了コード = %d", code)
	}
	if rec := readDigest(t, stateDir); rec["outcome"] != "ok" {
		t.Fatalf("ダイジェスト = %v, want ok", rec)
	}
}

// repair は終わりに全件・範囲で取る端点の鮮度も見る。古ければ終了コード 3（exitGaps。panic の 2 と分ける）で、ダイジェストも error。
// 以前は 20:00 の cron を check から repair に替えたとき（0134076）に鮮度の監視が抜けていた（レビュー N4）。
func TestRepairReportsStaleEndpoints(t *testing.T) {
	stateDir := sandbox(t, failingClient{})
	if code := execute([]string{"repair", "--only", "markets_calendar"}); code != exitGaps {
		t.Fatalf("終了コード = %d, want 3（一度も取っていない取引カレンダーは古い）", code)
	}
	rec := readDigest(t, stateDir)
	anomalies, _ := rec["anomalies"].([]any)
	if rec["outcome"] != "error" || len(anomalies) == 0 ||
		!strings.Contains(anomalies[0].(string), "/markets/calendar: 一度も取っていません") {
		t.Fatalf("ダイジェスト = %v", rec)
	}
}

// panic は cli.Guarded が受け、ExitPanic で終わる。ダイジェストにも残る。
func TestPanicIsGuarded(t *testing.T) {
	stateDir := sandbox(t, nil)
	newClient = func() (archive.Client, error) { panic("ぬるぽ") }
	if code := execute([]string{"sync"}); code != 2 {
		t.Fatalf("終了コード = %d, want 2（cli.ExitPanic）", code)
	}
	rec := readDigest(t, stateDir)
	if rec["outcome"] != "error" {
		t.Fatalf("ダイジェスト = %v", rec)
	}
	anomalies, _ := rec["anomalies"].([]any)
	if len(anomalies) == 0 || !strings.HasPrefix(anomalies[0].(string), "jquants.panic: panic: ぬるぽ") {
		t.Errorf("anomalies = %v", anomalies)
	}
}
