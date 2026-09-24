package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/shopspring/decimal"
)

// TestCheckEmptyPositionsStopsLiveRun は 2026-09-24 の再点検の再現。
//
// 建玉の照会がエラーなしで 0 件を返したとき、台帳にストップ（保有中のはず）があれば
// 発注する回は止める。以前はそのまま進み、RetainHeld がストップを全部外し、SyncStops が
// 台帳の stops を全部消し、sizer が保有中の銘柄を新規として買い直した。
func TestCheckEmptyPositionsStopsLiveRun(t *testing.T) {
	rep, err := repo.OpenRepo(filepath.Join(t.TempDir(), "wbjp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rep.Close() })
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	d := decimal.RequireFromString
	if err := rep.StartRun("r", "2026-09-24", "uat", "live"); err != nil {
		t.Fatal(err)
	}

	empty := map[string]domain.Position{}
	// 台帳に何も無ければ 0 件は正しい（初回・全部売った後）
	if err := checkEmptyPositions(rep, empty, "uat", "r", true, false, logger); err != nil {
		t.Fatalf("台帳が空なのに止まった: %v", err)
	}

	if err := rep.SyncStops(map[string]repo.StopRecord{"7203": {Symbol: "7203", StopPrice: d("900"), EntryPrice: d("1000"),
		CreatedOn: "2026-09-01", ATRMultiple: d("2")}}); err != nil {
		t.Fatal(err)
	}

	// 発注する回は止める
	err = checkEmptyPositions(rep, empty, "uat", "r", true, false, logger)
	if err == nil || !strings.Contains(err.Error(), "7203") {
		t.Fatalf("保有中のはずの銘柄があるのに止まらない: %v", err)
	}
	// 数量 0 の行だけの応答も 0 件と同じ
	zero := map[string]domain.Position{"7203": {Symbol: "7203", Quantity: decimal.Zero}}
	if err := checkEmptyPositions(rep, zero, "uat", "r", true, false, logger); err == nil {
		t.Error("数量 0 の建玉だけの応答で止まらない")
	}
	// dry-run（建玉 0 の模型）は警告だけで続ける
	if err := checkEmptyPositions(rep, empty, "uat", "r", false, false, logger); err != nil {
		t.Errorf("dry-run が止まった: %v", err)
	}
	// 口座が本当に空だと確かめた（--accept-flat）なら続ける
	if err := checkEmptyPositions(rep, empty, "uat", "r", true, true, logger); err != nil {
		t.Errorf("--accept-flat でも止まった: %v", err)
	}
	// 何か 1 銘柄でも見えていれば照会は生きている（手仕舞いは RetainHeld に任せる）
	held := map[string]domain.Position{"6758": {Symbol: "6758", Quantity: d("100")}}
	if err := checkEmptyPositions(rep, held, "uat", "r", true, false, logger); err != nil {
		t.Errorf("建玉が見えているのに止まった: %v", err)
	}
	// 止めた回はストップを消していない
	if saved, err := rep.GetStops(); err != nil || len(saved) != 1 {
		t.Errorf("ストップが残っていない: %+v err=%v", saved, err)
	}
}

// TestAcceptFlatExpiresAfterOneUse は 2026-09-24 の再点検。--accept-flat を付けたまま
// （cron の行に残したまま）だと、0 件の検査を毎回素通りしていた。1 回で失効させる。
func TestAcceptFlatExpiresAfterOneUse(t *testing.T) {
	rep, err := repo.OpenRepo(filepath.Join(t.TempDir(), "wbjp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rep.Close() })
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	d := decimal.RequireFromString
	empty := map[string]domain.Position{}
	saveStop := func() {
		t.Helper()
		if err := rep.SyncStops(map[string]repo.StopRecord{"7203": {Symbol: "7203", StopPrice: d("900"), EntryPrice: d("1000"),
			CreatedOn: "2026-09-01", ATRMultiple: d("2")}}); err != nil {
			t.Fatal(err)
		}
	}
	// 1 回の実行: 始めて、検査して、結果を残す
	check := func(runID, asOf string) error {
		t.Helper()
		if err := rep.StartRun(runID, asOf, "uat", "live"); err != nil {
			t.Fatal(err)
		}
		err := checkEmptyPositions(rep, empty, "uat", runID, true, true, logger)
		status := "success"
		if err != nil {
			status = "failed"
		}
		if ferr := rep.FinishRun(runID, status, nil, nil, nil); ferr != nil {
			t.Fatal(ferr)
		}
		return err
	}

	// 期待が空なら素通りではない（印も付けない）
	if err := check("r0", "2026-09-22"); err != nil {
		t.Fatalf("台帳が空なのに止まった: %v", err)
	}
	saveStop()
	// 1 回目は素通りする
	if err := check("r1", "2026-09-24"); err != nil {
		t.Fatalf("1 回目の --accept-flat で止まった: %v", err)
	}
	// 同じ日の 2 回目は止める（その回が成功した後もまだ保有中のはずなら照会を疑う）
	saveStop()
	if err := check("r2", "2026-09-24"); err == nil || !strings.Contains(err.Error(), "使い済み") {
		t.Fatalf("同じ日に 2 回素通りした: %v", err)
	}
	// 翌日でも、前に成功した回が素通りした回なら止める（cron に残したままの形）
	if err := check("r3", "2026-09-25"); err == nil {
		t.Fatal("素通りした回の直後の回でまた素通りした")
	}
	// 間に通常の検査で成功した回があれば、また 1 回だけ使える
	held := map[string]domain.Position{"7203": {Symbol: "7203", Quantity: d("100")}}
	if err := rep.StartRun("r4", "2026-09-26", "uat", "live"); err != nil {
		t.Fatal(err)
	}
	if err := checkEmptyPositions(rep, held, "uat", "r4", true, true, logger); err != nil {
		t.Fatal(err)
	}
	if err := rep.FinishRun("r4", "success", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := check("r5", "2026-09-29"); err != nil {
		t.Errorf("通常の回を挟んだのに --accept-flat が使えない: %v", err)
	}
	// 素通りした回が失敗したなら、使い済みにしない（同じ日にやり直せる）
	if err := rep.StartRun("r6", "2026-09-30", "uat", "live"); err != nil {
		t.Fatal(err)
	}
	if err := rep.FinishRun("r6", "success", nil, nil, nil); err != nil { // 通常の回
		t.Fatal(err)
	}
	if err := rep.StartRun("r7", "2026-10-01", "uat", "live"); err != nil {
		t.Fatal(err)
	}
	if err := checkEmptyPositions(rep, empty, "uat", "r7", true, true, logger); err != nil {
		t.Fatal(err)
	}
	if err := rep.FinishRun("r7", "failed", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := check("r8", "2026-10-01"); err != nil {
		t.Errorf("素通りした回が失敗したのにやり直せない: %v", err)
	}
}
