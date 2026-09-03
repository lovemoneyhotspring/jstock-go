package cli

import (
	"errors"
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
	// 本番 × --live × --yes 無し × 非対話（テストの stdin）は止まる
	if err := ConfirmLive(prod, true, false); err == nil {
		t.Error("非対話の本番発注は --yes 無しで通ってはいけない")
	}
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
