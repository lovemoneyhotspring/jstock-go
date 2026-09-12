package backtest

import (
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/shopspring/decimal"
)

// パネルの下限は設定より低い固定値。低くないと、設定によっては母集団の行が
// パネルから落ちてしまう（判定は読み出し側の Go が当てるので、SQL は削りすぎない）。
func TestPanelTurnoverFloorIsBelowEverySetting(t *testing.T) {
	cfg := config.Default()
	for name, v := range map[string]decimal.Decimal{
		"universe.min_turnover": cfg.Universe.MinTurnover,
		"margin.min_turnover":   cfg.Margin.MinTurnover,
	} {
		if f, _ := v.Float64(); f < PanelTurnoverFloor {
			t.Errorf("%s = %v がパネルの下限 %v を下回る", name, f, PanelTurnoverFloor)
		}
	}
}

// キャッシュは設定に依存しない（格子を 1 本の読み込みで回せることの土台）。
func TestPanelCacheQueryIgnoresUniverseSettings(t *testing.T) {
	a := config.Default()
	b := config.Default()
	b.Universe.MinTurnover = decimal.NewFromInt(500_000_000)
	b.Universe.ExcludeCapTerciles = 2
	b.Margin.Enabled = !a.Margin.Enabled
	src := panelSources{bars: "b", master: "m"}
	if buildCacheQuery(src, time.Now(), a) != buildCacheQuery(src, time.Now(), b) {
		t.Error("母集団の設定でキャッシュの中身が変わっている")
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
	q := buildCachedPanelQuery("/tmp/x.parquet", time.Now().AddDate(-1, 0, 0), time.Now())
	cached := map[string]bool{}
	for _, c := range strings.Split(strings.TrimPrefix(panelCacheColumns, "SELECT "), ",") {
		cached[strings.TrimSpace(c)] = true
	}
	for _, col := range []string{"d", "code", "o", "c", "prev_close", "next_open", "next_open_d", "vol20",
		"earn_yield", "sector", "segment", "shortable", "turnover_med", "mkt_cap", "earn_prev",
		"disc_today", "alert", "jsf_stop", "is_loss", "short_interest"} {
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
