package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadRealConfigs(t *testing.T) {
	// リポジトリに実在する設定が読めることは、移植の最低条件
	for _, dir := range []string{"../../../config/daytrade", "../../../config/daytrade_margin"} {
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		// 信用版は規則 R（weighting = turnover）で N = max_positions = 10（2026-09-25〜）
		wantN := 3
		if cfg.Capital.Weighting == WeightingTurnover {
			wantN = 10
		}
		if cfg.Capital.Positions() != wantN {
			t.Errorf("%s: N = %d, want %d", dir, cfg.Capital.Positions(), wantN)
		}
		if len(cfg.Regime.SkipMonths) != 1 || cfg.Regime.SkipMonths[0] != 12 {
			t.Errorf("%s: skip_months = %v, want [12]", dir, cfg.Regime.SkipMonths)
		}
		// drift_gate はコメントアウトされている（既定は無効）
		if cfg.Regime.DriftGate != nil {
			t.Errorf("%s: drift_gate は無効のはず", dir)
		}
		if cfg.Regime.UsSkipHigh == nil {
			t.Errorf("%s: us_skip_high が読めていない", dir)
		}
		// 2026-09-20 から平常日は gap_vol で並べる
		if cfg.Signal.RankBy != RankByGapVol {
			t.Errorf("%s: rank_by = %q, want %q", dir, cfg.Signal.RankBy, RankByGapVol)
		}
		// 2026-09-21 から米国小幅高の日は「LightGBM・前日終値 −1.5% の寄指・寄る前の回だけ」（この 3 つは組で変える）
		if cfg.Signal.RankByUsLow != RankByLGBM || cfg.Regime.UsSkipLegs != UsSkipLegsShort ||
			!cfg.Execution.PreopenLimitPctUsLow.Equal(decimal.RequireFromString("1.5")) {
			t.Errorf("%s: rank_by_us_low = %q / us_skip_legs = %q / preopen_limit_pct_us_low = %s, want lgbm / short / 1.5",
				dir, cfg.Signal.RankByUsLow, cfg.Regime.UsSkipLegs, cfg.Execution.PreopenLimitPctUsLow)
		}
		// 平常日は寄成のまま（寄指のプローブ待ち）
		if !cfg.Execution.ForDay(false).PreopenLimitPct.IsZero() {
			t.Errorf("%s: 平常日の preopen_limit_pct = %s, want 0", dir, cfg.Execution.PreopenLimitPct)
		}
	}
	margin, err := Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatal(err)
	}
	if !margin.Margin.Enabled || margin.Margin.Positions() != 3 {
		t.Errorf("ショートの N = %d（enabled=%v）, want 3 / true", margin.Margin.Positions(), margin.Margin.Enabled)
	}
	if margin.StrategyName() != "jp_gap_fade_margin" {
		t.Errorf("戦略名 = %s", margin.StrategyName())
	}
}

func TestBudgetPerOrder(t *testing.T) {
	cfg := Default()
	// 200 万 ÷ 3 = 666666.67 → 円未満切り捨て
	if got := cfg.Capital.BudgetPerOrder(); !got.Equal(decimal.NewFromInt(666_666)) {
		t.Errorf("BudgetPerOrder = %s, want 666666", got)
	}
	// 資金 0 は「買わない」（N = 0、予算 0）
	cfg.Capital.MaxCapital = decimal.Zero
	if cfg.Capital.Positions() != 0 || !cfg.Capital.BudgetPerOrder().IsZero() {
		t.Error("資金 0 で N・予算が 0 にならない")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	// 綴りを間違えた設定が「効いているつもり」で効かないまま本番に乗るのを防ぐ
	dir := write(t, "[capital]\nmax_capitl = 100\n")
	if _, err := Load(dir); err == nil {
		t.Error("未知の項目がエラーにならない")
	}
}

func TestValidateRanges(t *testing.T) {
	cases := map[string]string{
		"weighting が不正":             "[capital]\nweighting = \"random\"\n",
		"segments に未知の区分":           "[universe]\nsegments = [\"tokyo\"]\n",
		"exclude_cap_terciles が範囲外": "[universe]\nexclude_cap_terciles = 3\n",
		"skip_months が範囲外":          "[regime]\nskip_months = [13]\n",
		"equity_curve_scale が範囲外":   "[regime]\nequity_curve_scale = 1.5\n",
		"us_skip_high が low 以下":     "[regime]\nus_skip_low = 0.02\nus_skip_high = 0.01\n",
		"us_skip_legs が不正":          "[regime]\nus_skip_legs = \"long\"\n",
		"時間帯の開始が終了より後":              "[execution]\nentry_window = [\"09:30\", \"09:00\"]\n",
		"carry_penalty が範囲外":        "[margin]\ncarry_penalty = 2\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s がエラーにならない", name)
		}
	}
}

func TestWindow(t *testing.T) {
	cfg := Default()
	sh, sm, eh, em, err := cfg.Execution.Window("entry")
	if err != nil || sh != 9 || sm != 0 || eh != 9 || em != 15 {
		t.Errorf("entry_window = %d:%d〜%d:%d, %v", sh, sm, eh, em, err)
	}
	sh, sm, eh, em, err = cfg.Execution.Window("exit")
	if err != nil || sh != 15 || sm != 20 || eh != 15 || em != 30 {
		t.Errorf("exit_window = %d:%d〜%d:%d, %v", sh, sm, eh, em, err)
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("設定が無いのにエラーにならない")
	}
}

func TestExtendsOverlaysBase(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	child := filepath.Join(root, "child")
	for dir, body := range map[string]string{
		base:  "[capital]\nmax_capital = 1000000\norder_budget = 500000\n[universe]\nsegments = [\"standard\"]\nmin_turnover = 5\n[regime]\nskip_months = [12]\n",
		child: "extends = \"../base\"\n[capital]\norder_budget = 250000\n[universe]\nsegments = [\"prime\", \"growth\"]\n[margin]\nenabled = true\n",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(child)
	if err != nil {
		t.Fatal(err)
	}
	// 子に無い項目は土台の値
	if !cfg.Capital.MaxCapital.Equal(decimal.NewFromInt(1_000_000)) || !cfg.Universe.MinTurnover.Equal(decimal.NewFromInt(5)) {
		t.Errorf("土台の値が残っていない: %+v", cfg.Capital)
	}
	if len(cfg.Regime.SkipMonths) != 1 || cfg.Regime.SkipMonths[0] != 12 {
		t.Errorf("土台の配列が残っていない: %v", cfg.Regime.SkipMonths)
	}
	// 子に書いた項目は子の値。配列は丸ごと置き換え
	if !cfg.Capital.OrderBudget.Equal(decimal.NewFromInt(250_000)) {
		t.Errorf("子の値で上書きされていない: %s", cfg.Capital.OrderBudget)
	}
	if len(cfg.Universe.Segments) != 2 || cfg.Universe.Segments[0] != "prime" {
		t.Errorf("配列が置き換わっていない: %v", cfg.Universe.Segments)
	}
	if !cfg.Margin.Enabled {
		t.Error("子だけにある表が効いていない")
	}
}

func TestExtendsAcceptsAbsolutePath(t *testing.T) {
	// extends に絶対パスを書いたとき、configDir の下に連結されてはいけない。
	// filepath.Join(configDir, "/tmp/base") は <configDir>/tmp/base になる。
	root := t.TempDir()
	base := filepath.Join(root, "base")
	child := filepath.Join(root, "child")
	for dir, body := range map[string]string{
		base:  "[capital]\nmax_capital = 1000000\norder_budget = 500000\n",
		child: "extends = \"" + base + "\"\n[capital]\norder_budget = 250000\n",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(child)
	if err != nil {
		t.Fatalf("絶対パスの extends が読めない: %v", err)
	}
	if !cfg.Capital.MaxCapital.Equal(decimal.NewFromInt(1_000_000)) {
		t.Errorf("土台の値が効いていない: %s", cfg.Capital.MaxCapital)
	}
	if !cfg.Capital.OrderBudget.Equal(decimal.NewFromInt(250_000)) {
		t.Errorf("子の値で上書きされていない: %s", cfg.Capital.OrderBudget)
	}
}

func TestExtendsRejectsCycleViaAbsolutePath(t *testing.T) {
	// 絶対パスでも循環は弾く（相対と同じ visited で見ている）
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	for dir, other := range map[string]string{a: b, b: a} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "extends = \"" + other + "\"\n"
		if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(a); err == nil {
		t.Error("絶対パスの循環する extends がエラーにならない")
	}
}

func TestExtendsRejectsCycle(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	for dir, body := range map[string]string{a: "extends = \"../b\"\n", b: "extends = \"../a\"\n"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(a); err == nil {
		t.Error("循環する extends がエラーにならない")
	}
}

func TestMarginConfigMatchesBaseLongRules(t *testing.T) {
	// daytrade_margin はロング側を config/daytrade から継ぐ。両者の [universe] / [signal] が
	// ずれていたら、extends が効いていないか、片方だけ直したということ
	base, err := Load("../../../config/daytrade")
	if err != nil {
		t.Fatal(err)
	}
	margin, err := Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(base.Universe) != fmt.Sprint(margin.Universe) {
		t.Errorf("[universe] がずれている:\n base   %+v\n margin %+v", base.Universe, margin.Universe)
	}
	if fmt.Sprint(base.Signal) != fmt.Sprint(margin.Signal) {
		t.Errorf("[signal] がずれている:\n base   %+v\n margin %+v", base.Signal, margin.Signal)
	}
	// [regime] はショック日の倍率だけ子が上書きする（信用買いなら建玉を増やせるため）。
	// それ以外の項目は土台と同じでなければならない
	baseRegime, marginRegime := base.Regime, margin.Regime
	baseRegime.ShockLongScale, baseRegime.ShockShortScale = decimal.Zero, decimal.Zero
	marginRegime.ShockLongScale, marginRegime.ShockShortScale = decimal.Zero, decimal.Zero
	if fmt.Sprint(baseRegime) != fmt.Sprint(marginRegime) {
		t.Errorf("[regime] がずれている（ショック倍率以外）:\n base   %+v\n margin %+v", baseRegime, marginRegime)
	}
	if !margin.Regime.ShockLongScale.Equal(decimal.RequireFromString("1.5")) || !margin.Regime.ShockShortScale.IsZero() {
		t.Errorf("子のショック倍率が 1.5 / 0 でない: %+v", margin.Regime)
	}
	if !base.Regime.ShockLongScale.Equal(decimal.NewFromInt(1)) || !base.Regime.ShockShortScale.Equal(decimal.NewFromInt(1)) {
		t.Errorf("土台（現物）のショック倍率は 1 / 1（記録のみ）のはず: %+v", base.Regime)
	}
	// 子の [capital] が土台（現物の 200 万）を上書きできているかの見張り。金額そのものの
	// 妥当性ではなく extends の効きを見る。2026-09-16 に 300 → 500 万（保証金の増加ぶん。
	// 研究ノート 30-projects/daytrade-margin-sizing）
	if !margin.Margin.Enabled || !margin.Capital.MaxCapital.Equal(decimal.NewFromInt(5_000_000)) {
		t.Errorf("子の上書きが効いていない: %+v", margin.Capital)
	}
	if margin.Capital.MaxCapital.Equal(base.Capital.MaxCapital) {
		t.Errorf("子の [capital] が土台と同じ＝ extends の上書きが消えている: %s", margin.Capital.MaxCapital)
	}
}

func TestRunDeadline(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	at := func(y int, m time.Month, d, hh, mm, ss int) time.Time { return time.Date(y, m, d, hh, mm, ss, 0, jst) }
	exec := Default().Execution // entry 09:00〜09:15、exit 15:20〜15:30、max_run_seconds 150
	cases := []struct {
		name      string
		window    string
		now       time.Time
		useWindow bool
		maxRun    int
		want      time.Time
	}{
		{"時間帯の終わりが先", "entry", at(2026, 9, 14, 9, 14, 0), true, 150, at(2026, 9, 14, 9, 15, 0)},
		{"max_run_seconds が先", "entry", at(2026, 9, 14, 9, 1, 0), true, 150, at(2026, 9, 14, 9, 3, 30)},
		{"時間帯を見ない", "entry", at(2026, 9, 14, 9, 14, 0), false, 150, at(2026, 9, 14, 9, 16, 30)},
		{"max_run_seconds = 0 は時間帯の終わり", "entry", at(2026, 9, 14, 9, 1, 0), true, 0, at(2026, 9, 14, 9, 15, 0)},
		{"どちらも無ければ締め切りなし", "entry", at(2026, 9, 14, 9, 1, 0), false, 0, time.Time{}},
		{"引け", "exit", at(2026, 9, 14, 15, 29, 0), true, 150, at(2026, 9, 14, 15, 30, 0)},
		// UTC ではまだ前日（2026-09-13 23:30Z）。時間帯の終わりは JST の今日で組む
		{"JST の日付の境目", "entry", time.Date(2026, 9, 13, 23, 30, 0, 0, time.UTC), true, 0, at(2026, 9, 14, 9, 15, 0)},
	}
	for _, c := range cases {
		e := exec
		e.MaxRunSeconds = c.maxRun
		got := e.RunDeadline(c.window, c.now, c.useWindow, jst)
		if !got.Equal(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestInWindow(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	at := func(hh, mm, ss int) time.Time { return time.Date(2026, 9, 14, hh, mm, ss, 0, jst) }
	exec := Default().Execution
	cases := []struct {
		name   string
		window string
		now    time.Time
		want   bool
	}{
		{"開始ちょうど", "entry", at(9, 0, 0), true},
		{"開始の 1 秒前", "entry", at(8, 59, 59), false},
		{"終わりの 1 秒前", "entry", at(9, 14, 59), true},
		{"終わりちょうどは外（締め切りと同じ）", "entry", at(9, 15, 0), false},
		{"終わりの分の途中も外", "entry", at(9, 15, 30), false},
		{"引け", "exit", at(15, 25, 0), true},
		{"保険は前場に置かない", "protect", at(11, 0, 0), false},
		{"保険は後場寄りの 1 秒前も外", "protect", at(12, 29, 59), false},
		{"保険は後場寄りから", "protect", at(12, 30, 0), true},
		{"保険の終わりの 1 秒前", "protect", at(15, 18, 59), true},
		{"保険の終わりちょうどは外", "protect", at(15, 19, 0), false},
		{"UTC では前日でも JST で判定", "entry", time.Date(2026, 9, 14, 0, 5, 0, 0, time.UTC), true},
	}
	for _, c := range cases {
		if got := exec.InWindow(c.window, c.now, jst); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	broken := exec
	broken.EntryWindow = []string{"09:15"}
	if broken.InWindow("entry", at(9, 5, 0), jst) {
		t.Error("読めない時間帯を「中」とした")
	}
}

// 保険の手仕舞い（引け）の既定は無効（設定ファイルで有効にする）。設定ファイルは読める。
func TestProtectExitDefaultAndConfigFiles(t *testing.T) {
	if Default().Execution.ProtectExit {
		t.Error("execution.protect_exit の既定が有効（設定ファイルで明示して有効にする）")
	}
	for _, dir := range []string{"../../../config/daytrade", "../../../config/daytrade_margin"} {
		if _, err := Load(dir); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
	}
}

// 規則 R と保証金の比（margin.capacity_ratio）の組み合わせの検証。
func TestValidateTurnoverAndCapacity(t *testing.T) {
	ok, err := Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatal(err)
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("本番の設定が通らない: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*Config)
	}{
		{"比は turnover だけ", func(c *Config) { c.Capital.Weighting = "inverse_vol" }},
		{"比が 1 を超える", func(c *Config) { c.Margin.CapacityRatio = decimal.RequireFromString("1.1") }},
		{"ショック日が平日より小さい", func(c *Config) { c.Margin.ShockCapacityRatio = decimal.RequireFromString("0.5") }},
		{"ショック日が 1 を超える", func(c *Config) { c.Margin.ShockCapacityRatio = decimal.RequireFromString("1.2") }},
		{"天井が負", func(c *Config) { c.Margin.CapacityCeiling = decimal.NewFromInt(-1) }},
		{"ショック日にショートを建てる", func(c *Config) { c.Regime.ShockShortScale = decimal.NewFromInt(1) }},
		{"比が無いのにショック日の比", func(c *Config) { c.Margin.CapacityRatio = decimal.Zero }},
		{"turnover_ratio が 0", func(c *Config) { c.Capital.TurnoverRatio = decimal.Zero }},
		{"turnover_ratio が大きすぎる", func(c *Config) { c.Capital.TurnoverRatio = decimal.RequireFromString("0.1") }},
		{"name_divisor が 0", func(c *Config) { c.Capital.NameDivisor = 0 }},
		{"max_positions が name_divisor より小さい", func(c *Config) { c.Capital.MaxPositions = 5 }},
		{"ショートに turnover", func(c *Config) { c.Margin.Weighting = WeightingTurnover }},
		{"現物（margin 無効）に turnover", func(c *Config) {
			c.Margin.Enabled = false
			c.Margin.CapacityRatio, c.Margin.ShockCapacityRatio, c.Margin.CapacityCeiling = decimal.Zero, decimal.Zero, decimal.Zero
		}},
		{"ショートの倍率が 1 を超える", func(c *Config) { c.Margin.MultiplierNormal = decimal.RequireFromString("1.5") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := ok
			c.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("検証が通ってしまった")
			}
		})
	}
}

// 寄成は 9:00 までに送り切らないと寄付に参加できない。注文の送信は 2 回/秒（broker.orderLimiter）で、
// 8:59:53 開始・注文は約 1 秒後から送り、締め切りの 1 秒前（execute.EntrySendMargin）で止まるので、
// n 本目は 54.0 + (n−2) × 0.5 秒 ≤ 59.0 → 12 本が境目。1 秒の余裕を取って 10 本まで。
// 規則 R は 1 日に max_positions 本まで出すので、これを上げるなら開始時刻か送信の上限を先に見直す。
func TestTurnoverMaxPositionsFitsPreopenWindow(t *testing.T) {
	cfg, err := Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capital.Weighting == WeightingTurnover && cfg.Capital.MaxPositions > 10 {
		t.Errorf("capital.max_positions = %d: 寄成を 9:00 の 1 秒前までに余裕を持って送れるのは 10 本まで", cfg.Capital.MaxPositions)
	}
}
