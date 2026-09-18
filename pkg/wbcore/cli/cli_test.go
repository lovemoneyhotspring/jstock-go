package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

func TestYen(t *testing.T) {
	cases := map[any]string{
		decimal.NewFromInt(1234567): "1,234,567",
		-1234.6:                     "-1,235",
		12:                          "12",
		int64(1000):                 "1,000",
	}
	for in, want := range cases {
		if got := Yen(in); got != want {
			t.Errorf("Yen(%v) = %q, want %q", in, got, want)
		}
	}
	if got := Yen((*decimal.Decimal)(nil)); got != "" {
		t.Errorf("nil は空: %q", got)
	}
}

func TestParseDay(t *testing.T) {
	day, err := ParseDay("2026-09-03")
	if err != nil || !day.Equal(time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("ParseDay: %v %v", day, err)
	}
	if zero, err := ParseDay(""); err != nil || !zero.IsZero() {
		t.Errorf("空はゼロ値: %v %v", zero, err)
	}
	if _, err := ParseDay("2026/09/03"); err == nil {
		t.Error("形式違いはエラー")
	}
}

func TestDashAndPct(t *testing.T) {
	if Dash("  ") != "—" || Dash("x") != "x" {
		t.Error("Dash")
	}
	if Pct(0.0123) != "+1.23%" {
		t.Errorf("Pct: %s", Pct(0.0123))
	}
}

func TestConfirmLiveSkipsWhenNotNeeded(t *testing.T) {
	uat := &settings.AppSettings{Env: settings.EnvUAT}
	prod := &settings.AppSettings{Env: settings.EnvProd}
	if err := ConfirmLive(uat, true, false); err != nil {
		t.Errorf("UAT は聞かない: %v", err)
	}
	if err := ConfirmLive(prod, false, false); err != nil {
		t.Errorf("dry-run は聞かない: %v", err)
	}
	if err := ConfirmLive(prod, true, true); err != nil {
		t.Errorf("--yes は聞かない: %v", err)
	}
}

// 本番 × --live × --yes 無しは、非対話なら止まり、対話なら y のときだけ通る。
func TestConfirmLiveInteractive(t *testing.T) {
	prod := &settings.AppSettings{Env: settings.EnvProd}
	oldTTY, oldIn := stdinIsTerminal, confirmInput
	t.Cleanup(func() { stdinIsTerminal, confirmInput = oldTTY, oldIn })

	stdinIsTerminal = func() bool { return false }
	if err := ConfirmLive(prod, true, false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("非対話は --yes を求めるエラー: %v", err)
	}

	stdinIsTerminal = func() bool { return true }
	confirmInput = strings.NewReader("y\n")
	if err := ConfirmLive(prod, true, false); err != nil {
		t.Errorf("y は通る: %v", err)
	}
	confirmInput = strings.NewReader("n\n")
	if err := ConfirmLive(prod, true, false); err == nil {
		t.Error("n は中止")
	}
	confirmInput = strings.NewReader("")
	if err := ConfirmLive(prod, true, false); err == nil {
		t.Error("空入力は中止")
	}
}

// startRun はテスト用に一時ディレクトリで Run を起こし、ログとダイジェストの場所を返す。
func startRun(t *testing.T, app string) (*Run, string, string) {
	t.Helper()
	t.Cleanup(digest.Reset)
	t.Cleanup(logging.ResetRunContext)
	s := &settings.AppSettings{Env: settings.EnvUAT, StateDir: t.TempDir(), LogDir: t.TempDir()}
	run := StartRun(app, s, "run")
	if run.RunID == "" || run.Logger == nil {
		t.Fatal("run_id とロガーが要る")
	}
	return run, filepath.Join(s.LogDir, app+"-uat.jsonl"), filepath.Join(s.StateDir, "digest")
}

func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s を読めない: %v", path, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("壊れた行 %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// Finish はダイジェストを 1 行書き、err があれば outcome を error にする。2 回呼んでも増えない。
func TestFinishWritesDigest(t *testing.T) {
	run, _, digestDir := startRun(t, "wbjp")
	run.Finish(errors.New("boom"))
	run.Finish(nil)

	files, _ := filepath.Glob(filepath.Join(digestDir, "uat-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("ダイジェスト = %v, want 1 ファイル", files)
	}
	records := readJSONL(t, files[0])
	if len(records) != 1 {
		t.Fatalf("行数 = %d, want 1", len(records))
	}
	rec := records[0]
	if rec["outcome"] != "error" || rec["run_id"] != run.RunID || rec["app"] != "wbjp" {
		t.Errorf("ダイジェスト = %v", rec)
	}
	if anomalies, _ := rec["anomalies"].([]any); len(anomalies) != 1 || anomalies[0] != "wbjp.command_failed: boom" {
		t.Errorf("anomalies = %v", rec["anomalies"])
	}
	if digest.Active() {
		t.Error("Finish の後も記録中になっている")
	}
	// nil の Run は何もしない
	var none *Run
	none.Finish(errors.New("x"))
}

// Alert は届かなかったとき警告ログを残す。届けば残さない。
func TestAlertLogsWhenUndelivered(t *testing.T) {
	run, logPath, _ := startRun(t, "accum")
	delivered := true
	run.Alerter = func(_, _ string, _ *logging.Logger) bool { return delivered }
	run.Alert("届く", "本文")
	delivered = false
	run.Alert("届かない", "本文")
	run.Finish(nil)

	var warned []string
	for _, rec := range readJSONL(t, logPath) {
		if rec["code"] == "cli.alert_undelivered" {
			extra, _ := rec["extra"].(map[string]any)
			warned = append(warned, fmt.Sprint(extra["title"]))
		}
	}
	if len(warned) != 1 || warned[0] != "届かない" {
		t.Errorf("届かなかった通知の警告 = %v, want [届かない]", warned)
	}
	// nil の Run は何もしない
	var none *Run
	none.Alert("x", "y")
}

// SetVerify は Run・ロガー・ダイジェストの 3 つに印を付ける。
func TestSetVerifyMarksEverything(t *testing.T) {
	run, logPath, digestDir := startRun(t, "daytrade")
	run.SetVerify(true)
	run.Info("daytrade.test", "印の確認")
	run.Finish(nil)

	if !run.Verify {
		t.Error("Run に印が無い")
	}
	files, _ := filepath.Glob(filepath.Join(digestDir, "uat-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("ダイジェスト = %v", files)
	}
	if v, _ := readJSONL(t, files[0])[0]["verify"].(bool); !v {
		t.Error("ダイジェストに verify が無い")
	}
	found := false
	for _, rec := range readJSONL(t, logPath) {
		if rec["code"] == "daytrade.test" {
			found = true
			if v, _ := rec["verify"].(bool); !v {
				t.Errorf("ログの行に verify が無い: %v", rec)
			}
		}
	}
	if !found {
		t.Error("ログに daytrade.test が無い")
	}
	// nil でも落ちない
	var none *Run
	none.SetVerify(true)
}

func TestCrashRecordsAndAlerts(t *testing.T) {
	defer digest.Reset()
	defer logging.ResetRunContext()
	s := &settings.AppSettings{Env: settings.EnvUAT, StateDir: t.TempDir(), LogDir: t.TempDir()}
	run := StartRun("wbjp", s, "run")
	if run.RunID == "" || run.Logger == nil {
		t.Fatal("run_id とロガーが要る")
	}
	var got string
	run.Alerter = func(title, body string, _ *logging.Logger) bool {
		got = title + "|" + body
		return true
	}
	if run.Crash("日次実行", "wbjp.crash", nil) != nil {
		t.Error("nil はそのまま")
	}
	err := errors.New("boom")
	if run.Crash("日次実行", "wbjp.crash", err) != err {
		t.Error("元のエラーを返す")
	}
	if got != "wbjp: 日次実行が異常終了|boom" {
		t.Errorf("通知の内容: %q", got)
	}
	run.Finish(err)
}

func TestConnectBrokerPaper(t *testing.T) {
	b, err := ConnectBroker("paper", &settings.AppSettings{Env: settings.EnvUAT})
	if err != nil || b.Name() != "paper" {
		t.Fatalf("paper: %v %v", b, err)
	}
	if _, err := ConnectBroker("webull", &settings.AppSettings{Env: settings.EnvUAT}); err == nil {
		t.Error("未知の broker はエラー")
	}
}

// 3 桁区切りは整数部だけ。小数点ごと数えると建玉の加重平均（2868.17）が
// 「2,868,.17」になり、値段として読めなくなる。
func TestAddCommasKeepsFraction(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"2868.17", "2,868.17"},
		{"1923.56", "1,923.56"},
		{"-60800", "-60,800"},
		{"-1234.5", "-1,234.5"},
		{"999", "999"},
		{"1000", "1,000"},
		{"0.5", "0.5"},
	} {
		if got := AddCommas(c.in); got != c.want {
			t.Errorf("AddCommas(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
