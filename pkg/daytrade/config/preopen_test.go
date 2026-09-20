package config

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
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

// 寄る前の回の締め切りは板寄せ（9:00:00）。過ぎてから出す寄成は寄った銘柄に約定しない。
func TestPreopenDeadlineIsTheAuction(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	e := Default().Execution
	e.PreopenLegs = PreopenLegsLong
	e.EntryWindow = []string{"08:59", "09:15"}
	now := time.Date(2026, 9, 19, 8, 59, 45, 0, jst)

	got := e.RunDeadline("entry", now, true, jst)
	want := time.Date(2026, 9, 19, 9, 0, 0, 0, jst)
	if !got.Equal(want) {
		t.Errorf("寄る前の締め切り = %s, want %s", got.In(jst), want)
	}

	// 9:00 以降の回は従来どおり（max_run_seconds と窓の終わりの早い方）
	after := time.Date(2026, 9, 19, 9, 1, 0, 0, jst)
	got = e.RunDeadline("entry", after, true, jst)
	if !got.Equal(after.Add(time.Duration(e.MaxRunSeconds) * time.Second)) {
		t.Errorf("9:00 以降の締め切り = %s", got.In(jst))
	}

	// 寄成を出さない設定なら寄る前でも従来どおり
	none := e
	none.PreopenLegs = PreopenLegsNone
	if got := none.RunDeadline("entry", now, true, jst); got.Equal(want) {
		t.Error("preopen_legs = none で板寄せの締め切りが付いた")
	}
}

// preopen_limit_pct（寄指）は 0〜5% で、ロングを寄る前に出す設定のときだけ通す。
func TestValidatePreopenLimitPct(t *testing.T) {
	c := Default()
	c.Execution.EntryWindow = []string{"08:59", "09:15"}
	c.Execution.PreopenLegs = PreopenLegsLong
	c.Execution.PreopenLimitPct = decimal.RequireFromString("0.5")
	if err := c.Validate(); err != nil {
		t.Fatalf("long × 0.5%% で弾かれた: %v", err)
	}
	if !c.Execution.PreopenLimitFor(domain.SideBuy) || c.Execution.PreopenLimitFor(domain.SideSell) {
		t.Error("寄指はロングだけ")
	}
	for _, bad := range []string{"-0.1", "5.1"} {
		c.Execution.PreopenLimitPct = decimal.RequireFromString(bad)
		if err := c.Validate(); err == nil {
			t.Errorf("preopen_limit_pct = %s を通した", bad)
		}
	}
	c.Execution.PreopenLimitPct = decimal.RequireFromString("0.5")
	c.Execution.PreopenLegs = PreopenLegsShort
	if err := c.Validate(); err == nil {
		t.Error("ロングを寄る前に出さない設定で preopen_limit_pct を通した")
	}
}
