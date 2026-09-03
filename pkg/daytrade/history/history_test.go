package history

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/shopspring/decimal"
)

func TestPlanFrames(t *testing.T) {
	vol := 0.02
	p := plan.Plan{
		Meta: plan.Meta{
			Day: "2026-09-03", PrevDay: "2026-09-02", Positions: 3,
			BudgetPerOrder: "666666", IVGate: "0", Candidates: 2, Eligible: 1,
			ShortEligible: 1, CreatedAt: "2026-09-02T11:30:00Z",
		},
		Candidates: []universe.Candidate{
			{Code: "10000", Symbol: "1000", Name: "甲", Segment: "prime",
				PrevClose: 1000, TurnoverMed: 5e8, MktCap: 9e11, Vol20: &vol,
				CapTercile: 3, Eligible: true, ShortEligible: true, Shortable: true},
			{Code: "20000", Symbol: "2000", Name: "乙", Segment: "growth", PrevClose: 500},
		},
	}
	frame, meta := PlanFrames(p)
	if frame.Height() != 2 || meta.Height() != 1 {
		t.Fatalf("行数 = %d / %d", frame.Height(), meta.Height())
	}
	// 列の型は日によらず揃っていないと、後から DuckDB で横断して読めない
	for _, column := range PlanSchema {
		if !frame.Has(column.Name) {
			t.Errorf("列 %s が無い", column.Name)
		}
	}
	if frame.Rows[0]["vol20"] != 0.02 {
		t.Errorf("vol20 = %v", frame.Rows[0]["vol20"])
	}
	// ボラの取れない銘柄は null（0 で埋めると重みが変わる）
	if frame.Rows[1]["vol20"] != nil {
		t.Errorf("ボラ無しが null でない: %v", frame.Rows[1]["vol20"])
	}
	if meta.Rows[0]["budget_per_order"] != 666666.0 {
		t.Errorf("budget_per_order = %v", meta.Rows[0]["budget_per_order"])
	}
	if _, ok := meta.Rows[0]["prev_day"].(time.Time); !ok {
		t.Errorf("prev_day が日付になっていない: %T", meta.Rows[0]["prev_day"])
	}
}

func TestQuotesFrame(t *testing.T) {
	at := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	received := map[string]selection.Quote{
		"1000": {Symbol: "1000", Price: decimal.NewFromInt(950), At: at, Source: "csv"},
		"2000": {Symbol: "2000", Price: decimal.NewFromInt(500), At: at, Source: "csv"},
	}
	usable := map[string]selection.Quote{"1000": received["1000"]}
	frame := QuotesFrame(received, usable, map[string]float64{"1000": 1000})
	if frame.Height() != 2 {
		t.Fatalf("行数 = %d", frame.Height())
	}
	// 銘柄順に並ぶ（実行のたびに順序が揺れないように）
	if frame.Rows[0]["symbol"] != "1000" {
		t.Errorf("並びが違う: %v", frame.Rows[0]["symbol"])
	}
	if frame.Rows[0]["usable"] != true || frame.Rows[1]["usable"] != false {
		t.Error("usable の印が付いていない")
	}
	if gap := frame.Rows[0]["gap"].(float64); gap < -0.051 || gap > -0.049 {
		t.Errorf("gap = %v, want ≈ -0.05", gap)
	}
	// 前日終値が無ければギャップも null（0 と混ぜない）
	if frame.Rows[1]["gap"] != nil {
		t.Errorf("前日終値なしのギャップ = %v", frame.Rows[1]["gap"])
	}
}

func TestRankingFrameMarksPicks(t *testing.T) {
	ranked := []selection.Ranked{
		{Rank: 1, Symbol: "1000", Code: "10000", PrevClose: decimal.NewFromInt(1000),
			Price: decimal.NewFromInt(950), Gap: decimal.RequireFromString("-0.05")},
		{Rank: 2, Symbol: "2000", Code: "20000", PrevClose: decimal.NewFromInt(1000),
			Price: decimal.NewFromInt(980), Gap: decimal.RequireFromString("-0.02")},
	}
	picks := []selection.Pick{{
		Symbol: "1000", Price: decimal.NewFromInt(950),
		Quantity: decimal.NewFromInt(100), Side: domain.SideBuy,
	}}
	frame := RankingFrame(ranked, picks, "BUY", 1, decimal.NewFromInt(100_000))
	// 順位表は**全行**を持つ（「なぜ X が選ばれなかったか」を後から追うため）
	if frame.Height() != 2 {
		t.Fatalf("行数 = %d, want 2", frame.Height())
	}
	if frame.Rows[0]["picked"] != true || frame.Rows[1]["picked"] != false {
		t.Error("選定の印が付いていない")
	}
	if frame.Rows[0]["quantity"] != 100.0 || frame.Rows[0]["amount"] != 95000.0 {
		t.Errorf("株数・金額 = %v / %v", frame.Rows[0]["quantity"], frame.Rows[0]["amount"])
	}
	if frame.Rows[1]["quantity"] != nil {
		t.Errorf("選ばれていない行に株数が入っている: %v", frame.Rows[1]["quantity"])
	}
}

func TestOpenRunFrameCoercesAndDropsUnknown(t *testing.T) {
	frame := OpenRunFrame(map[string]any{
		"mode": "dry_run", "outcome": "picked",
		"quotes_requested": 100, "budget": decimal.NewFromInt(666_666),
		"scale": 0.5, "trade": true,
		"知らない項目": "捨てられるはず",
	})
	if frame.Height() != 1 {
		t.Fatalf("行数 = %d", frame.Height())
	}
	row := frame.Rows[0]
	if row["mode"] != "dry_run" || row["outcome"] != "picked" {
		t.Errorf("文字列が入っていない: %+v", row)
	}
	// int も Decimal も列の型に寄せる（型が揺れると union_by_name が破綻する）
	if row["quotes_requested"] != int64(100) {
		t.Errorf("quotes_requested = %v (%T)", row["quotes_requested"], row["quotes_requested"])
	}
	if row["budget"] != 666666.0 {
		t.Errorf("budget = %v (%T)", row["budget"], row["budget"])
	}
	if _, ok := row["知らない項目"]; ok {
		t.Error("スキーマに無い項目が残っている")
	}
	// 指定しなかった列は null で揃う
	if v, ok := row["vix"]; !ok || v != nil {
		t.Errorf("未指定の列が null でない: %v", v)
	}
}

func TestAppendAndReadRoundTrip(t *testing.T) {
	store := history.NewStore(t.TempDir())
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	frame := OpenRunFrame(map[string]any{"mode": "live", "outcome": "picked", "n": 3})
	if _, err := store.Append(KindOpenRun, frame, day, history.AppendOptions{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(KindOpenRun, history.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Height() != 1 {
		t.Fatalf("読み戻し %d 行", got.Height())
	}
	if got.Rows[0]["mode"] != "live" || got.Rows[0]["run_id"] != "r1" {
		t.Errorf("往復で値が変わった: %+v", got.Rows[0])
	}
}
