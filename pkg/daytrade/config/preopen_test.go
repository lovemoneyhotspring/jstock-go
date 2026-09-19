package config

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// 寄る前に出すのは preopen_legs に挙げた脚だけ。9:00 以降の回は寄成にしない。
func TestPreopenAtAndLegs(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	at := func(hh, mm int) time.Time {
		return time.Date(2026, 9, 19, hh, mm, 0, 0, jst)
	}
	e := Execution{PreopenLegs: PreopenLegsShort}
	if !e.PreopenAt(at(8, 59), jst) {
		t.Error("8:59 が寄る前と判定されない")
	}
	if e.PreopenAt(at(9, 0), jst) || e.PreopenAt(at(9, 1), jst) {
		t.Error("9:00 以降を寄る前と判定した")
	}
	if e.PreopenFor(domain.SideBuy) || !e.PreopenFor(domain.SideSell) {
		t.Error("short: 脚の選び方が違う")
	}
	// 設定が none（既定）なら時刻によらず偽
	none := Execution{PreopenLegs: PreopenLegsNone}
	if none.PreopenAt(at(8, 59), jst) || none.PreopenEnabled() {
		t.Error("none で寄成を出そうとしている")
	}
	if (Execution{}).PreopenEnabled() {
		t.Error("空文字（設定なし）で寄成を出そうとしている")
	}
}

// 窓が 9:00 開始のままだと寄る前の回が走らず、寄成が一度も出ない。設定の食い違いとして弾く。
func TestValidatePreopenNeedsEarlyWindow(t *testing.T) {
	c := Default()
	c.Execution.PreopenLegs = PreopenLegsShort
	if err := c.Validate(); err == nil {
		t.Error("entry_window が 9:00 開始のまま preopen_legs を通した")
	}
	c.Execution.EntryWindow = []string{"08:59", "09:15"}
	if err := c.Validate(); err != nil {
		t.Errorf("8:59 開始の窓で弾かれた: %v", err)
	}
	c.Execution.PreopenLegs = "opening"
	if err := c.Validate(); err == nil {
		t.Error("未知の preopen_legs を通した")
	}
}
