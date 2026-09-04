package universe_test

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/internal/fixture"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/shopspring/decimal"
)

const layout = "2006-01-02"

var start = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

// buildArchive は 40 営業日ぶんの小さなアーカイブ。
//
// 時価総額を階段状にして分位の切り方を、売買代金を 1 億円の上下に置いて流動性の
// 足切りを、区分と ProdCat で母集団の絞り込みを、それぞれ確かめられるようにする。
func buildArchive(t *testing.T) (*archive.Archive, []time.Time) {
	t.Helper()
	days := fixture.BusinessDays(start, 40)
	symbols := []fixture.Symbol{
		{Code: "10000", Name: "大型プライム", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1000, Turnover: 5e8, MktCap: 9e11},
		{Code: "20000", Name: "中型プライム", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 2000, Turnover: 4e8, MktCap: 5e11},
		{Code: "30000", Name: "小型プライム", Market: "プライム", ProdCat: "011", Mrgn: "1",
			Base: 500, Turnover: 3e8, MktCap: 1e10},
		{Code: "40000", Name: "薄商いプライム", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 800, Turnover: 1e7, MktCap: 6e11}, // 売買代金が足りない
		{Code: "50000", Name: "グロース", Market: "グロース", ProdCat: "011", Mrgn: "2",
			Base: 700, Turnover: 6e8, MktCap: 7e11}, // 区分が違う
		{Code: "60000", Name: "上場投信", Market: "プライム", ProdCat: "021", Mrgn: "2",
			Base: 3000, Turnover: 9e8, MktCap: 8e11}, // 株式でない
	}
	arch, err := fixture.Build(t.TempDir(), days, symbols)
	if err != nil {
		t.Fatal(err)
	}
	return arch, days
}

func codes(rows []universe.Candidate, keep func(universe.Candidate) bool) []string {
	var out []string
	for _, c := range rows {
		if keep(c) {
			out = append(out, c.Code)
		}
	}
	return out
}

func TestBuildFiltersUniverse(t *testing.T) {
	arch, days := buildArchive(t)
	prevDay := days[len(days)-2]
	day := days[len(days)-1]

	cfg := config.Default()
	rows, err := universe.Build(arch, day, prevDay, cfg.Universe, cfg.Margin)
	if err != nil {
		t.Fatal(err)
	}
	// ETF（ProdCat 021）は候補にすら入らない
	for _, c := range rows {
		if c.Code == "60000" {
			t.Error("株式でない銘柄が候補に入っている")
		}
	}
	eligible := codes(rows, func(c universe.Candidate) bool { return c.Eligible })
	// プライム × 売買代金 1 億以上 × 時価総額の下位 1/3 を除く → 10000 と 20000
	want := map[string]bool{"10000": true, "20000": true}
	if len(eligible) != len(want) {
		t.Fatalf("eligible = %v, want %v", eligible, want)
	}
	for _, code := range eligible {
		if !want[code] {
			t.Errorf("想定外の銘柄が母集団に: %s", code)
		}
	}
}

func TestBuildComputesFeatures(t *testing.T) {
	arch, days := buildArchive(t)
	prevDay := days[len(days)-2]
	cfg := config.Default()
	rows, err := universe.Build(arch, days[len(days)-1], prevDay, cfg.Universe, cfg.Margin)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]universe.Candidate{}
	for _, c := range rows {
		byCode[c.Code] = c
	}
	big := byCode["10000"]
	// 値動きを与えていないので前日終値は基準のまま、20 日ボラは 0
	if big.PrevClose != 1000 {
		t.Errorf("prev_close = %v, want 1000", big.PrevClose)
	}
	if big.TurnoverMed != 5e8 {
		t.Errorf("turnover_med = %v, want 5e8", big.TurnoverMed)
	}
	if big.Vol20 == nil || *big.Vol20 != 0 {
		t.Errorf("vol20 = %v, want 0", big.Vol20)
	}
	if big.Symbol != "1000" {
		t.Errorf("発注用の銘柄コード = %s, want 1000", big.Symbol)
	}
	if big.Segment != "prime" || !big.Shortable {
		t.Errorf("区分・貸借の読み取りが違う: %s / %v", big.Segment, big.Shortable)
	}
	// 小型（Mrgn=1）は貸借でないのでショートできない
	if byCode["30000"].Shortable {
		t.Error("信用銘柄を貸借扱いにしている")
	}
	// 分位: 流動性の下限を満たす銘柄の中で切る（40000 は含まれない）
	if big.CapTercile != 3 {
		t.Errorf("大型の分位 = %d, want 3", big.CapTercile)
	}
	if byCode["30000"].CapTercile != 1 {
		t.Errorf("小型の分位 = %d, want 1", byCode["30000"].CapTercile)
	}
	if byCode["40000"].CapTercile != 0 {
		t.Errorf("流動性の足切りに掛かった銘柄の分位 = %d, want 0", byCode["40000"].CapTercile)
	}
}

// 足が 20 本揃わない銘柄（上場間もない）は、バックテストのパネルと同じく母集団に入れない。
func TestBuildExcludesShortHistory(t *testing.T) {
	days := fixture.BusinessDays(start, 40)
	symbols := []fixture.Symbol{
		{Code: "10000", Name: "大型プライム", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1000, Turnover: 5e8, MktCap: 9e11},
		{Code: "70000", Name: "新規上場", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1500, Turnover: 8e8, MktCap: 6e11, ListedOn: days[len(days)-12]}, // 足が 11 本
	}
	arch, err := fixture.Build(t.TempDir(), days, symbols)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	rows, err := universe.Build(arch, days[len(days)-1], days[len(days)-2], cfg.Universe, cfg.Margin)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]universe.Candidate{}
	for _, c := range rows {
		byCode[c.Code] = c
	}
	ipo, ok := byCode["70000"]
	if !ok {
		t.Fatal("新規上場の銘柄が候補の一覧に無い（外れた理由を見せるために行は残す）")
	}
	if ipo.Eligible || ipo.ShortEligible {
		t.Errorf("足が 20 本無い銘柄が母集団に入っている: %+v", ipo)
	}
	if ipo.TurnoverMed != 0 || ipo.Vol20 != nil {
		t.Errorf("足りない本数で特徴量を出している: turnover %v / vol %v", ipo.TurnoverMed, ipo.Vol20)
	}
	if !byCode["10000"].Eligible {
		t.Error("足が揃った銘柄まで外れている")
	}
}

func TestBuildExcludesEarningsAndAlerts(t *testing.T) {
	arch, days := buildArchive(t)
	prevDay := days[len(days)-2]
	day := days[len(days)-1]
	// 前日の引け後（15:30 以降）の開示 → 翌日は買わない
	if err := fixture.AddEarnings(arch, "10000", prevDay, "16:00"); err != nil {
		t.Fatal(err)
	}
	// 前日の**引け前**の開示は対象外（場中の開示は「決算翌日」ではない）
	if err := fixture.AddEarnings(arch, "20000", prevDay, "12:00"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.AddMarginAlert(arch, "30000", prevDay, true); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	rows, err := universe.Build(arch, day, prevDay, cfg.Universe, cfg.Margin)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]universe.Candidate{}
	for _, c := range rows {
		byCode[c.Code] = c
	}
	if !byCode["10000"].EarnPrev {
		t.Error("引け後の決算開示を拾っていない")
	}
	if byCode["20000"].EarnPrev {
		t.Error("引け前の開示を「引け後」と扱っている")
	}
	if byCode["10000"].Eligible {
		t.Error("決算翌日の銘柄を母集団から外していない")
	}
	if !byCode["30000"].Alert || !byCode["30000"].JsfStop {
		t.Error("信用規制・売り禁を拾っていない")
	}
}

func TestBuildShortEligible(t *testing.T) {
	arch, days := buildArchive(t)
	prevDay := days[len(days)-2]
	day := days[len(days)-1]
	if err := fixture.AddMarginAlert(arch, "20000", prevDay, true); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Margin.Enabled = true
	cfg.Margin.MaxCapital = decimal.NewFromInt(2_000_000)
	rows, err := universe.Build(arch, day, prevDay, cfg.Universe, cfg.Margin)
	if err != nil {
		t.Fatal(err)
	}
	short := codes(rows, func(c universe.Candidate) bool { return c.ShortEligible })
	// ショートは分位で外さない（小型に効きが厚い）が、貸借でない 30000 と
	// 売り禁の 20000 は外れる → 10000 だけ
	if len(short) != 1 || short[0] != "10000" {
		t.Errorf("short_eligible = %v, want [10000]", short)
	}
}

func TestBuildRequiresPreviousDayBars(t *testing.T) {
	arch, days := buildArchive(t)
	cfg := config.Default()
	// 足の無い翌営業日を前営業日として指定すると、黙って古い足を使わずに落ちる
	missing := days[len(days)-1].AddDate(0, 0, 7)
	_, err := universe.Build(arch, missing.AddDate(0, 0, 1), missing, cfg.Universe, cfg.Margin)
	if err == nil {
		t.Error("前営業日の足が無いのにエラーにならない")
	}
}

func TestSegmentOf(t *testing.T) {
	cases := map[string]string{
		"プライム（内国株式）":    "prime",
		"東証一部":          "prime",
		"グロース（内国株式）":    "growth",
		"マザーズ":          "growth",
		"JASDAQ グロース":   "growth", // グロースを先に判定する
		"スタンダード":        "standard",
		"東証二部":          "standard",
		"JASDAQ スタンダード": "standard",
		"PRO Market":    "other",
		"":              "other",
	}
	for name, want := range cases {
		if got := universe.SegmentOf(name); got != want {
			t.Errorf("SegmentOf(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestToBrokerSymbol(t *testing.T) {
	cases := map[string]string{"72030": "7203", "130A0": "130A", "7203": "7203", "": ""}
	for in, want := range cases {
		if got := universe.ToBrokerSymbol(in); got != want {
			t.Errorf("ToBrokerSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapTerciles(t *testing.T) {
	values := []float64{10, 20, 30, 40, 50, 60}
	mask := []bool{true, true, true, true, true, true}
	got := universe.CapTerciles(values, mask)
	want := []int{1, 1, 2, 2, 3, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("分位[%d] = %d, want %d（全体 %v）", i, got[i], want[i], got)
		}
	}
	// 母集団の外は 0（順位にも件数にも数えない）
	got = universe.CapTerciles(values, []bool{false, true, true, true, false, false})
	if got[0] != 0 || got[4] != 0 {
		t.Errorf("母集団外が 0 でない: %v", got)
	}
	if got[1] != 1 || got[3] != 3 {
		t.Errorf("母集団内で分位を切っていない: %v", got)
	}
}

func TestIsPostClose(t *testing.T) {
	before := time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC)
	after := time.Date(2024, 11, 5, 0, 0, 0, 0, time.UTC)
	// 引け時刻は 2024-11-05 から 15:30
	if !universe.IsPostClose(before, "15:10") {
		t.Error("旧引け（15:00）後の開示を引け後と判定していない")
	}
	if universe.IsPostClose(after, "15:10") {
		t.Error("新引け（15:30）前の開示を引け後と判定している")
	}
	if !universe.IsPostClose(after, "15:30") {
		t.Error("15:30 ちょうどを引け後と判定していない")
	}
}

func TestIsJsfStop(t *testing.T) {
	if !universe.IsJsfStop(`{"DailyPublication": "1", "RestrictedByJSF": "1"}`) {
		t.Error("売り禁を拾えていない")
	}
	if universe.IsJsfStop(`{"RestrictedByJSF": "0"}`) {
		t.Error("売り禁でないものを拾っている")
	}
	if universe.IsJsfStop("") {
		t.Error("空を売り禁扱いにしている")
	}
}

func TestCloseChangedOn(t *testing.T) {
	if universe.CloseChangedOn.Format(layout) != "2024-11-05" {
		t.Errorf("引け時刻の変更日が違う: %s", universe.CloseChangedOn)
	}
}
