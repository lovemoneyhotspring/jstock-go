package evaluate

import (
	"testing"
	"time"

	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
)

// TestReviewCountsLatestEvaluationOnly は、同じ判断日を晩ごとに評価し直して履歴に積んでも、
// Review の件数が増えず、最後の評価だけを数えること（2026-09-24 のレビュー）。
func TestReviewCountsLatestEvaluationOnly(t *testing.T) {
	columns := append([]corehistory.Column{
		{Name: "day", Type: corehistory.TypeDate},
		{Name: "recorded_at", Type: corehistory.TypeTimestamp},
	}, EvaluationColumns...)
	first := time.Date(2026, 9, 1, 11, 25, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 1)
	frame := corehistory.NewFrame(columns, []map[string]any{
		{"day": day("2026-08-01"), "recorded_at": first, "horizon": int64(20), "group": "adopted", "ret_bp": 100.0},
		{"day": day("2026-08-01"), "recorded_at": first, "horizon": int64(20), "group": "rest", "ret_bp": 0.0},
		{"day": day("2026-08-01"), "recorded_at": second, "horizon": int64(20), "group": "adopted", "ret_bp": 300.0},
		{"day": day("2026-08-01"), "recorded_at": second, "horizon": int64(20), "group": "rest", "ret_bp": 0.0},
		// horizon が違えば別の評価として残る
		{"day": day("2026-08-01"), "recorded_at": first, "horizon": int64(5), "group": "adopted", "ret_bp": 50.0},
	})

	table := Review(frame)
	if table.Height() != 2 {
		t.Fatalf("(判断日, horizon) ごとに 1 行のはず: %d", table.Height())
	}
	for _, row := range table.Rows {
		if row["horizon"] == int64(20) && (row["adopted"] != int64(1) || row["adopted_bp"] != 300.0) {
			t.Errorf("最後の評価だけを数えるはず: %v", row)
		}
	}

	done := EvaluatedDays(frame, 20)
	if !done[day("2026-08-01")] || len(done) != 1 {
		t.Errorf("評価済みの日: %v", done)
	}
	if len(EvaluatedDays(frame, 10)) != 0 {
		t.Error("別の horizon を評価済みとみなした")
	}
}
