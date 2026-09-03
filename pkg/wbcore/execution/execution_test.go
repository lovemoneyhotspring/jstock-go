package execution

import (
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/shopspring/decimal"
)

func day(text string) time.Time {
	d, _ := time.ParseInLocation("2006-01-02", text, time.UTC)
	return d
}

func TestSlippageBPSign(t *testing.T) {
	// 買い: 想定より安く買えたら有利（プラス）
	if got := SlippageBP("BUY", 1000, 990); math.Abs(got.(float64)-100) > 1e-9 {
		t.Errorf("買いで安く買えたのに %v", got)
	}
	// 買い: 高く買ってしまったら不利（マイナス）
	if got := SlippageBP("BUY", 1000, 1010); got.(float64) >= 0 {
		t.Errorf("買いで高く買ったのに %v", got)
	}
	// 売り: 高く売れたら有利（プラス）
	if got := SlippageBP("SELL", 1000, 1010); math.Abs(got.(float64)-100) > 1e-9 {
		t.Errorf("売りで高く売れたのに %v", got)
	}
	// 値が無いときは nil
	if got := SlippageBP("BUY", nil, 1000); got != nil {
		t.Errorf("想定価格が無いのに %v", got)
	}
	if got := SlippageBP("BUY", 0, 1000); got != nil {
		t.Errorf("想定価格 0 なのに %v", got)
	}
	if got := SlippageBP("BUY", 1000, nil); got != nil {
		t.Errorf("約定価格が無いのに %v", got)
	}
}

func TestRowNormalizesTypes(t *testing.T) {
	row := Row(Spec{
		Event:        EventIntent,
		App:          "accum",
		Symbol:       "7203",
		Side:         "buy",
		Reason:       ReasonPlaced,
		Quantity:     "100",
		IntentPrice:  decimal.RequireFromString("2500.5"),
		IntentAmount: decimal.NewFromInt(250050),
	})
	if row["side"] != "BUY" {
		t.Errorf("side = %v（必ず大文字）", row["side"])
	}
	if row["quantity"] != int64(100) {
		t.Errorf("quantity = %v (%T)", row["quantity"], row["quantity"])
	}
	if row["intent_price"] != 2500.5 {
		t.Errorf("intent_price = %v", row["intent_price"])
	}
	if row["trade"] != nil || row["note"] != nil {
		t.Errorf("空文字は nil にすべき: %v %v", row["trade"], row["note"])
	}
	if row["reason"] != "placed" {
		t.Errorf("reason = %v", row["reason"])
	}
}

func TestFrameKeepsSchemaWhenEmpty(t *testing.T) {
	frame := Frame(nil)
	if frame.Height() != 0 {
		t.Fatalf("行数 = %d", frame.Height())
	}
	if frame.Width() != len(Schema) {
		t.Fatalf("列数 = %d, want %d", frame.Width(), len(Schema))
	}
}

func TestCollectFlushRoundTrip(t *testing.T) {
	Reset()
	store := history.NewStore(t.TempDir())

	Collect(Spec{Event: EventIntent, App: "accum", Symbol: "7203", Side: "BUY", Reason: ReasonPlaced,
		ClientOrderID: "coid-1", Quantity: 100, IntentPrice: 2500, IntentAmount: 250000, Live: true})
	Collect(Spec{Event: EventSkip, App: "accum", Symbol: "6758", Side: "BUY", Reason: ReasonLotTooSmall,
		Note: "予算が1単元に届かない"})

	if len(Pending()) != 2 {
		t.Fatalf("貯めた行 = %d", len(Pending()))
	}
	if err := Flush(store, day("2026-09-03")); err != nil {
		t.Fatal(err)
	}
	if len(Pending()) != 0 {
		t.Fatal("Flush 後は空になるべき")
	}
	// 2 回目の Flush は何もしない
	if err := Flush(store, day("2026-09-03")); err != nil {
		t.Fatal(err)
	}
	if len(store.Files(Kind, history.Range{})) != 1 {
		t.Fatalf("ファイル数 = %d, want 1（貯めて 1 ファイル）", len(store.Files(Kind, history.Range{})))
	}

	frame, err := store.Read(Kind, history.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Height() != 2 {
		t.Fatalf("行数 = %d", frame.Height())
	}
	for _, row := range frame.Rows {
		if row["symbol"] == "6758" && row["reason"] != string(ReasonLotTooSmall) {
			t.Errorf("reason = %v", row["reason"])
		}
	}
}

func TestRecordSkipsEmpty(t *testing.T) {
	store := history.NewStore(t.TempDir())
	if err := Record(store, nil, day("2026-09-03")); err != nil {
		t.Fatal(err)
	}
	if len(store.Files(Kind, history.Range{})) != 0 {
		t.Fatal("0 行のときはファイルを作らない")
	}
}

func TestSummarize(t *testing.T) {
	rows := []map[string]any{
		Row(Spec{Event: EventFill, App: "accum", Symbol: "7203", Side: "BUY", Reason: ReasonFilled,
			IntentPrice: 1000, FillPrice: 990, IntentAmount: 100000}),
		Row(Spec{Event: EventFill, App: "accum", Symbol: "6758", Side: "BUY", Reason: ReasonFilled,
			IntentPrice: 1000, FillPrice: 1010, IntentAmount: 100000}),
		Row(Spec{Event: EventSkip, App: "accum", Symbol: "9984", Side: "BUY", Reason: ReasonLotTooSmall}),
		Row(Spec{Event: EventSkip, App: "wbjp", Symbol: "9984", Side: "BUY", Reason: ReasonDryRun}),
	}
	summary := Summarize(Frame(rows))
	if summary.Height() != 3 {
		t.Fatalf("要約の行数 = %d, want 3", summary.Height())
	}
	first := summary.Rows[0]
	if first["app"] != "accum" || first["reason"] != "filled" || first["count"] != int64(2) {
		t.Fatalf("先頭行 = %v（件数の多い理由から並ぶ）", first)
	}
	// +100bp と -99.0…bp の平均。約定していない行は平均に含めない
	avg := first["avg_slippage_bp"].(float64)
	if math.Abs(avg-0.5) > 1.0 {
		t.Errorf("平均スリッページ = %v", avg)
	}
	if first["amount"] != 200000.0 {
		t.Errorf("amount = %v", first["amount"])
	}
	for _, row := range summary.Rows {
		if row["reason"] == "lot_too_small" && row["avg_slippage_bp"] != nil {
			t.Errorf("約定していない理由に平均が付いている: %v", row)
		}
	}
}

func TestSummarizeEmptyKeepsSchema(t *testing.T) {
	summary := Summarize(Frame(nil))
	if summary.Height() != 0 || summary.Width() != len(SummarySchema) {
		t.Fatalf("空の要約が形を保っていない: %d 行 %d 列", summary.Height(), summary.Width())
	}
}
