package strategy

import (
	"strings"
	"testing"
)

func TestAllStrategiesAreRegistered(t *testing.T) {
	want := []string{"atr_breakout", "level_bounce", "margin_balance", "momentum_rank", "ross_cameron", "rsi_pullback", "rsi_reversion", "sma_cross", "trend_pullback"}
	got := Available()
	if len(got) != len(want) {
		t.Fatalf("登録数 = %d (%v), 期待 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %s, 期待 %s", i, got[i], want[i])
		}
	}
}

// 設定ファイルのパラメータが実体に届くこと。switch でのハードコードに
// 戻ると、ここが既定値のまま通ってしまう。
func TestParamsReachTheStrategy(t *testing.T) {
	s, err := Create("atr_breakout", map[string]any{
		"channel":       int64(30),
		"atr_period":    int64(21),
		"min_atr_ratio": 0.02,
	})
	if err != nil {
		t.Fatal(err)
	}
	ab, ok := s.(*ATRBreakout)
	if !ok {
		t.Fatalf("型が違う: %T", s)
	}
	if ab.Channel != 30 || ab.ATRPeriod != 21 || ab.MinATRRatio != 0.02 {
		t.Errorf("パラメータが届いていない: %+v", ab)
	}
}

func TestUnknownParamIsRejected(t *testing.T) {
	// 綴りを間違えたキーが黙って無視されると、設定したつもりの値で動かない。
	_, err := Create("sma_cross", map[string]any{"fastt": int64(10)})
	if err == nil || !strings.Contains(err.Error(), "知らないパラメータ") {
		t.Fatalf("エラー = %v", err)
	}
}

func TestInvalidParamTypeIsRejected(t *testing.T) {
	_, err := Create("sma_cross", map[string]any{"fast": "25"})
	if err == nil || !strings.Contains(err.Error(), "整数") {
		t.Fatalf("エラー = %v", err)
	}
}

func TestInvalidCombinationIsRejected(t *testing.T) {
	if _, err := Create("sma_cross", map[string]any{"fast": int64(75), "slow": int64(25)}); err == nil {
		t.Error("fast >= slow は弾くべき")
	}
	if _, err := Create("momentum_rank", map[string]any{"rebalance": "yearly"}); err == nil {
		t.Error("未知の rebalance は弾くべき")
	}
}

func TestUnknownStrategyListsCandidates(t *testing.T) {
	_, err := Create("bear_stack", nil)
	if err == nil || !strings.Contains(err.Error(), "sma_cross") {
		t.Fatalf("候補を添えるべき: %v", err)
	}
}

func TestEveryStrategyBuildsWithDefaults(t *testing.T) {
	for _, name := range Available() {
		s, err := Create(name, nil)
		if err != nil {
			t.Fatalf("%s: 既定値で作れない: %v", name, err)
		}
		if s.Name() != name {
			t.Errorf("%s: Name() = %s", name, s.Name())
		}
		if s.WarmupBars() < 1 {
			t.Errorf("%s: WarmupBars = %d", name, s.WarmupBars())
		}
		if s.Describe() == "" {
			t.Errorf("%s: Describe が空", name)
		}
	}
}
