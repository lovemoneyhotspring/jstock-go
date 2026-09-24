package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

// 発注できなかった銘柄がある回は、銘柄ごとの理由を通知してエラーで返す（非 0 終了。A6）。
// 以前はログの warn だけで、通知にもダイジェストにも出ず終了コード 0 だった。
func TestReportRunErrorAlertsFailedOrders(t *testing.T) {
	var titles, bodies []string
	r := &cli.Run{App: "accum", Alerter: func(title, body string, _ *logging.Logger) bool {
		titles = append(titles, title)
		bodies = append(bodies, body)
		return true
	}}
	failed := &execute.OrdersFailedError{Lines: []string{"563A: 買付余力不足", "2559: 判定用の足（^IXIC）が無いため見送り"}}

	err := reportRunError(r, failed)
	if !errors.Is(err, failed) {
		t.Fatalf("エラーを返すべき（非 0 終了）: %v", err)
	}
	if len(titles) != 1 || !strings.Contains(titles[0], "2 銘柄を発注できませんでした") {
		t.Fatalf("通知 = %v", titles)
	}
	if !strings.Contains(bodies[0], "563A: 買付余力不足") || !strings.Contains(bodies[0], "2559") {
		t.Errorf("通知の本文に銘柄ごとの理由が無い: %q", bodies[0])
	}
}

// 失敗が無ければ何もしない。ほかのエラーは異常終了として通知する。
func TestReportRunErrorPassesThrough(t *testing.T) {
	var titles []string
	r := &cli.Run{App: "accum", Alerter: func(title, _ string, _ *logging.Logger) bool {
		titles = append(titles, title)
		return true
	}}
	if err := reportRunError(r, nil); err != nil || len(titles) != 0 {
		t.Fatalf("成功の回に通知した: %v / %v", err, titles)
	}
	boom := errors.New("台帳を読めないため発注を中止しました")
	if err := reportRunError(r, boom); !errors.Is(err, boom) {
		t.Fatalf("エラーを返すべき: %v", err)
	}
	if len(titles) != 1 || !strings.Contains(titles[0], "異常終了") {
		t.Errorf("通知 = %v", titles)
	}
}
