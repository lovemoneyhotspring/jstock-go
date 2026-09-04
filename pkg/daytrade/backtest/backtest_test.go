package backtest_test

import (
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/internal/fixture"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/shopspring/decimal"
)

const layout = "2006-01-02"

var start = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

// buildArchive は 60 営業日ぶんのアーカイブ。最後の 10 日に、A はギャップダウン後に反発、
// B はギャップアップ後に反落、という「逆張りが効く」動きを入れる。
func buildArchive(t *testing.T, days []time.Time) *archive.Archive {
	t.Helper()
	gapDown := map[string]float64{}
	reboundUp := map[string]float64{}
	gapUp := map[string]float64{}
	fadeDown := map[string]float64{}
	for _, day := range days[len(days)-10:] {
		key := day.Format(layout)
		gapDown[key] = -0.03  // 寄付で −3%
		reboundUp[key] = 0.02 // 引けまでに +2% 戻す
		gapUp[key] = 0.08     // 寄付で +8%
		fadeDown[key] = -0.02 // 引けまでに −2% 下げる
	}
	symbols := []fixture.Symbol{
		{Code: "10000", Name: "ギャップ下げ", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1000, Turnover: 5e8, MktCap: 9e11, GapOn: gapDown, IntradayOn: reboundUp},
		{Code: "20000", Name: "ギャップ上げ", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 2000, Turnover: 4e8, MktCap: 8e11, GapOn: gapUp, IntradayOn: fadeDown},
		{Code: "30000", Name: "動かない", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1500, Turnover: 3e8, MktCap: 7e11},
		{Code: "40000", Name: "小型", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 300, Turnover: 2e8, MktCap: 1e10},
	}
	arch, err := fixture.Build(t.TempDir(), days, symbols)
	if err != nil {
		t.Fatal(err)
	}
	return arch
}

func baseConfig() config.Config {
	cfg := config.Default()
	// 危険信号は個別に確かめるので、ここでは全部切って規則そのものを見る
	cfg.Regime.SkipMonths = nil
	cfg.Regime.UsSkipHigh = nil
	cfg.Regime.DriftGate = nil
	cfg.Regime.EquityCurveDays = 0
	cfg.Capital.Weighting = "equal"
	return cfg
}

func TestLoadPanelBuildsFeatures(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	cfg := baseConfig()
	// 特徴量（20 日中央値・ボラ）が揃うのは 20 営業日目以降なので、後半だけを見る
	from, to := days[30], days[len(days)-1]

	panel, err := backtest.LoadPanel(arch, from, to, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(panel.Days) != 30 {
		t.Fatalf("営業日 %d, want 30", len(panel.Days))
	}
	last := to.Format(layout)
	found := map[string]backtest.Row{}
	for _, r := range panel.Rows {
		if r.Date.Format(layout) == last {
			found[r.Code] = r
		}
	}
	// プライム × 流動性 OK × 分位で下位 1/3（小型 40000）を除く → 3 銘柄
	if len(found) != 3 {
		t.Fatalf("最終日の母集団 %d 銘柄（%v）, want 3", len(found), found)
	}
	a := found["10000"]
	if a.Gap > -0.029 || a.Gap < -0.031 {
		t.Errorf("ギャップ = %v, want ≈ -0.03", a.Gap)
	}
	if a.Open >= a.PrevClose {
		t.Errorf("寄付が前日終値以上（ギャップダウンになっていない）: %v / %v", a.Open, a.PrevClose)
	}
	// 制限値幅が前日終値から引かれている
	if a.LimitLow >= a.PrevClose || a.LimitHigh <= a.PrevClose {
		t.Errorf("制限値幅が前日終値を挟んでいない: %v 〜 %v（基準 %v）", a.LimitLow, a.LimitHigh, a.PrevClose)
	}
	if !a.Eligible {
		t.Error("ギャップ下げ銘柄が母集団に入っていない")
	}
}

func TestSimulateProfitsFromGapDown(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	cfg := baseConfig()
	result, err := backtest.Run(arch, cfg, days[30], days[len(days)-1], nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// ギャップダウンから +2% 戻る銘柄だけを買うので、期間の損益はプラス
	if result.Summary.TotalPnL <= 0 {
		t.Errorf("損益 = %v, want > 0", result.Summary.TotalPnL)
	}
	if result.Summary.TradedDays != 10 {
		t.Errorf("取引日 = %d, want 10（ギャップを入れた 10 日）", result.Summary.TradedDays)
	}
	if len(result.Trades) == 0 {
		t.Fatal("取引が 1 件も無い")
	}
	for _, tr := range result.Trades {
		if tr.Code != "10000" {
			t.Errorf("ギャップダウン以外を買っている: %s（gap %v）", tr.Code, tr.Gap)
		}
		if tr.Shares <= 0 || int(tr.Shares)%100 != 0 {
			t.Errorf("株数が単元になっていない: %v", tr.Shares)
		}
		if tr.Fees <= 0 {
			t.Errorf("現物なのに手数料が 0: %+v", tr)
		}
	}
	if result.Summary.RoundTripBP <= 0 {
		t.Errorf("往復手数料 = %v bp, want > 0", result.Summary.RoundTripBP)
	}
}

// gapFill は約定モデルの差し替えの確認用: 建値を寄付より 1% 高く（滑り）、手仕舞いは引け。
type gapFill struct{}

func (gapFill) Fill(r backtest.Row) (float64, float64, bool) { return r.Open * 1.01, r.Close, true }

func TestSimulateWithFillModel(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	cfg := baseConfig()
	panel, err := backtest.LoadPanel(arch, days[30], days[len(days)-1], cfg)
	if err != nil {
		t.Fatal(err)
	}
	base, err := backtest.Simulate(panel, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	slipped, err := backtest.SimulateWith(panel, cfg, nil, backtest.Options{Fill: gapFill{}})
	if err != nil {
		t.Fatal(err)
	}
	if slipped.Summary.TotalPnL >= base.Summary.TotalPnL {
		t.Errorf("滑りを入れたのに損益が減らない: %v >= %v", slipped.Summary.TotalPnL, base.Summary.TotalPnL)
	}
	if len(slipped.Trades) != len(base.Trades) {
		t.Errorf("約定モデルで銘柄の選び方が変わった: %d != %d", len(slipped.Trades), len(base.Trades))
	}
	for i, tr := range slipped.Trades {
		if tr.Entry <= base.Trades[i].Entry || tr.Exit != base.Trades[i].Exit {
			t.Errorf("建値・手仕舞い値が約定モデルの値になっていない: %+v", tr)
		}
	}
}

func TestSimulateCarriesLongPinnedAtLimitDown(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	pinned := days[len(days)-5]
	next := days[len(days)-4]
	// 前日終値 ≈ 1000 円 → 制限値幅 ±300。寄付 −3%、引けまでに −29% で引けが 688 円 ≤ 700 円（ストップ安）。
	// 翌日はさらに −5% で寄る
	symbols := []fixture.Symbol{
		{Code: "10000", Name: "張り付き", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1000, Turnover: 5e8, MktCap: 9e11,
			GapOn:      map[string]float64{pinned.Format(layout): -0.03, next.Format(layout): -0.05},
			IntradayOn: map[string]float64{pinned.Format(layout): -0.29}},
		{Code: "20000", Name: "動かない", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 2000, Turnover: 4e8, MktCap: 8e11},
		{Code: "30000", Name: "小型", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 300, Turnover: 2e8, MktCap: 1e10},
	}
	arch, err := fixture.Build(t.TempDir(), days, symbols)
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Signal.SkipLimitDown = false // 寄付はストップ安ではないが、念のため判定を切る
	result, err := backtest.Run(arch, cfg, days[30], days[len(days)-1], nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var carried *backtest.Trade
	for i := range result.Trades {
		if result.Trades[i].Date.Equal(pinned) {
			carried = &result.Trades[i]
		}
	}
	if carried == nil {
		t.Fatal("張り付きの日の取引が無い")
	}
	if !carried.Carried {
		t.Fatalf("引けストップ安のロングが持ち越しになっていない: %+v", *carried)
	}
	// 翌寄り（引け × 0.95）で売った損益 = 株数 × (翌寄り − 建値) − 手数料。
	// fixture は値段を 0.1 円に丸めるので、その 1 刻みぶんの差は許す
	nextOpen := carried.Exit * 0.95
	want := carried.Shares*(nextOpen-carried.Entry) - carried.Fees
	if diff := math.Abs(carried.PnL - want); diff > carried.Shares*0.1 {
		t.Errorf("持ち越しの損益 = %v, want ≈ %v", carried.PnL, want)
	}
	if carried.PnL >= carried.Shares*(carried.Exit-carried.Entry) {
		t.Errorf("翌寄りの下落が損益に載っていない: %v", carried.PnL)
	}
}

func TestSimulateEquityCurveDoesNotShrinkAfterUntradedWindow(t *testing.T) {
	// 1〜2 月を休み、3 月から建てる。休み明けの窓は「建てていない」ので縮めない
	days := fixture.BusinessDays(start, 60) // 1/5 〜 3 月末
	arch := buildArchive(t, days)
	cfg := baseConfig()
	cfg.Regime.SkipMonths = []int{1, 2}
	cfg.Regime.EquityCurveDays = 20
	cfg.Regime.EquityCurveScale = decimal.RequireFromString("0.5")
	result, err := backtest.Run(arch, cfg, days[0], days[len(days)-1], nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var march []backtest.Daily
	for _, d := range result.Daily {
		if d.Date.Month() == 3 {
			march = append(march, d)
		}
	}
	if len(march) == 0 {
		t.Fatal("3 月の日が無い")
	}
	if march[0].Scale != 1 {
		t.Errorf("休み明け初日の倍率 = %v, want 1（建てていない窓で縮めない）", march[0].Scale)
	}
}

func TestSimulateMarginCapsShortAtMarketLimit(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	gapUp := map[string]float64{}
	fade := map[string]float64{}
	for _, day := range days[len(days)-10:] {
		gapUp[day.Format(layout)] = 0.08
		fade[day.Format(layout)] = -0.02
	}
	symbols := []fixture.Symbol{
		// 100 円の低位株: 67 万円で 6,700 株 → 50 単元（5,000 株）を超えるので成行では出せない
		{Code: "10000", Name: "低位ギャップ上げ", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 100, Turnover: 5e8, MktCap: 9e11, GapOn: gapUp, IntradayOn: fade},
		{Code: "20000", Name: "ギャップ上げ", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 2000, Turnover: 4e8, MktCap: 8e11, GapOn: gapUp, IntradayOn: fade},
		{Code: "30000", Name: "動かない", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1500, Turnover: 3e8, MktCap: 7e11},
	}
	arch, err := fixture.Build(t.TempDir(), days, symbols)
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Margin.Enabled = true
	cfg.Margin.MaxCapital = decimal.NewFromInt(2_000_000)
	cfg.Margin.OrderBudget = decimal.NewFromInt(670_000)
	cfg.Margin.Weighting = "equal"
	result, err := backtest.RunMargin(arch, cfg, days[30], days[len(days)-1], nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ShortTrades) == 0 {
		t.Fatal("ショートの取引が無い")
	}
	cheap := 0
	for _, tr := range result.ShortTrades {
		if tr.Shares > 5000 {
			t.Errorf("株数が 50 単元を超えている: %+v", tr)
		}
		if tr.Code == "10000" {
			cheap++
			if tr.Shares != 5000 {
				t.Errorf("低位株の売建が 50 単元で頭打ちになっていない: %+v", tr)
			}
		}
	}
	if cheap == 0 {
		t.Error("低位株の売建を建てていない（上限で切って建てるのが正しい）")
	}
}

func TestSimulateSkipMonthsStopsTrading(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	cfg := baseConfig()
	// パネルの期間はすべて 1〜3 月なので、その全部を休む指定にすれば取引ゼロになる
	cfg.Regime.SkipMonths = []int{1, 2, 3}
	result, err := backtest.Run(arch, cfg, days[30], days[len(days)-1], nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.TradedDays != 0 || result.Summary.TotalPnL != 0 {
		t.Errorf("休む月に取引している: 取引日 %d / 損益 %v",
			result.Summary.TradedDays, result.Summary.TotalPnL)
	}
	if len(result.Trades) != 0 {
		t.Errorf("止めた日の取引が残っている: %d 件", len(result.Trades))
	}
}

func TestSimulateRequiresCapital(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	cfg := baseConfig()
	cfg.Capital.MaxCapital = decimal.Zero
	if _, err := backtest.Run(arch, cfg, days[30], days[len(days)-1], nil, ""); err == nil {
		t.Error("資金 0 で検証できてしまう")
	}
}

func TestSimulateMarginAddsShortLeg(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	cfg := baseConfig()
	cfg.Margin.Enabled = true
	cfg.Margin.MaxCapital = decimal.NewFromInt(2_000_000)
	cfg.Margin.OrderBudget = decimal.NewFromInt(670_000)
	cfg.Margin.Weighting = "equal"

	result, err := backtest.RunMargin(arch, cfg, days[30], days[len(days)-1], nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ShortTrades) == 0 {
		t.Fatal("ショートの取引が無い")
	}
	for _, tr := range result.ShortTrades {
		if tr.Code != "20000" {
			t.Errorf("ギャップアップ以外を売建てている: %s（gap %v）", tr.Code, tr.Gap)
		}
		// 立花証券の信用取引は手数料 0 円。費用は extra_cost_bp だけ
		wantFee := tr.Amount * 5 / 1e4
		if diff := tr.Fees - wantFee; diff > 1 || diff < -1 {
			t.Errorf("ショートの費用 = %v, want ≈ %v（extra_cost_bp のみ）", tr.Fees, wantFee)
		}
	}
	// ギャップアップから −2% 下げる銘柄を売るので、ショート側もプラス
	if result.ShortSummary.TotalPnL <= 0 {
		t.Errorf("ショートの損益 = %v, want > 0", result.ShortSummary.TotalPnL)
	}
	if result.Summary.TotalPnL <= result.LongSummary.TotalPnL {
		t.Errorf("合算がロング単独以下: %v <= %v", result.Summary.TotalPnL, result.LongSummary.TotalPnL)
	}
	for _, d := range result.Daily {
		if d.N != d.LongN+d.ShortN {
			t.Errorf("%s: 銘柄数の内訳が合わない %d != %d + %d",
				d.Date.Format(layout), d.N, d.LongN, d.ShortN)
		}
	}
}

func TestSimulateMarginRequiresEnabled(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	cfg := baseConfig()
	if _, err := backtest.RunMargin(arch, cfg, days[30], days[len(days)-1], nil, ""); err == nil {
		t.Error("margin 無効なのに RunMargin が通る")
	}
}

func TestRequiredMargin(t *testing.T) {
	cfg := baseConfig()
	cfg.Margin.MaxCapital = decimal.NewFromInt(2_000_000)
	peak, required := backtest.RequiredMargin(cfg)
	// ロング 200 万 + ショート 200 万 × 倍率 1.0 = 400 万、その 33%
	if !peak.Equal(decimal.NewFromInt(4_000_000)) {
		t.Errorf("建玉の最大 = %s, want 4000000", peak)
	}
	if !required.Equal(decimal.NewFromInt(1_320_000)) {
		t.Errorf("必要保証金 = %s, want 1320000", required)
	}
}

func TestYearlyOf(t *testing.T) {
	daily := []backtest.Daily{
		{Date: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), PnL: 100, N: 2},
		{Date: time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC), PnL: -50, N: 2},
		{Date: time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC), PnL: 0, N: 0},
	}
	years := backtest.YearlyOf(daily)
	if len(years) != 1 {
		t.Fatalf("年数 = %d", len(years))
	}
	y := years[0]
	if y.Days != 3 || y.Traded != 2 || y.PnL != 50 {
		t.Errorf("年別の集計が違う: %+v", y)
	}
	if y.WinRate != 0.5 {
		t.Errorf("勝率（取引日ベース）= %v, want 0.5", y.WinRate)
	}
}
