package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func writeBlackout(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "earnings.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadBlackout(t *testing.T) {
	path := writeBlackout(t, "[earnings]\nAAA = [\"2025-10-29\", \"2026-01-28\"]\nBBB = [\"2025-10-27\"]\n")
	b, err := LoadBlackout(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b["AAA"]) != 2 || len(b["BBB"]) != 1 {
		t.Fatalf("読めていない: %v", b)
	}

	// 発表日の 3 日前から当日までがブラックアウト。
	if !b.InBlackout("AAA", "2025-10-27", 3) {
		t.Error("3日前は対象")
	}
	if !b.InBlackout("AAA", "2025-10-29", 3) {
		t.Error("発表当日は対象（翌日ギャップの前日）")
	}
	if b.InBlackout("AAA", "2025-10-25", 3) {
		t.Error("4日前は対象外")
	}
	if b.InBlackout("AAA", "2025-10-30", 3) {
		t.Error("発表翌日は対象外")
	}
	if b.InBlackout("CCC", "2025-10-29", 3) {
		t.Error("表に無い銘柄は対象外")
	}
}

func TestLoadBlackoutErrors(t *testing.T) {
	if _, err := LoadBlackout(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Error("存在しないファイルはエラー")
	}
	if _, err := LoadBlackout(writeBlackout(t, "[earnings]\nAAA = \"2025-10-29\"\n")); err == nil {
		t.Error("リストでない指定はエラー")
	}
	if _, err := LoadBlackout(writeBlackout(t, "[earnings]\nAAA = [\"not-a-date\"]\n")); err == nil {
		t.Error("日付として読めない値はエラー")
	}
}

// 決算前は保有を降ろす。日足では翌日のギャップを避けられないため。
func TestRSIPullbackExitsBeforeEarnings(t *testing.T) {
	bars := pullbackUniverse(barsFrom("AAA", uptrendWithDip(), 5_000_000, nil))
	asOf := bars["AAA"][len(bars["AAA"])-1].Date

	s := rsiPullbackStrategy(t, func(o *RSIPullbackOptions) {
		o.BlackoutFile = writeBlackout(t, "[earnings]\nAAA = [\""+asOf+"\"]\n")
	})
	positions := map[string]domain.Position{"AAA": heldPosition("AAA", 100, 100)}
	ctx := NewContext(asOf, bars, positions, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction != -1.0 || !contains(sig.Reason, "決算前") {
		t.Errorf("決算前は手仕舞う: %+v", sig)
	}
}

// 決算前は新規に建てない。
func TestRSIPullbackSkipsEntryInBlackout(t *testing.T) {
	bars := pullbackUniverse(barsFrom("AAA", uptrendWithDip(), 5_000_000, nil))
	asOf := bars["AAA"][len(bars["AAA"])-1].Date

	s := rsiPullbackStrategy(t, func(o *RSIPullbackOptions) {
		o.BlackoutFile = writeBlackout(t, "[earnings]\nAAA = [\""+asOf+"\"]\n")
	})
	ctx := NewContext(asOf, bars, nil, decimal.Zero)

	if _, ok := signalFor(t, mustOnBars(t, s, ctx), "AAA"); ok {
		t.Error("決算前は新規シグナルを出さない")
	}
}
