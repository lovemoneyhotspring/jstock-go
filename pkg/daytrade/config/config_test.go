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
		if cfg.Capital.Positions() != 3 {
			t.Errorf("%s: N = %d, want 3", dir, cfg.Capital.Positions())
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
		// 米国小幅高の日は LightGBM のロングだけ取引し、ショートは休む
		if cfg.Regime.UsSkipLegs != UsSkipLegsShort {
			t.Errorf("%s: us_skip_legs = %q, want %q", dir, cfg.Regime.UsSkipLegs, UsSkipLegsShort)
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

// 保険の手仕舞い（引け）は実機で未検証なので、既定は無効。設定ファイルは読める（有効にするのは検証の後）。
func TestProtectExitIsOffByDefault(t *testing.T) {
	if Default().Execution.ProtectExit {
		t.Error("execution.protect_exit の既定が有効（実機で検証するまで無効にしておく）")
	}
	for _, dir := range []string{"../../../config/daytrade", "../../../config/daytrade_margin"} {
		if _, err := Load(dir); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
	}
}
