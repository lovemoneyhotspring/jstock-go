package main

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// 米国小幅高の日を寄る前の寄指だけで取引する設定（execution.preopen_limit_pct_us_low）では、
// 9:00 以降の回は見送りになる。寄る前の回・平常日・設定が無い日は判定を変えない。
func TestUsLowPreopenOnly(t *testing.T) {
	cfg := dtconfig.Default()
	cfg.Execution.PreopenLimitPctUsLow = decimal.RequireFromString("1.5")
	usLow := regime.Verdict{Trade: true, UsLow: true, ShortOff: true, ShortOffReason: "小幅高 → ショートだけ休む"}

	if got := usLowPreopenOnly(cfg, usLow, true); !got.Trade {
		t.Error("寄る前の回まで見送りにした")
	}
	got := usLowPreopenOnly(cfg, usLow, false)
	if got.Trade || len(got.Reasons) != 1 {
		t.Errorf("9:00 以降の回を見送りにしていない: trade=%v reasons=%v", got.Trade, got.Reasons)
	}
	if got := usLowPreopenOnly(cfg, regime.Verdict{Trade: true}, false); !got.Trade {
		t.Error("平常日の 9:00 以降の回を見送りにした")
	}
	cfg.Execution.PreopenLimitPctUsLow = decimal.Zero
	if got := usLowPreopenOnly(cfg, usLow, false); !got.Trade {
		t.Error("設定が無いのに見送りにした（us_skip_legs = short の従来の形を変えてはいけない）")
	}
}

// 米国小幅高の日は、寄指の指値を作れない銘柄（前日終値なし）を寄成で出さずに落とす。
func TestDropWithoutOpeningLimit(t *testing.T) {
	cfg := dtconfig.Default()
	cfg.Execution.EntryWindow = []string{"08:59", "09:15"}
	cfg.Execution.PreopenLegs = dtconfig.PreopenLegsLong
	cfg.Regime.UsSkipLegs = dtconfig.UsSkipLegsShort
	cfg.Execution.PreopenLimitPctUsLow = decimal.RequireFromString("1.5")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Execution = cfg.Execution.ForDay(true)
	picks := []selection.Pick{
		{Symbol: "7203", Side: domain.SideBuy, PrevClose: decimal.NewFromInt(1000)},
		{Symbol: "6758", Side: domain.SideBuy},
	}
	got := dropWithoutOpeningLimit(picks, cfg)
	if len(got) != 1 || got[0].Symbol != "7203" {
		t.Errorf("残った銘柄 = %v, want 7203 だけ", got)
	}
}

// 小幅高の日の寄指の位置は**発注が読む env.Cfg** に届いていなければならない。届いていないと、
// この日のロングが黙って寄成で出る（applyDayConfig の env への代入を消すとここが落ちる）。
func TestApplyDayConfigReachesEntryRequest(t *testing.T) {
	cfg := dtconfig.Default()
	cfg.Execution.EntryWindow = []string{"08:59", "09:15"}
	cfg.Execution.PreopenLegs = dtconfig.PreopenLegsLong
	cfg.Regime.UsSkipLegs = dtconfig.UsSkipLegsShort
	cfg.Signal.RankByUsLow = dtconfig.RankByGap
	cfg.Execution.PreopenLimitPctUsLow = decimal.RequireFromString("1.5")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	pick := selection.Pick{Symbol: "7203", Side: domain.SideBuy, Quantity: decimal.NewFromInt(100), PrevClose: decimal.NewFromInt(1000)}

	env := execute.Env{Cfg: cfg}
	got := applyDayConfig(cfg, &env, true)
	if got.Signal.RankBy != dtconfig.RankByGap {
		t.Errorf("小幅高の日の並べ方 = %q", got.Signal.RankBy)
	}
	req := execute.EntryRequest(pick, day, env.Cfg, 0, true)
	// 1000 × 0.985 = 985
	if req.OrderType != domain.OrderTypeLimit || req.LimitPrice == nil || !req.LimitPrice.Equal(decimal.NewFromInt(985)) ||
		req.Condition != domain.ConditionOpening {
		t.Fatalf("小幅高の日の寄る前の注文 = %s / %v / %q, want LIMIT / 985 / OPENING", req.OrderType, req.LimitPrice, req.Condition)
	}

	// 平常日は寄成のまま
	env = execute.Env{Cfg: cfg}
	applyDayConfig(cfg, &env, false)
	req = execute.EntryRequest(pick, day, env.Cfg, 0, true)
	if req.OrderType != domain.OrderTypeMarket || req.LimitPrice != nil || req.Condition != domain.ConditionOpening {
		t.Errorf("平常日の寄る前の注文 = %s / %v / %q, want MARKET / nil / OPENING", req.OrderType, req.LimitPrice, req.Condition)
	}
}
