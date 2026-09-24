package config

import (
	"strings"
	"testing"
)

// 設定は daytrade と同じ厳格さで読む（2026-09-24 のレビュー C1）。
// 綴りを誤った項目・読めない数値を既定に倒すと、効いているつもりの設定が効かないまま
// 本番に乗る。kill_switch の打ち間違いなら緊急停止が効かない。
func TestLoadRejectsUnknownAndUnreadable(t *testing.T) {
	const tactic = `
[[tactics]]
id = "x"
tactic = "constant"
symbols = ["1306.T"]
`
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "最上位の未知の項目（kill_switch の打ち間違い）",
			body: "kil_switch = true\n" + minimalHeader + tactic,
			want: "kil_switch",
		},
		{
			name: "execution の未知の項目",
			body: minimalHeader + "lot_size_override = { \"452A.T\" = 10 }\n" + tactic,
			want: "lot_size_override",
		},
		{
			name: "tactics の未知の項目",
			body: minimalHeader + tactic + "multipler = 4\n",
			want: "multipler",
		},
		{
			name: "予算が数値として読めない",
			body: minimalHeader + tactic + "monthly_budget = \"25,000\"\n",
			want: "monthly_budget",
		},
		{
			name: "予算が真偽値",
			body: minimalHeader + tactic + "monthly_budget = true\n",
			want: "monthly_budget",
		},
		{
			name: "予算を 0 と書いた（既定に倒さない）",
			body: minimalHeader + tactic + "monthly_budget = 0\n",
			want: "monthly_budget",
		},
		{
			name: "既定の予算が読めない",
			body: "monthly_budget = \"abc\"\n[execution]\nbroker = \"paper\"\n" + tactic,
			want: "monthly_budget",
		},
		{
			name: "kill_switch が文字列",
			body: "kill_switch = \"true\"\n" + minimalHeader + tactic,
			want: "kill_switch",
		},
		{
			name: "market の打ち間違い",
			body: minimalHeader + tactic + "market = \"Jp\"\n",
			want: "market",
		},
		{
			name: "signal_market の打ち間違い",
			body: minimalHeader + tactic + "signal_symbol = \"^GSPC\"\nsignal_market = \"USA\"\n",
			want: "signal_market",
		},
		{
			name: "口座区分の打ち間違い",
			body: minimalHeader + "tax_account_type = \"TOKUTEI\"\n" + tactic,
			want: "tax_account_type",
		},
		{
			name: "売買単位が 0",
			body: minimalHeader + "lot_size_overrides = { \"452A.T\" = 0 }\n" + tactic,
			want: "lot_size_overrides",
		},
		{
			name: "同じ銘柄の売買単位を 2 つの表記で書いた",
			body: minimalHeader + "lot_size_overrides = { \"452A.T\" = 10, \"452A\" = 1 }\n" + tactic,
			want: "2 回",
		},
		{
			name: "バスケットの未知の項目",
			body: minimalHeader + tactic + "\n[[baskets]]\nid = \"b\"\nweights = { \"1306.T\" = 1.0 }\nwieghts = 1\n",
			want: "wieghts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadAccumConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("エラーになるべき")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("エラーに %q を含むべき: %v", tc.want, err)
			}
		})
	}
}

// 予算を書かなければ既定を使う（厳格にしても省略は許す）。文字列の "25_000" も読める。
func TestLoadBudgetDefaultsAndStrings(t *testing.T) {
	cfg, err := LoadAccumConfig(writeConfig(t, minimalHeader+`
[[tactics]]
id = "省略"
tactic = "constant"
symbols = ["1306.T"]

[[tactics]]
id = "文字列"
tactic = "constant"
symbols = ["1305.T"]
monthly_budget = "12_345"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Tactics[0].MonthlyBudget.String(); got != "25000" {
		t.Errorf("省略した予算 = %s, want 25000（最上位の monthly_budget）", got)
	}
	if got := cfg.Tactics[1].MonthlyBudget.String(); got != "12345" {
		t.Errorf("文字列の予算 = %s, want 12345", got)
	}
}

// 売買単位の上書きは、設定の表記（"452A.T"）でも発注の表記（"452A"）でも引ける。
// 以前は "452A.T" のキーを "452A" で引いて一度も当たらず、上書きが効いていなかった（A5）。
func TestLotSizeForMatchesBothNotations(t *testing.T) {
	e := ExecutionConfig{LotSizeOverrides: map[string]int{"452A.T": 10, "563A": 1}}
	cases := []struct {
		symbol string
		want   int
		ok     bool
	}{
		{"452A", 10, true},
		{"452A.T", 10, true},
		{"563A", 1, true},
		{"563A.T", 1, true},
		{"2559", 0, false},
	}
	for _, tc := range cases {
		got, ok := e.LotSizeFor(tc.symbol)
		if got != tc.want || ok != tc.ok {
			t.Errorf("LotSizeFor(%q) = %d, %v, want %d, %v", tc.symbol, got, ok, tc.want, tc.ok)
		}
	}
}

// 本番が読む設定（config/accum/accum.toml）が厳格な読み込みを通ること。
// 通らないと、次に bin を作り直した時点で accum sync / evaluate / run が全部止まる。
func TestRepositoryConfigLoadsStrictly(t *testing.T) {
	cfg, err := LoadAccumConfig("../../../config")
	if err != nil {
		t.Fatalf("本番の設定が読めません: %v", err)
	}
	if cfg.KillSwitch {
		t.Log("kill_switch = true（発注は止まっている）")
	}
	// 設定のキーは "452A.T" の表記。発注側の "452A" で当たること
	for symbol, want := range map[string]int{"452A": 10, "563A": 1, "1305": 10, "1591": 1, "1306": 10} {
		if got, ok := cfg.Execution.LotSizeFor(symbol); !ok || got != want {
			t.Errorf("LotSizeFor(%q) = %d, %v, want %d", symbol, got, ok, want)
		}
	}
	var active []string
	for _, entry := range cfg.Active() {
		active = append(active, entry.Symbols...)
	}
	if len(active) == 0 {
		t.Error("有効な銘柄が 1 つも無い")
	}
}
