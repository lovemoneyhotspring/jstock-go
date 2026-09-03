package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig は一時ディレクトリに accum.toml を書き、その置き場所を返す。
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accum.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const minimalHeader = `
monthly_budget = 25_000
[execution]
broker = "paper"
order_type = "limit"
limit_offset = "0.01"
`

func TestLoadMinimal(t *testing.T) {
	dir := writeConfig(t, minimalHeader+`
[[tactics]]
id = "基準"
tactic = "constant"
symbols = ["1306.T"]
monthly_budget = 20_000
`)
	cfg, err := LoadAccumConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tactics) != 1 {
		t.Fatalf("戦略が %d 件", len(cfg.Tactics))
	}
	if got := cfg.Execution.MaxStaleDays; got != 6 {
		t.Errorf("max_stale_days の既定は 6、実際は %d", got)
	}
	// 発注時間帯の既定は 14:00〜15:00。
	if got := cfg.Tactics[0].Window.Describe(); got != "14:00〜15:00 JST" {
		t.Errorf("window の既定 = %q", got)
	}
}

func TestValidateAssignmentRejectsDoubleBuy(t *testing.T) {
	dir := writeConfig(t, minimalHeader+`
[[tactics]]
id = "A"
tactic = "constant"
symbols = ["1306.T"]
monthly_budget = 10_000

[[tactics]]
id = "B"
tactic = "bear_stack"
symbols = ["1306.T"]
multiplier = 2
monthly_budget = 10_000
`)
	_, err := LoadAccumConfig(dir)
	if err == nil {
		t.Fatal("同じ銘柄への二重割り当てを弾くべき")
	}
	if !strings.Contains(err.Error(), "二重買付") {
		t.Errorf("二重買付の警告を含むべき: %v", err)
	}
}

func TestValidateAssignmentIgnoresDisabled(t *testing.T) {
	// 無効な戦略は割り当ての衝突に数えない。
	dir := writeConfig(t, minimalHeader+`
[[tactics]]
id = "A"
tactic = "constant"
symbols = ["1306.T"]
monthly_budget = 10_000

[[tactics]]
id = "B"
tactic = "constant"
symbols = ["1306.T"]
monthly_budget = 10_000
enabled = false
`)
	if _, err := LoadAccumConfig(dir); err != nil {
		t.Fatalf("無効な戦略との衝突は無視すべき: %v", err)
	}
}

func TestDuplicateIDRejected(t *testing.T) {
	dir := writeConfig(t, minimalHeader+`
[[tactics]]
id = "同じ"
tactic = "constant"
symbols = ["1306.T"]
monthly_budget = 10_000

[[tactics]]
id = "同じ"
tactic = "constant"
symbols = ["1321.T"]
monthly_budget = 10_000
`)
	_, err := LoadAccumConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "id が重複") {
		t.Errorf("id の重複を弾くべき: %v", err)
	}
}

func TestRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "未知の戦略名",
			body: `[[tactics]]
id = "x"
tactic = "moon_phase"
symbols = ["1306.T"]
monthly_budget = 10_000`,
			want: "未知の戦略",
		},
		{
			name: "予算が0以下",
			body: `[[tactics]]
id = "x"
tactic = "constant"
symbols = ["1306.T"]
monthly_budget = -1`,
			want: "monthly_budget",
		},
		{
			name: "銘柄が空",
			body: `[[tactics]]
id = "x"
tactic = "constant"
symbols = []
monthly_budget = 10_000`,
			want: "symbols が空",
		},
		{
			name: "銘柄の重複",
			body: `[[tactics]]
id = "x"
tactic = "constant"
symbols = ["1306.T", "1306.T"]
monthly_budget = 10_000`,
			want: "重複",
		},
		{
			name: "倍率が1未満",
			body: `[[tactics]]
id = "x"
tactic = "bear_stack"
symbols = ["1306.T"]
multiplier = 0.5
monthly_budget = 10_000`,
			want: "1.0 以上",
		},
		{
			name: "移動平均の順序が逆",
			body: `[[tactics]]
id = "x"
tactic = "bear_stack"
symbols = ["1306.T"]
multiplier = 2
fast = 200
mid = 50
slow = 20
monthly_budget = 10_000`,
			want: "短期 < 中期 < 長期",
		},
		{
			name: "段表が単調でない",
			body: `[[tactics]]
id = "x"
tactic = "stack_ladder"
symbols = ["1306.T"]
multipliers = { 3 = 4.0, 5 = 2.0 }
monthly_budget = 10_000`,
			want: "倍率も大きく",
		},
		{
			name: "弱気スコアが範囲外",
			body: `[[tactics]]
id = "x"
tactic = "stack_ladder"
symbols = ["1306.T"]
multipliers = { 9 = 2.0 }
monthly_budget = 10_000`,
			want: "0〜6",
		},
		{
			name: "levelsが浅い順でない",
			body: `[[tactics]]
id = "x"
tactic = "drawdown_ladder"
symbols = ["1306.T"]
levels = [0.30, 0.10]
multipliers = [2.0, 3.0]
monthly_budget = 10_000`,
			want: "浅い順",
		},
		{
			name: "levelsとmultipliersの個数違い",
			body: `[[tactics]]
id = "x"
tactic = "drawdown_ladder"
symbols = ["1306.T"]
levels = [0.10, 0.20]
multipliers = [2.0]
monthly_budget = 10_000`,
			want: "長さが違います",
		},
		{
			name: "発注時間帯が立会時間外",
			body: `[[tactics]]
id = "x"
tactic = "constant"
symbols = ["1306.T"]
window = { start = "16:00", end = "17:00" }
monthly_budget = 10_000`,
			want: "立会時間外",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeConfig(t, minimalHeader+"\n"+tc.body)
			_, err := LoadAccumConfig(dir)
			if err == nil {
				t.Fatalf("エラーになるべき")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("エラーに %q を含むべき: %v", tc.want, err)
			}
		})
	}
}

func TestExecutionValidate(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"order_typeが不正", `[execution]
order_type = "stop"`, "market か limit"},
		{"limit_offsetが範囲外", `[execution]
order_type = "limit"
limit_offset = "0.5"`, "0 以上 0.2 未満"},
		{"limit_offsetが負", `[execution]
order_type = "limit"
limit_offset = "-0.01"`, "0 以上 0.2 未満"},
		{"max_stale_daysが0以下", `[execution]
order_type = "limit"
max_stale_days = -1`, "1 以上"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeConfig(t, "monthly_budget = 25_000\n"+tc.body)
			_, err := LoadAccumConfig(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("エラーに %q を含むべき: %v", tc.wantErr, err)
			}
		})
	}
}

func TestSignalLags(t *testing.T) {
	// 東証の銘柄を米国の指数で判定するなら、同じ日付の指数の足はまだ無い。
	jpWithUSSignal := TacticEntry{Market: "JP", SignalSymbol: "^GSPC", SignalMarket: "US"}
	if !jpWithUSSignal.SignalLags() {
		t.Error("米国指数で東証銘柄を判定するときは1日ずらすべき")
	}

	// 判定用銘柄が無ければずらさない。
	plain := TacticEntry{Market: "JP"}
	if plain.SignalLags() {
		t.Error("signal_symbol が無ければずらさない")
	}

	// 同じ市場ならずらさない。
	sameMarket := TacticEntry{Market: "JP", SignalSymbol: "1306.T"}
	if sameMarket.SignalLags() {
		t.Error("同じ市場ならずらさない")
	}
}

func TestBuildAppliesParams(t *testing.T) {
	dir := writeConfig(t, minimalHeader+`
[[tactics]]
id = "段階型"
tactic = "stack_ladder"
symbols = ["1306.T"]
multipliers = { 2 = 1.5, 6 = 3.0 }
monthly_budget = 10_000
window = { start = "12:30", end = "13:00" }
`)
	cfg, err := LoadAccumConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	tac, err := cfg.Tactics[0].Build()
	if err != nil {
		t.Fatal(err)
	}
	// 設定した段表が説明に出る（既定 3→×1.5, 5→×2, 6→×4 ではない）。
	if got := tac.Describe(); got != "stack_ladder(2→×1.5, 6→×3)" {
		t.Errorf("段表が反映されていない: %q", got)
	}
	if got := tac.Window().Describe(); got != "12:30〜13:00 JST" {
		t.Errorf("発注時間帯が反映されていない: %q", got)
	}
}
