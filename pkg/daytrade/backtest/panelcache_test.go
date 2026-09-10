package backtest

import (
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/shopspring/decimal"
)

func TestCacheTurnoverFloorTakesTheSmaller(t *testing.T) {
	cfg := config.Default()
	cfg.Universe.MinTurnover = decimal.NewFromInt(300_000_000)
	cfg.Margin.Enabled = false
	cfg.Margin.MinTurnover = decimal.NewFromInt(100_000_000)
	// ショートが無効なら見るのはロング側だけ
	if got := cacheTurnoverFloor(cfg); got != 3e8 {
		t.Errorf("ショート無効: %v, want 3e8", got)
	}
	// 有効なら小さい方（キャッシュから落とすと母集団が欠ける）
	cfg.Margin.Enabled = true
	if got := cacheTurnoverFloor(cfg); got != 1e8 {
		t.Errorf("ショート有効: %v, want 1e8", got)
	}
}

func TestSourceFilesListsEveryParquet(t *testing.T) {
	src := panelSources{
		bars:   "read_parquet(['/a/bars-2017-01.parquet', '/a/bars-2017-02.parquet'], union_by_name=true)",
		master: "read_parquet(['/a/master.parquet'], union_by_name=true)",
		fins:   "read_parquet(['/a/fins.parquet'], union_by_name=true)", hasFins: true,
	}
	got := sourceFiles(src)
	want := []string{"/a/bars-2017-01.parquet", "/a/bars-2017-02.parquet", "/a/fins.parquet", "/a/master.parquet"}
	if len(got) != len(want) {
		t.Fatalf("sourceFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sourceFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// キャッシュに落とす列と、読み出し側が参照する列がずれると「無い列」で落ちる。
// 版（panelCacheVersion）を上げ忘れる事故も防ぎたいので、名前の対応をここで押さえる。
func TestCachedQueryOnlyUsesCachedColumns(t *testing.T) {
	cfg := config.Default()
	// 列がすべて参照される設定にする（除外条件は真のときだけ SQL に出る）
	cfg.Universe.ExcludeLoss = true
	cfg.Margin.Enabled = true
	cfg.Margin.ExcludeJsfStop = true
	q := buildCachedPanelQuery("/tmp/x.parquet", time.Now().AddDate(-1, 0, 0), time.Now(), cfg)
	cached := map[string]bool{}
	for _, c := range strings.Split(strings.TrimPrefix(panelCacheColumns, "SELECT "), ",") {
		cached[strings.TrimSpace(c)] = true
	}
	for _, col := range []string{"d", "code", "o", "c", "prev_close", "next_open", "next_open_d", "vol20",
		"earn_yield", "segment", "shortable", "turnover_med", "cap_tercile", "earn_prev", "disc_today",
		"alert", "jsf_stop", "is_loss"} {
		if !cached[col] {
			t.Errorf("%s がキャッシュの列に無い", col)
		}
		if !strings.Contains(q, col) {
			t.Errorf("読み出しの SQL が %s を使っていない", col)
		}
	}
	// 期間の終わりは next_open_d で切る（営業日で切ると足の途切れた銘柄を取りこぼす）
	if !strings.Contains(q, "next_open_d >") {
		t.Error("next_open を end で無効にしていない")
	}
}
