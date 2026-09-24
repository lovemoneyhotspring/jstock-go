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

	empty := map[string]domain.Position{}
	// 台帳に何も無ければ 0 件は正しい（初回・全部売った後）
	if err := checkEmptyPositions(rep, empty, "uat", "r", true, false, logger); err != nil {
		t.Fatalf("台帳が空なのに止まった: %v", err)
	}

	if err := rep.SaveStop(repo.StopRecord{Symbol: "7203", StopPrice: d("900"), EntryPrice: d("1000"),
		CreatedOn: "2026-09-01", ATRMultiple: d("2")}); err != nil {
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
