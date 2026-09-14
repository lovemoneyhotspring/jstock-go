package risk

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

// risingBars は終値 1, 2, …, n（安値は終値 − 0.5）の足。
func risingBars(n int) []domain.Bar {
	bars := make([]domain.Bar, n)
	for i := range bars {
		c := decimal.NewFromInt(int64(i + 1))
		bars[i] = domain.Bar{Close: c, Low: c.Sub(decimal.RequireFromString("0.5")), High: c}
	}
	return bars
}

func decEq(t *testing.T, name string, got *decimal.Decimal, want string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil（期待 %s）", name, want)
		return
	}
	if !got.Equal(decimal.RequireFromString(want)) {
		t.Errorf("%s: %s（期待 %s）", name, got, want)
	}
}

func TestRegimeInputFromBars(t *testing.T) {
	cfg := wbjpcfg.RegimeConfig{SMALong: 10, SMAMid: 5, SlopeLookback: 5}
	in := RegimeInputFromBars(risingBars(30), cfg)
	decEq(t, "close", in.Close, "30")
	decEq(t, "long", in.LongMA, "25.5") // 21..30 の平均
	decEq(t, "mid", in.MidMA, "28")     // 26..30 の平均
	decEq(t, "slope", in.Slope, "5")    // 25.5 − 20.5（16..25 の平均）
}

// TestRegimeInputSlopeBoundary は、傾きの基準位置が長期線の最初の有効値ちょうどなら
// 傾きを出し、1 本でも手前（ウォームアップ中）・範囲外なら出さない（落ちない）こと。
func TestRegimeInputSlopeBoundary(t *testing.T) {
	bars := risingBars(30) // 長期線（10 本）の最初の有効値は 9 番目、末尾は 29 番目
	cases := []struct {
		lookback  int
		wantSlope string // 空なら nil
	}{
		{0, "0"},
		{20, "20"}, // 29 − 20 = 9: 最初の有効値（5.5）との差
		{21, ""},   // 8: ウォームアップ中
		{29, ""},   // 0
		{30, ""},   // −1: 範囲外
		{100, ""},
		{-1, ""}, // 設定では入らないが、範囲外を読まない
	}
	for _, c := range cases {
		in := RegimeInputFromBars(bars, wbjpcfg.RegimeConfig{SMALong: 10, SMAMid: 5, SlopeLookback: c.lookback})
		if c.wantSlope == "" {
			if in.Slope != nil {
				t.Errorf("lookback=%d: 傾きは出さないはず: %s", c.lookback, in.Slope)
			}
			continue
		}
		decEq(t, "slope", in.Slope, c.wantSlope)
	}
}

func TestRegimeInputWarmupAndEmpty(t *testing.T) {
	cfg := wbjpcfg.RegimeConfig{SMALong: 10, SMAMid: 5, SlopeLookback: 1}
	in := RegimeInputFromBars(risingBars(7), cfg)
	decEq(t, "close", in.Close, "7")
	decEq(t, "mid", in.MidMA, "5")
	if in.LongMA != nil || in.Slope != nil {
		t.Errorf("ウォームアップ中の長期線・傾きは nil: %+v", in)
	}

	if in := RegimeInputFromBars(nil, cfg); in.Close != nil || in.LongMA != nil || in.MidMA != nil || in.Slope != nil {
		t.Errorf("足が無ければ全て nil: %+v", in)
	}
	// 期間が不正なら終値だけ
	if in := RegimeInputFromBars(risingBars(30), wbjpcfg.RegimeConfig{}); in.Close == nil || in.LongMA != nil {
		t.Errorf("期間 0: %+v", in)
	}
}

func TestTrendValue(t *testing.T) {
	period := func(n int) *int { return &n }
	bars := risingBars(10)
	cases := []struct {
		name  string
		stops wbjpcfg.StopsConfig
		want  string // 空なら ok=false
	}{
		{"未設定", wbjpcfg.StopsConfig{}, ""},
		{"0 日", wbjpcfg.StopsConfig{TrendExitSMA: period(0)}, ""},
		{"sma", wbjpcfg.StopsConfig{TrendExitSMA: period(5), TrendExitKind: "sma"}, "8"},
		{"種類が空なら sma", wbjpcfg.StopsConfig{TrendExitSMA: period(4)}, "8.5"},
		{"足が期間ちょうど", wbjpcfg.StopsConfig{TrendExitSMA: period(10)}, "5.5"},
		{"足が足りない", wbjpcfg.StopsConfig{TrendExitSMA: period(11)}, ""},
		// 当日を除く過去 3 本（7, 8, 9 本目）の安値の最小 = 7 − 0.5
		{"donchian", wbjpcfg.StopsConfig{TrendExitSMA: period(3), TrendExitKind: "donchian"}, "6.5"},
		// 当日を除くので、足が期間ちょうどではまだ出ない
		{"donchian 足が期間ちょうど", wbjpcfg.StopsConfig{TrendExitSMA: period(10), TrendExitKind: "donchian"}, ""},
	}
	for _, c := range cases {
		got, ok := TrendValue(bars, c.stops)
		if c.want == "" {
			if ok {
				t.Errorf("%s: 出さないはず: %s", c.name, got)
			}
			continue
		}
		if !ok || !got.Equal(decimal.RequireFromString(c.want)) {
			t.Errorf("%s: %s ok=%v（期待 %s）", c.name, got, ok, c.want)
		}
	}

	// ema は直近に重い。終値 10 が 5 本続いたあと 20 に跳ねると sma（12）より上
	jump := make([]domain.Bar, 6)
	for i := range jump {
		c := decimal.NewFromInt(10)
		if i == 5 {
			c = decimal.NewFromInt(20)
		}
		jump[i] = domain.Bar{Close: c, Low: c, High: c}
	}
	sma, _ := TrendValue(jump, wbjpcfg.StopsConfig{TrendExitSMA: period(5)})
	ema, ok := TrendValue(jump, wbjpcfg.StopsConfig{TrendExitSMA: period(5), TrendExitKind: "ema"})
	if !sma.Equal(decimal.NewFromInt(12)) || !ok || !ema.GreaterThan(sma) {
		t.Errorf("ema=%s ok=%v sma=%s", ema, ok, sma)
	}
}
