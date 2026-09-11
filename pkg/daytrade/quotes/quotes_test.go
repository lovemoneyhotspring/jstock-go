package quotes

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/shopspring/decimal"
)

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quotes.csv")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCSVFetch(t *testing.T) {
	path := writeCSV(t, "symbol,price,at\n7203,2500,2026-09-03T00:00:00Z\n9984,8000\n1234,-1\n")
	source := &CSV{Path: path}
	got, err := source.Fetch([]string{"7203", "9984", "1234", "5555"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("気配 %d 件, want 2（負の値と未収載は落ちる）: %v", len(got), got)
	}
	if !got["7203"].Price.Equal(decimal.NewFromInt(2500)) {
		t.Errorf("価格 = %s", got["7203"].Price)
	}
	if got["7203"].At.Format(time.RFC3339) != "2026-09-03T00:00:00Z" {
		t.Errorf("時刻を読めていない: %v", got["7203"].At)
	}
	// at が無ければ「今」（鮮度の検査は通る）
	if got["9984"].At.IsZero() {
		t.Error("at 省略時に時刻が入っていない")
	}
	if got["7203"].Source != "csv" {
		t.Errorf("取得元 = %s", got["7203"].Source)
	}
}

func TestCSVMissingFile(t *testing.T) {
	source := &CSV{Path: filepath.Join(t.TempDir(), "none.csv")}
	if _, err := source.Fetch([]string{"7203"}); err == nil {
		t.Error("ファイルが無いのにエラーにならない")
	}
}

func TestFreshDropsStaleAndDelayed(t *testing.T) {
	now := time.Date(2026, 9, 3, 0, 5, 0, 0, time.UTC)
	received := map[string]selection.Quote{
		"fresh":   {Symbol: "fresh", At: now.Add(-30 * time.Second)},
		"stale":   {Symbol: "stale", At: now.Add(-10 * time.Minute)},
		"delayed": {Symbol: "delayed", At: now, Delayed: true},
	}
	kept, stale, delayed, _ := Fresh(received, 90, now, false)
	if len(kept) != 1 || kept["fresh"].Symbol != "fresh" {
		t.Errorf("残った気配 = %v", kept)
	}
	if len(stale) != 1 || len(delayed) != 1 {
		t.Errorf("stale=%v delayed=%v", stale, delayed)
	}
	// 分丸めの補正: 149 秒前は 90 + 59 秒に収まるので通り、150 秒前は落ちる
	edge := map[string]selection.Quote{
		"in":  {Symbol: "in", At: now.Add(-149 * time.Second)},
		"out": {Symbol: "out", At: now.Add(-150 * time.Second)},
	}
	kept, stale, _, _ = Fresh(edge, 90, now, false)
	if len(kept) != 1 || kept["in"].Symbol != "in" {
		t.Errorf("分丸めの補正で残った気配 = %v", kept)
	}
	if len(stale) != 1 || stale[0] != "out" {
		t.Errorf("上限を超えた気配が落ちていない: %v", stale)
	}

	// 検証用の逃げ道: allow_delayed なら全部通す
	kept, _, _, _ = Fresh(received, 90, now, true)
	if len(kept) != 3 {
		t.Errorf("allow_delayed で %d 件しか通っていない", len(kept))
	}
}

// 現在値時刻が古くても板が返っていれば落とさない。tDPP:T は最後に約定した時刻なので、
// 約定の薄い銘柄は板が生きていても古いと判定される——落ちるのは寄付が遅れる銘柄＝利益源。
func TestFreshKeepsStaleWhenBookIsPresent(t *testing.T) {
	now := time.Date(2026, 9, 3, 0, 5, 0, 0, time.UTC)
	received := map[string]selection.Quote{
		// 板が返っているので、現在値時刻が 10 分前でも残す
		"thin": {Symbol: "thin", At: now.Add(-10 * time.Minute), HasBook: true},
		// 板が返らない古い気配は今までどおり落とす
		"dead": {Symbol: "dead", At: now.Add(-10 * time.Minute)},
	}
	kept, stale, _, bookKept := Fresh(received, 90, now, false)
	if len(kept) != 1 || kept["thin"].Symbol != "thin" {
		t.Errorf("残った気配 = %v", kept)
	}
	if len(stale) != 1 || stale[0] != "dead" {
		t.Errorf("板の無い古い気配が落ちていない: %v", stale)
	}
	if len(bookKept) != 1 || bookKept[0] != "thin" {
		t.Errorf("板で残した銘柄が数えられていない: %v", bookKept)
	}
	// 遅延の気配は板があっても通さない（別の穴）
	delayedQuote := map[string]selection.Quote{
		"d": {Symbol: "d", At: now, Delayed: true, HasBook: true},
	}
	if kept, _, delayed, _ := Fresh(delayedQuote, 90, now, false); len(kept) != 0 || len(delayed) != 1 {
		t.Errorf("遅延の気配が板で通ってしまう: kept=%v delayed=%v", kept, delayed)
	}
}

// 最良気配から値段を作る（両方あれば中値、片方だけならその値、無ければゼロ）。
func TestBookPrice(t *testing.T) {
	d := decimal.NewFromInt
	cases := []struct {
		name     string
		bid, ask decimal.Decimal
		want     string
	}{
		{"板寄せ中は同値", d(6710), d(6710), "6710"},
		{"開いていれば中値", d(6512), d(6515), "6513.5"},
		{"買いだけ", d(872), decimal.Zero, "872"},
		{"売りだけ", decimal.Zero, d(900), "900"},
		{"板が無い", decimal.Zero, decimal.Zero, "0"},
	}
	for _, c := range cases {
		if got := bookPrice(c.bid, c.ask); got.String() != c.want {
			t.Errorf("%s: bookPrice(%s, %s) = %s, want %s", c.name, c.bid, c.ask, got, c.want)
		}
	}
}

func TestNewRejectsUnknownSource(t *testing.T) {
	if _, err := New("yahoo", Params{}); err == nil {
		t.Error("未知の quote_source が通る")
	}
	if _, err := New("csv", Params{}); err == nil {
		t.Error("quote_file 無しの csv が通る")
	}
	source, err := New("csv", Params{QuoteFile: "a.csv"})
	if err != nil || source.Name() != "csv" {
		t.Errorf("csv を組み立てられない: %v", err)
	}
}

func TestDropOpened(t *testing.T) {
	got, dropped := DropOpened(map[string]selection.Quote{
		"1000": {Symbol: "1000", Price: decimal.NewFromInt(950), Opened: true},
		"2000": {Symbol: "2000", Price: decimal.NewFromInt(970)},
		"3000": {Symbol: "3000", Price: decimal.NewFromInt(980), Opened: true},
	})
	if len(got) != 1 {
		t.Fatalf("残った気配 %d 件（%v）, want 1", len(got), got)
	}
	if _, ok := got["2000"]; !ok {
		t.Error("まだ寄っていない銘柄が残っていない")
	}
	if len(dropped) != 2 || dropped[0] != "1000" || dropped[1] != "3000" {
		t.Errorf("外した銘柄 = %v, want [1000 3000]", dropped)
	}
}

func TestFutureStampedAndDescribeAges(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 1, 0, 0, time.UTC) // 09:01 JST
	quotes := map[string]selection.Quote{
		"7203": {Symbol: "7203", At: now.Add(-30 * time.Second)},
		"9984": {Symbol: "9984", At: now.Add(6*time.Hour + 29*time.Minute)}, // 前日 15:30 を今日と読んだ形
		"6758": {Symbol: "6758", At: now.Add(30 * time.Second)},             // 時計のずれの範囲
	}
	future := FutureStamped(quotes, now, time.Minute)
	if len(future) != 1 || future[0] != "9984" {
		t.Fatalf("future=%v", future)
	}
	lines := DescribeAges(quotes, []string{"9984", "7203", "none"}, now)
	if len(lines) != 2 || lines[0] != "9984@15:30:00 -23340s" || lines[1] != "7203@09:00:30 30s" {
		t.Errorf("lines=%v", lines)
	}
	kept := DropSymbols(quotes, map[string]struct{}{"7203": {}})
	if _, ok := kept["7203"]; ok || len(kept) != 2 {
		t.Errorf("DropSymbols: %v", kept)
	}
}
