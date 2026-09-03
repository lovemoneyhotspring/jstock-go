package history

import (
	"testing"
	"time"

	wbhistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/shopspring/decimal"
)

func sample() Decision {
	return Decision{
		Symbol:     "1306.T",
		Market:     "JP",
		JudgedOn:   "2026-09-02",
		Month:      "2026-09-01",
		Close:      decimal.NewFromFloat(2500.5),
		Due:        decimal.NewFromInt(25_000),
		Target:     decimal.NewFromInt(25_000),
		Placed:     decimal.Zero,
		Multiplier: 2.0,
		Tactic:     "bear_stack(x2)",
		Reason:     "入金日 25000 円",
	}
}

func TestDecisionFrameKeepsShapeWithoutRows(t *testing.T) {
	frame := DecisionFrame(nil)
	if frame.Height() != 0 {
		t.Errorf("0 件のはず: %d", frame.Height())
	}
	if frame.Width() != len(DecisionColumns) {
		t.Errorf("列は保つはず: %d", frame.Width())
	}
	for _, want := range []string{"symbol", "judged_on", "multiplier", "due"} {
		if !frame.Has(want) {
			t.Errorf("列 %s が無い", want)
		}
	}
}

func TestDecisionFrameConvertsAmountsToFloat(t *testing.T) {
	frame := DecisionFrame([]Decision{sample()})
	row := frame.Rows[0]
	if got, ok := row["due"].(float64); !ok || got != 25_000 {
		t.Errorf("due: got %v", row["due"])
	}
	if got, ok := row["close"].(float64); !ok || got != 2500.5 {
		t.Errorf("close: got %v", row["close"])
	}
	judged, ok := row["judged_on"].(time.Time)
	if !ok || judged.Format("2006-01-02") != "2026-09-02" {
		t.Errorf("judged_on: got %v", row["judged_on"])
	}
	if row["multiplier"].(float64) != 2.0 {
		t.Errorf("multiplier: got %v", row["multiplier"])
	}
}

func TestDecisionFrameLeavesBadDatesMissing(t *testing.T) {
	d := sample()
	d.JudgedOn = ""
	d.Month = "2026-09"
	frame := DecisionFrame([]Decision{d})
	if frame.Rows[0]["judged_on"] != nil {
		t.Errorf("空の日付は欠損のはず: %v", frame.Rows[0]["judged_on"])
	}
	if frame.Rows[0]["month"] != nil {
		t.Errorf("不正な日付は欠損のはず: %v", frame.Rows[0]["month"])
	}
}

// 追記した判断が同じ形で読み戻せること（Parquet の往復）。
func TestAppendAndReadRoundTrip(t *testing.T) {
	store := wbhistory.NewStore(t.TempDir())
	day, _ := time.ParseInLocation("2006-01-02", "2026-09-02", time.UTC)
	if _, err := store.Append(Kind, DecisionFrame([]Decision{sample()}), day, wbhistory.AppendOptions{RunID: "test"}); err != nil {
		t.Fatal(err)
	}
	frame, err := store.Read(Kind, wbhistory.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Height() != 1 {
		t.Fatalf("1 行のはず: %d", frame.Height())
	}
	row := frame.Rows[0]
	if row["symbol"] != "1306.T" {
		t.Errorf("symbol: %v", row["symbol"])
	}
	if row["run_id"] != "test" {
		t.Errorf("run_id が付くはず: %v", row["run_id"])
	}
	if got, ok := row["multiplier"].(float64); !ok || got != 2.0 {
		t.Errorf("multiplier: %v", row["multiplier"])
	}
}
