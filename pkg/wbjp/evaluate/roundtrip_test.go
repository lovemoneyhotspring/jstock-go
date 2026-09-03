package evaluate

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	wbjphistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/history"
)

// screen が積んだ Parquet をそのまま評価できるかを、書いて読んで確かめる。
// 単体の Frame だけで通しても、Parquet の型が揺れていれば運用では動かない。
func TestEvaluateFromStoredScreen(t *testing.T) {
	store := corehistory.NewStore(t.TempDir())
	entry := day("2026-01-05")

	frame := wbjphistory.ScreenFrame(
		[]domain.CombinedSignal{
			{Symbol: "A", Direction: 0.9},
			{Symbol: "B", Direction: 0.6},
			{Symbol: "C", Direction: 0.1},
		},
		wbjphistory.ScreenOptions{
			Threshold:    0.5,
			MaxPositions: 1,
			Combiner:     "weighted",
			Meta: map[string]map[string]any{
				"A": {"close": 100.0},
				"B": {"close": 200.0},
				"C": {"close": 300.0},
			},
		},
	)
	if _, err := store.Append(wbjphistory.Kind, frame, entry,
		corehistory.AppendOptions{RunID: "screen-1"}); err != nil {
		t.Fatalf("履歴を書けません: %v", err)
	}

	screens, err := store.Latest(wbjphistory.Kind, entry)
	if err != nil {
		t.Fatalf("履歴を読めません: %v", err)
	}
	if screens.Height() != 3 {
		t.Fatalf("3 行のはず: %d", screens.Height())
	}

	bars := fakeBars{
		"A": {bar("A", "2026-01-05", 100), bar("A", "2026-01-06", 110)},
		"B": {bar("B", "2026-01-05", 200), bar("B", "2026-01-06", 200)},
		"C": {bar("C", "2026-01-05", 300), bar("C", "2026-01-06", 285)},
	}
	result := Evaluate(screens, bars, entry, 1)

	if result.Rows[0]["group"] != "adopted" || result.Rows[0]["symbol"] != "A" {
		t.Errorf("採用の印が往復で失われている: %+v", result.Rows[0])
	}
	if result.Rows[1]["group"] != "passed" || result.Rows[2]["group"] != "rest" {
		t.Errorf("群の判定が往復で失われている: %v / %v", result.Rows[1], result.Rows[2])
	}
	if result.Rows[0]["screen_run_id"] != "screen-1" {
		t.Errorf("判断の実行 ID が繋がっていない: %v", result.Rows[0]["screen_run_id"])
	}
	if !nearly(result.Rows[0]["ret_bp"], 1000) {
		t.Errorf("+10%% は 1000 bp: %v", result.Rows[0]["ret_bp"])
	}

	// 評価の履歴も書けて読み戻せる（review の材料になる）
	if _, err := store.Append(Kind, result, entry, corehistory.AppendOptions{RunID: "eval-1"}); err != nil {
		t.Fatalf("評価を書けません: %v", err)
	}
	stored, err := store.Read(Kind, corehistory.Range{})
	if err != nil {
		t.Fatalf("評価を読めません: %v", err)
	}
	table := Review(stored)
	if table.Height() != 1 {
		t.Fatalf("1 日ぶんのはず: %d", table.Height())
	}
	row := table.Rows[0]
	if row["adopted"] != int64(1) {
		t.Errorf("採用の件数が違う: %v", row["adopted"])
	}
	if !nearly(row["adopted_bp"], 1000) || !nearly(row["rest_bp"], -500) {
		t.Errorf("日別の平均が違う: %+v", row)
	}

	totals := ReviewTotals(table)
	if totals.Rows[0]["beat_rest_rate"] != 1.0 {
		t.Errorf("採用が圏外を上回った日の割合が違う: %v", totals.Rows[0])
	}
}
