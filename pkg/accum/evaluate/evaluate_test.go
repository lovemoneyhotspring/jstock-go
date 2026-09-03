package evaluate

import (
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/shopspring/decimal"
)

func day(value string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", value, time.UTC)
	return t
}

// storeWith は temp ディレクトリに足を書いたストアを返す。
func storeWith(t *testing.T, symbol string, closes []float64) *data.BarStore {
	t.Helper()
	store := data.NewBarStore(t.TempDir())
	bars := make([]domain.Bar, 0, len(closes))
	d := day("2024-01-01")
	for _, c := range closes {
		price := decimal.NewFromFloat(c)
		bars = append(bars, domain.Bar{
			Symbol: symbol, Date: d.Format("2006-01-02"),
			Open: price, High: price, Low: price, Close: price, Volume: decimal.NewFromInt(1),
		})
		d = d.AddDate(0, 0, 1)
	}
	if err := store.Write(symbol, bars); err != nil {
		t.Fatal(err)
	}
	return store
}

func decisionFrame(rows []map[string]any) history.Frame {
	return history.NewFrame([]history.Column{
		{Name: "symbol", Type: history.TypeString},
		{Name: "judged_on", Type: history.TypeDate},
		{Name: "close", Type: history.TypeFloat64},
		{Name: "due", Type: history.TypeFloat64},
		{Name: "multiplier", Type: history.TypeFloat64},
	}, rows)
}

func TestBucketOfCutsAtTheBoundaries(t *testing.T) {
	cases := map[float64]string{
		0.5: Buckets[0], 1.0: Buckets[0],
		1.01: Buckets[1], 1.49: Buckets[1],
		1.5: Buckets[2], 1.99: Buckets[2],
		2.0: Buckets[3], 4.0: Buckets[3],
	}
	for multiplier, want := range cases {
		if got := BucketOf(multiplier); got != want {
			t.Errorf("%v: got %q want %q", multiplier, got, want)
		}
	}
}

func TestForwardCloseCountsBarsNotDays(t *testing.T) {
	bars := []domain.Bar{
		{Date: "2024-01-01", Close: decimal.NewFromInt(100)},
		{Date: "2024-01-05", Close: decimal.NewFromInt(110)},
		{Date: "2024-01-09", Close: decimal.NewFromInt(120)},
	}
	date, close, ok := ForwardClose(bars, "2024-01-01", 2)
	if !ok || date != "2024-01-09" || close != 120 {
		t.Errorf("got %v %v %v", date, close, ok)
	}
	if _, _, ok := ForwardClose(bars, "2024-01-01", 5); ok {
		t.Error("足が足りなければ見つからないはず")
	}
}

func TestEvaluateKeepsRowsWithoutForwardBars(t *testing.T) {
	store := storeWith(t, "1306.T", []float64{100, 101, 102})
	frame := decisionFrame([]map[string]any{
		{"symbol": "1306.T", "judged_on": day("2024-01-01"), "close": 100.0, "due": 1000.0, "multiplier": 1.0},
		{"symbol": "1306.T", "judged_on": day("2024-01-03"), "close": 102.0, "due": 2000.0, "multiplier": 2.0},
	})

	result := Evaluate(frame, store, 2)
	if result.Height() != 2 {
		t.Fatalf("実績が出ない判断も残すはず: %d 行", result.Height())
	}
	// 1 行目は 2 本先（2024-01-03、終値 102）が取れる
	if got := result.Rows[0]["ret_bp"]; got == nil {
		t.Fatal("1 行目は評価できるはず")
	} else if math.Abs(got.(float64)-200) > 1e-6 {
		t.Errorf("ret_bp: got %v want 200", got)
	}
	// 2 行目は先の足が無いので実績だけ欠損
	if result.Rows[1]["ret_bp"] != nil {
		t.Errorf("先の足が無い行は欠損のはず: %v", result.Rows[1]["ret_bp"])
	}
	if result.Rows[1]["bucket"] != Buckets[3] {
		t.Errorf("倍率 2.0 の帯が違う: %v", result.Rows[1]["bucket"])
	}
}

func TestSummarizeGroupsByBucketInOrder(t *testing.T) {
	store := storeWith(t, "1306.T", []float64{100, 90, 120})
	frame := decisionFrame([]map[string]any{
		{"symbol": "1306.T", "judged_on": day("2024-01-01"), "close": 100.0, "due": 1000.0, "multiplier": 1.0},
		{"symbol": "1306.T", "judged_on": day("2024-01-01"), "close": 100.0, "due": 3000.0, "multiplier": 4.0},
	})
	summary := Summarize(Evaluate(frame, store, 1))
	if summary.Height() != 2 {
		t.Fatalf("2 つの帯が出るはず: %d", summary.Height())
	}
	// Buckets の定義順（通常 → 増額）に並ぶ
	if summary.Rows[0]["bucket"] != Buckets[0] || summary.Rows[1]["bucket"] != Buckets[3] {
		t.Errorf("並び順が違う: %v", summary.ToMaps())
	}
	// 90/100 - 1 = -1000bp
	if got := summary.Rows[0]["avg_ret_bp"].(float64); math.Abs(got+1000) > 1e-6 {
		t.Errorf("平均リターン: got %v want -1000", got)
	}
	if got := summary.Rows[0]["win_rate"].(float64); got != 0 {
		t.Errorf("下げているので勝率 0: %v", got)
	}
	if got := summary.Rows[1]["due"].(float64); got != 3000 {
		t.Errorf("投下額の合計: got %v want 3000", got)
	}
}

func TestSummarizeOfEmptyHistoryIsEmpty(t *testing.T) {
	summary := Summarize(history.NewFrame([]history.Column{}, nil))
	if summary.Height() != 0 {
		t.Errorf("空の履歴からは空の集計: %v", summary.ToMaps())
	}
}
