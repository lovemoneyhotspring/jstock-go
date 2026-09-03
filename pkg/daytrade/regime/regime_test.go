package regime

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/shopspring/decimal"
)

func day(month, d int) time.Time { return time.Date(2026, time.Month(month), d, 0, 0, 0, 0, time.UTC) }

func f(v float64) *float64 { return &v }

func TestSkipMonths(t *testing.T) {
	cfg := config.Default().Regime
	cfg.SkipMonths = []int{12}
	if v := Evaluate(cfg, Signals{Day: day(12, 1)}); v.Trade {
		t.Error("12 月に取引を止めていない")
	}
	if v := Evaluate(cfg, Signals{Day: day(11, 30)}); !v.Trade {
		t.Errorf("11 月を止めている: %v", v.Reasons)
	}
}

func TestIVGate(t *testing.T) {
	cfg := config.Default().Regime
	cfg.IVGate = decimal.NewFromInt(18)
	if v := Evaluate(cfg, Signals{Day: day(6, 1), IVPrev: f(15)}); v.Trade {
		t.Error("低 IV で取引を止めていない")
	}
	if v := Evaluate(cfg, Signals{Day: day(6, 1), IVPrev: f(20)}); !v.Trade {
		t.Errorf("高 IV を止めている: %v", v.Reasons)
	}
	// IV が取れない日はゲートを効かせない
	if v := Evaluate(cfg, Signals{Day: day(6, 1)}); !v.Trade {
		t.Errorf("IV 不明で止めている: %v", v.Reasons)
	}
}

func TestDriftGateAndBigGapOverride(t *testing.T) {
	cfg := config.Default().Regime
	gate := decimal.RequireFromString("-0.0003")
	cfg.DriftGate = &gate
	// ドリフトが閾値以下 → 止める
	if v := Evaluate(cfg, Signals{Day: day(6, 1), Drift: f(-0.001)}); v.Trade {
		t.Error("負のドリフトで止めていない")
	}
	// 市場ギャップが ±1% を超える日は例外（急落の寄付が最も効く）
	v := Evaluate(cfg, Signals{Day: day(6, 1), Drift: f(-0.001), MarketGap: f(-0.02)})
	if !v.Trade {
		t.Errorf("大ギャップの例外が効いていない: %v", v.Reasons)
	}
	// ちょうど 1% は「超える」ではないので例外にならない
	v = Evaluate(cfg, Signals{Day: day(6, 1), Drift: f(-0.001), MarketGap: f(0.01)})
	if v.Trade {
		t.Error("市場ギャップちょうど 1% で例外にしている")
	}
}

func TestEquityCurveShrinksRatherThanStops(t *testing.T) {
	cfg := config.Default().Regime
	cfg.EquityCurveDays = 20
	cfg.EquityCurveScale = decimal.RequireFromString("0.5")
	v := Evaluate(cfg, Signals{Day: day(6, 1), RecentPnL: f(-50000)})
	if !v.Trade {
		t.Error("資産曲線では休まず縮めるはず")
	}
	if v.Scale != 0.5 || !v.Weak() {
		t.Errorf("Scale = %v, Weak = %v", v.Scale, v.Weak())
	}
	if v.ScaleReason == "" {
		t.Error("縮めた理由が空")
	}
	// scale = 0 なら休む
	cfg.EquityCurveScale = decimal.Zero
	if v := Evaluate(cfg, Signals{Day: day(6, 1), RecentPnL: f(-1)}); v.Trade {
		t.Error("scale 0 で休んでいない")
	}
	// 損益がプラスなら縮めない
	cfg.EquityCurveScale = decimal.RequireFromString("0.5")
	if v := Evaluate(cfg, Signals{Day: day(6, 1), RecentPnL: f(1)}); v.Weak() {
		t.Error("黒字なのに縮めている")
	}
}

func TestUsGateAndVixOverride(t *testing.T) {
	cfg := config.Default().Regime
	high := decimal.RequireFromString("0.01")
	cfg.UsSkipHigh = &high
	// 小幅高（0〜+1%）× 低 VIX → 休む
	if v := Evaluate(cfg, Signals{Day: day(6, 1), UsRet: f(0.005), Vix: f(15)}); v.Trade {
		t.Error("小幅高の翌日を止めていない")
	}
	// 高 VIX なら例外
	if v := Evaluate(cfg, Signals{Day: day(6, 1), UsRet: f(0.005), Vix: f(30)}); !v.Trade {
		t.Errorf("VIX の例外が効いていない: %v", v.Reasons)
	}
	// 帯の外（大幅高）は止めない
	if v := Evaluate(cfg, Signals{Day: day(6, 1), UsRet: f(0.02), Vix: f(15)}); !v.Trade {
		t.Errorf("大幅高を止めている: %v", v.Reasons)
	}
	// 下落した翌日は止めない
	if v := Evaluate(cfg, Signals{Day: day(6, 1), UsRet: f(-0.01), Vix: f(15)}); !v.Trade {
		t.Errorf("下落の翌日を止めている: %v", v.Reasons)
	}
}

func TestNotesAlwaysPresent(t *testing.T) {
	// 診断値は「毎朝すべて計算してログに残す」ので、ゲートが無効でも欠けてはいけない
	v := Evaluate(config.Default().Regime, Signals{Day: day(6, 1), Drift: f(0.0001)})
	for _, key := range []string{"month", "iv_prev", "drift_bp", "market_gap_bp", "recent_pnl", "us_ret_bp", "vix", "scale"} {
		if _, ok := v.Notes[key]; !ok {
			t.Errorf("診断値 %s が無い", key)
		}
	}
	if v.Notes["drift_bp"] != 1.0 {
		t.Errorf("drift_bp = %v, want 1", v.Notes["drift_bp"])
	}
}

func TestMarketGapOf(t *testing.T) {
	if MarketGapOf(nil) != nil {
		t.Error("空なら nil のはず")
	}
	if got := *MarketGapOf([]float64{-0.03, -0.01, -0.02}); got != -0.02 {
		t.Errorf("中央値 = %v, want -0.02", got)
	}
	// 偶数個は中間 2 つの平均
	if got := *MarketGapOf([]float64{-0.04, -0.02}); got != -0.03 {
		t.Errorf("中央値 = %v, want -0.03", got)
	}
}
