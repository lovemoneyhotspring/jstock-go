package tactics

import "fmt"

// entry は登録簿の1件。
type entry struct {
	name    string
	summary string
	// build は既定パラメータの実体を作る。
	build func() (Tactic, error)
}

// registry は設定ファイルの tactic = "..." から戦略を引く登録簿。
//
// 一覧（accum strategies）と比較（accum compare）はこの並び順をそのまま使う。
// 説明文をコマンド側に散らすと登録簿と食い違うため、ここを唯一の出所にする。
var registry = []entry{
	{
		name:    "constant",
		summary: "定額。増額しない純粋なドル平均法。比較の基準",
		build:   func() (Tactic, error) { return &Constant{}, nil },
	},
	{
		name:    "bear_stack",
		summary: "完全下降配列（終値 < MA20 < MA50 < MA200）で増額",
		build:   func() (Tactic, error) { return NewBearStack(0, 0, 0, 0) },
	},
	{
		name:    "stack_ladder",
		summary: "弱気スコア（0〜6）に応じて段階的に増額",
		build:   func() (Tactic, error) { return NewStackLadder(nil, 0, 0, 0) },
	},
	{
		name:    "drawdown_ladder",
		summary: "過去最高値からの下落率に応じて段階的に増額",
		build:   func() (Tactic, error) { return NewDrawdownLadder(nil, nil, false, 0) },
	},
}

// Available は登録済みの戦略名を定義順で返す。
func Available() []string {
	names := make([]string, 0, len(registry))
	for _, e := range registry {
		names = append(names, e.name)
	}
	return names
}

// Summary は戦略の一行説明を返す。
func Summary(name string) string {
	for _, e := range registry {
		if e.name == name {
			return e.summary
		}
	}
	return ""
}

// Create は既定パラメータの戦略を作る。
func Create(name string) (Tactic, error) {
	for _, e := range registry {
		if e.name == name {
			return e.build()
		}
	}
	return nil, fmt.Errorf("未知の戦略です: %q（%v）", name, Available())
}

// CreateAll は登録済みの全戦略を既定パラメータで作る。比較に使う。
func CreateAll() ([]Tactic, error) {
	out := make([]Tactic, 0, len(registry))
	for _, e := range registry {
		t, err := e.build()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.name, err)
		}
		out = append(out, t)
	}
	return out, nil
}
