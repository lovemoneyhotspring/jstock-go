package repo

import (
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func openTemp(t *testing.T) *Repo {
	t.Helper()
	r, err := OpenRepo(filepath.Join(t.TempDir(), "wbjp-test.db"))
	if err != nil {
		t.Fatalf("台帳を開けません: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestGetRunMissingIsNil(t *testing.T) {
	r := openTemp(t)
	got, err := r.GetRun("いない")
	if err != nil {
		t.Fatalf("見つからないのは失敗ではない: %v", err)
	}
	if got != nil {
		t.Errorf("nil のはず: %+v", got)
	}
}

func TestGetRunAndRecentRuns(t *testing.T) {
	r := openTemp(t)
	for _, id := range []string{"run-1", "run-2", "run-3"} {
		if err := r.StartRun(id, "2026-01-05", "uat", "dry-run"); err != nil {
			t.Fatalf("記録できません: %v", err)
		}
	}
	equity := decimal.NewFromInt(1234)
	if err := r.FinishRun("run-2", "ok", &equity, nil, nil); err != nil {
		t.Fatalf("終了を書けません: %v", err)
	}

	got, err := r.GetRun("run-2")
	if err != nil || got == nil {
		t.Fatalf("引けません: %v %v", got, err)
	}
	if got.Status != "ok" || got.Equity == nil || !got.Equity.Equal(equity) {
		t.Errorf("実行の中身が違う: %+v", got)
	}

	recent, err := r.RecentRuns(2)
	if err != nil {
		t.Fatalf("一覧できません: %v", err)
	}
	if len(recent) != 2 {
		t.Errorf("limit が効いていない: %d", len(recent))
	}
}

func TestExplainCollectsSections(t *testing.T) {
	r := openTemp(t)
	if err := r.StartRun("run-1", "2026-01-05", "uat", "live"); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordSignals("run-1", []domain.Signal{
		{Strategy: "sma_cross", Symbol: "7203", Direction: 0.8, Confidence: 0.5, Reason: "上抜け"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordRiskEvents("run-1", map[string]string{"6758": "上限超過"}); err != nil {
		t.Fatal(err)
	}

	sections, err := r.Explain("run-1")
	if err != nil {
		t.Fatalf("explain できません: %v", err)
	}
	// 判断の流れの順に並ぶ
	want := []string{"signals", "combined_signals", "targets", "orders", "risk_events"}
	if len(sections) != len(want) {
		t.Fatalf("区画の数が違う: %d", len(sections))
	}
	for i, name := range want {
		if sections[i].Name != name {
			t.Errorf("%d 番目は %s のはず: %s", i, name, sections[i].Name)
		}
	}
	if len(sections[0].Rows) != 1 || sections[0].Rows[0]["symbol"] != "7203" {
		t.Errorf("シグナルが取れていない: %+v", sections[0].Rows)
	}
	if len(sections[4].Rows) != 1 || sections[4].Rows[0]["reason"] != "上限超過" {
		t.Errorf("拒否理由が取れていない: %+v", sections[4].Rows)
	}
	// 他の実行の記録は混ざらない
	if len(sections[3].Rows) != 0 {
		t.Errorf("注文は無いはず: %+v", sections[3].Rows)
	}
}

func TestRecordRiskEventsEmptyIsNoop(t *testing.T) {
	r := openTemp(t)
	if err := r.RecordRiskEvents("いない", nil); err != nil {
		t.Errorf("0 件なら何もしないはず: %v", err)
	}
	if err := r.RecordSnapshot("いない", "2026-01-05", nil); err != nil {
		t.Errorf("0 件なら何もしないはず: %v", err)
	}
}

func TestRecordSnapshot(t *testing.T) {
	r := openTemp(t)
	if err := r.StartRun("run-1", "2026-01-05", "uat", "live"); err != nil {
		t.Fatal(err)
	}
	positions := []domain.Position{{
		Symbol:    "7203",
		Quantity:  decimal.NewFromInt(100),
		CostPrice: decimal.NewFromInt(2000),
		LastPrice: decimal.NewFromInt(2100),
	}}
	if err := r.RecordSnapshot("run-1", "2026-01-05", positions); err != nil {
		t.Fatalf("建玉を残せません: %v", err)
	}

	var count int
	if err := r.db.QueryRow(
		"SELECT COUNT(*) FROM position_snapshots WHERE run_id = ?;", "run-1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("1 件のはず: %d", count)
	}
}
