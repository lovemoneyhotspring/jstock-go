package main

import (
	"testing"

	accumhist "github.com/lovemoneyhotspring/jstock-go/pkg/accum/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/shopspring/decimal"
)

// 照合が貯めた実行品質の行は、実行の終わりの flushExecution で積立の履歴に書き出される
// （以前は Collect だけで Flush を呼ばず、プロセスと一緒に捨てていた）。
func TestFlushExecutionWritesAccumHistory(t *testing.T) {
	saved := *appSettings
	t.Cleanup(func() { *appSettings = saved })
	appSettings.StateDir = t.TempDir()
	execution.Reset()
	t.Cleanup(execution.Reset)

	execution.Collect(execution.Spec{
		Event: "fill", App: "accum", Symbol: "1306", Side: "BUY", ClientOrderID: "c1",
		Live: true, Quantity: decimal.NewFromInt(10), FillQuantity: decimal.NewFromInt(10),
		FillPrice: decimal.NewFromInt(3000), Reason: execution.ReasonFilled,
	})
	flushExecution()

	if n := len(execution.Pending()); n != 0 {
		t.Errorf("書き出した後も %d 行残っている", n)
	}
	files := accumhist.StoreFor(appSettings).Files(execution.Kind, history.Range{})
	if len(files) != 1 {
		t.Fatalf("実行品質のファイル = %v, want 1 つ", files)
	}
	// 貯めた行が無ければ何も書かない
	flushExecution()
	if files := accumhist.StoreFor(appSettings).Files(execution.Kind, history.Range{}); len(files) != 1 {
		t.Errorf("空の Flush でファイルが増えた: %v", files)
	}
}
