package registry

import (
	"errors"
	"strings"
	"testing"
)

type strategy struct{ fast int }

func newRegistry(t *testing.T) *Registry[*strategy] {
	t.Helper()
	r := New[*strategy]("戦略")
	r.MustRegister("sma_cross", "移動平均のクロス。\n2 行目は無視される", func(params map[string]any) (*strategy, error) {
		fast, ok := params["fast"].(int)
		if !ok {
			return nil, errors.New("fast が要ります")
		}
		return &strategy{fast: fast}, nil
	})
	return r
}

func TestRegisterAndCreate(t *testing.T) {
	r := newRegistry(t)
	if r.Len() != 1 || !r.Contains("sma_cross") {
		t.Fatalf("登録できていない: %v", r.Available())
	}
	s, err := r.Create("sma_cross", map[string]any{"fast": 25})
	if err != nil {
		t.Fatal(err)
	}
	if s.fast != 25 {
		t.Errorf("fast = %d", s.fast)
	}
}

func TestUnknownNameListsCandidates(t *testing.T) {
	r := newRegistry(t)
	_, err := r.Create("bear_stack", nil)
	if err == nil {
		t.Fatal("未知の名前は弾くべき")
	}
	// 候補を添えていないと、設定を直す手がかりが無い
	if !strings.Contains(err.Error(), "sma_cross") || !strings.Contains(err.Error(), "戦略") {
		t.Errorf("エラー文 = %s", err)
	}
}

func TestBadParamsAreWrapped(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Create("sma_cross", nil); err == nil || !strings.Contains(err.Error(), "パラメータが不正") {
		t.Fatalf("エラー = %v", err)
	}
}

func TestDuplicateRegistrationFails(t *testing.T) {
	r := newRegistry(t)
	// 同名が通ると、意図しない実体が選ばれて静かに違う売買をしてしまう
	if err := r.Register("sma_cross", "", func(map[string]any) (*strategy, error) { return nil, nil }); err == nil {
		t.Fatal("同名の二重登録は弾くべき")
	}
	if err := r.Register("", "", func(map[string]any) (*strategy, error) { return nil, nil }); err == nil {
		t.Fatal("名前無しは弾くべき")
	}
	if err := r.Register("x", "", nil); err == nil {
		t.Fatal("生成関数 nil は弾くべき")
	}
}

func TestSummaryOfTakesFirstLine(t *testing.T) {
	if got := SummaryOf("**移動平均**の ``クロス``。\n続き"); got != "移動平均の クロス。" {
		t.Errorf("SummaryOf = %q", got)
	}
	if got := SummaryOf("   "); got != "" {
		t.Errorf("空の説明 = %q", got)
	}
	r := newRegistry(t)
	if got := r.SummaryOf("sma_cross"); got != "移動平均のクロス。" {
		t.Errorf("登録簿の説明 = %q", got)
	}
	if got := r.SummaryOf("unknown"); got != "" {
		t.Errorf("未知の説明 = %q", got)
	}
}

func TestDescribeIsSorted(t *testing.T) {
	r := newRegistry(t)
	r.MustRegister("bear_stack", "下降トレンド", func(map[string]any) (*strategy, error) { return &strategy{}, nil })
	described := r.Describe()
	if len(described) != 2 || described[0].Name != "bear_stack" {
		t.Fatalf("Describe = %v", described)
	}
}
