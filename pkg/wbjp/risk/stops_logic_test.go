package risk

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func decPtr(s string) *decimal.Decimal {
	d := dec(s)
	return &d
}

func intPtr(v int) *int { return &v }

func pos(symbol, qty, cost, last string) domain.Position {
	return domain.Position{
		Symbol:            symbol,
		Quantity:          dec(qty),
		AvailableQuantity: dec(qty),
		CostPrice:         dec(cost),
		LastPrice:         dec(last),
	}
}

// --- Ensure ---------------------------------------------------------------

func TestEnsureUsesInitialStopPct(t *testing.T) {
	sb := NewStopBook(nil)
	positions := map[string]domain.Position{"7203": pos("7203", "100", "1000", "1000")}
	// ATR を渡していても initial_stop_pct が優先される（-4% = 960）。
	sb.EnsureWithOptions(positions, map[string]decimal.Decimal{"7203": dec("50")}, "2026-09-03",
		EnsureOptions{ATRMultiple: dec("2"), InitialStopPct: decPtr("0.04")})

	stop, ok := sb.Get("7203")
	if !ok {
		t.Fatal("ストップが作られていない")
	}
	if !stop.StopPrice.Equal(dec("960")) {
		t.Errorf("initial_stop_pct が効いていない: %s", stop.StopPrice)
	}
}

func TestEnsureWithoutATRSkips(t *testing.T) {
	sb := NewStopBook(nil)
	sb.EnsureWithOptions(map[string]domain.Position{"7203": pos("7203", "100", "1000", "1000")},
		nil, "2026-09-03", EnsureOptions{ATRMultiple: dec("2")})
	if sb.Len() != 0 {
		t.Error("ATR が無い銘柄に勝手なストップを置いてはいけない")
	}
}

func TestEnsureTrailingATRMultiple(t *testing.T) {
	sb := NewStopBook(nil)
	// 初期は狭く（1.5）、追従は広く（2.5）。
	sb.EnsureWithOptions(map[string]domain.Position{"7203": pos("7203", "100", "1000", "1000")},
		map[string]decimal.Decimal{"7203": dec("20")}, "2026-09-03",
		EnsureOptions{ATRMultiple: dec("1.5"), Trailing: true, TrailingATRMultiple: decPtr("2.5")})

	stop, _ := sb.Get("7203")
	if !stop.StopPrice.Equal(dec("970")) {
		t.Errorf("初期ストップは ATR×1.5: %s", stop.StopPrice)
	}
	if !stop.ATRMultiple.Equal(dec("2.5")) {
		t.Errorf("追従倍率は trailing_atr_multiple: %s", stop.ATRMultiple)
	}
}

func TestEnsureOptionsFromWiresConfig(t *testing.T) {
	opts := EnsureOptionsFrom(wbjpcfg.StopsConfig{
		Trailing:            true,
		InitialStopPct:      decPtr("0.04"),
		TrailingATRMultiple: decPtr("2.5"),
		TrailingPct:         decPtr("0.08"),
	}, dec("1.5"))
	if !opts.Trailing || opts.InitialStopPct == nil || opts.TrailingATRMultiple == nil ||
		opts.TrailingPct == nil || !opts.ATRMultiple.Equal(dec("1.5")) {
		t.Errorf("[stops] が Ensure に結線されていない: %+v", opts)
	}
}

// --- トレーリング ---------------------------------------------------------

func TestUpdateTrailingUsesPercent(t *testing.T) {
	sb := NewStopBook(nil)
	sb.EnsureWithOptions(map[string]domain.Position{"7203": pos("7203", "100", "1000", "1000")},
		map[string]decimal.Decimal{"7203": dec("20")}, "2026-09-03",
		EnsureOptions{ATRMultiple: dec("2"), Trailing: true, TrailingPct: decPtr("0.08")})

	// 終値 1200 → 最高終値 1200 の -8% = 1104。ATR ではなく % で追従する。
	sb.UpdateTrailing(map[string]decimal.Decimal{"7203": dec("1200")}, map[string]decimal.Decimal{"7203": dec("20")})
	stop, _ := sb.Get("7203")
	if !stop.StopPrice.Equal(dec("1104")) {
		t.Errorf("trailing_pct が効いていない: %s", stop.StopPrice)
	}

	// 下がっても引き下げない
	sb.UpdateTrailing(map[string]decimal.Decimal{"7203": dec("1150")}, nil)
	stop, _ = sb.Get("7203")
	if !stop.StopPrice.Equal(dec("1104")) {
		t.Errorf("ストップを引き下げてはいけない: %s", stop.StopPrice)
	}
}

// --- 建値ストップ ---------------------------------------------------------

func TestUpdateBreakeven(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", StopPrice: dec("960"), EntryPrice: dec("1000"),
		CreatedOn: "2026-09-03", InitialStopPrice: decPtr("960")})

	// 1R = 40 円。+30 円（0.75R）ではまだ動かさない。
	sb.UpdateBreakeven(map[string]decimal.Decimal{"7203": dec("1030")}, decPtr("1"))
	if stop, _ := sb.Get("7203"); !stop.StopPrice.Equal(dec("960")) {
		t.Errorf("0.75R で建値に上げてはいけない: %s", stop.StopPrice)
	}

	// +40 円（1R）で建値へ。
	sb.UpdateBreakeven(map[string]decimal.Decimal{"7203": dec("1040")}, decPtr("1"))
	if stop, _ := sb.Get("7203"); !stop.StopPrice.Equal(dec("1000")) {
		t.Errorf("1R 到達で建値に上がるべき: %s", stop.StopPrice)
	}
}

func TestUpdateBreakevenDisabled(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", StopPrice: dec("960"), EntryPrice: dec("1000"), InitialStopPrice: decPtr("960")})
	sb.UpdateBreakeven(map[string]decimal.Decimal{"7203": dec("2000")}, nil)
	if stop, _ := sb.Get("7203"); !stop.StopPrice.Equal(dec("960")) {
		t.Error("breakeven_after_r 未設定なら何もしないはず")
	}
}

func TestInitialRiskIgnoresTrailedStop(t *testing.T) {
	// トレーリングで縮んだ現在のストップを使うと R が膨らみ利確が早まる。
	stop := Stop{EntryPrice: dec("1000"), StopPrice: dec("990"), InitialStopPrice: decPtr("960")}
	if !stop.InitialRisk().Equal(dec("40")) {
		t.Errorf("1R は建値 − 初期ストップ: %s", stop.InitialRisk())
	}
	if !stop.RiskPerShare().Equal(dec("10")) {
		t.Errorf("RiskPerShare は現在のストップまでの距離: %s", stop.RiskPerShare())
	}
}

// --- 時間切れ -------------------------------------------------------------

func TestTimeExitTargets(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", StopPrice: dec("960"), EntryPrice: dec("1000"), CreatedOn: "2026-09-03"})
	sb.Set(Stop{Symbol: "6758", StopPrice: dec("960"), EntryPrice: dec("1000"), CreatedOn: "2026-09-03"})
	closes := map[string]decimal.Decimal{"7203": dec("990"), "6758": dec("1200")}

	// 2026-09-17 は 10 営業日後。含み益なしの 7203 だけが stale で落ちる。
	targets := sb.TimeExitTargets(closes, "2026-09-17", intPtr(10), intPtr(40))
	if len(targets) != 1 || targets[0].Symbol != "7203" {
		t.Fatalf("stale_exit_days が効いていない: %+v", targets)
	}

	// max_hold_days に達すれば含み益があっても落ちる。
	targets = sb.TimeExitTargets(closes, "2026-09-17", nil, intPtr(10))
	if len(targets) != 2 {
		t.Fatalf("max_hold_days が効いていない: %+v", targets)
	}

	// どちらも nil なら何もしない。
	if got := sb.TimeExitTargets(closes, "2026-09-17", nil, nil); got != nil {
		t.Errorf("無効時は何も返さない: %+v", got)
	}
}

// --- 2 段階利確 -----------------------------------------------------------

func TestTakeProfitTargetsScalesOut(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", StopPrice: dec("960"), EntryPrice: dec("1000"),
		CreatedOn: "2026-09-03", InitialStopPrice: decPtr("960"), InitialQuantity: decPtr("300")})

	closes := map[string]decimal.Decimal{"7203": dec("1080")} // +80 円 = 2R
	qty := map[string]decimal.Decimal{"7203": dec("300")}
	lots := map[string]decimal.Decimal{"7203": dec("100")}

	targets := sb.TakeProfitTargets(closes, qty, lots, decPtr("2"), dec("0.5"), dec("100"))
	if len(targets) != 1 {
		t.Fatalf("利確目標が作られていない: %+v", targets)
	}
	// 300 株の 50% を残す → 単元丸めで 100 株
	if !targets[0].Quantity.Equal(dec("100")) {
		t.Errorf("残り株数: %s", targets[0].Quantity)
	}

	stop, _ := sb.Get("7203")
	if !stop.ScaledOut {
		t.Error("ScaledOut が書き換わっていない（デッドフィールドのまま）")
	}
	if !stop.StopPrice.Equal(dec("1000")) {
		t.Errorf("利確と同時にストップを建値へ上げるべき: %s", stop.StopPrice)
	}

	// 二度目は発動しない
	if got := sb.TakeProfitTargets(closes, qty, lots, decPtr("2"), dec("0.5"), dec("100")); len(got) != 0 {
		t.Errorf("利確は一度きり: %+v", got)
	}
}

func TestTakeProfitTargetsBelowTarget(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", StopPrice: dec("960"), EntryPrice: dec("1000"),
		InitialStopPrice: decPtr("960"), InitialQuantity: decPtr("300")})
	got := sb.TakeProfitTargets(map[string]decimal.Decimal{"7203": dec("1040")},
		map[string]decimal.Decimal{"7203": dec("300")}, nil, decPtr("2"), dec("0.5"), dec("100"))
	if len(got) != 0 {
		t.Errorf("1R では利確しない: %+v", got)
	}
	if stop, _ := sb.Get("7203"); stop.ScaledOut {
		t.Error("未達なのに ScaledOut を立ててはいけない")
	}
}

func TestTakeProfitTargetsDisabled(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", StopPrice: dec("960"), EntryPrice: dec("1000"),
		InitialStopPrice: decPtr("960"), InitialQuantity: decPtr("300")})
	if got := sb.TakeProfitTargets(map[string]decimal.Decimal{"7203": dec("2000")},
		map[string]decimal.Decimal{"7203": dec("300")}, nil, nil, dec("0.5"), dec("100")); got != nil {
		t.Errorf("take_profit_r 未設定なら何もしない: %+v", got)
	}
}

// --- ランナー -------------------------------------------------------------

func TestRunnerTargets(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", EntryPrice: dec("1000"), StopPrice: dec("1000"), ScaledOut: true})
	sb.Set(Stop{Symbol: "6758", EntryPrice: dec("1000"), StopPrice: dec("960")}) // 利確前
	qty := map[string]decimal.Decimal{"7203": dec("100"), "6758": dec("200")}

	// 移動平均を上回っていれば「残数を維持」を明示する。
	// これが無いと翌日サイジングが満額まで買い戻してしまう。
	targets := sb.RunnerTargets(map[string]decimal.Decimal{"7203": dec("1100"), "6758": dec("1100")},
		qty, map[string]decimal.Decimal{"7203": dec("1050"), "6758": dec("1050")}, false)
	if len(targets) != 1 || targets[0].Symbol != "7203" || !targets[0].Quantity.Equal(dec("100")) {
		t.Fatalf("利確済みの銘柄だけ維持されるべき: %+v", targets)
	}

	// 移動平均割れで残りを手仕舞い
	targets = sb.RunnerTargets(map[string]decimal.Decimal{"7203": dec("1000")},
		qty, map[string]decimal.Decimal{"7203": dec("1050")}, false)
	if len(targets) != 1 || !targets[0].Quantity.IsZero() {
		t.Fatalf("移動平均割れで 0 株になるべき: %+v", targets)
	}

	// always なら利確前の建玉にも適用する
	targets = sb.RunnerTargets(map[string]decimal.Decimal{"7203": dec("1100"), "6758": dec("1000")},
		qty, map[string]decimal.Decimal{"7203": dec("1050"), "6758": dec("1050")}, true)
	if len(targets) != 2 {
		t.Fatalf("always では両方が対象: %+v", targets)
	}
	if targets[0].Symbol != "6758" || !targets[0].Quantity.IsZero() {
		t.Errorf("6758 は移動平均割れで手仕舞い: %+v", targets[0])
	}
}

// --- 優先順位 -------------------------------------------------------------

func TestApplyStopPriority(t *testing.T) {
	strategy := []domain.TargetPosition{
		{Symbol: "7203", Quantity: dec("300"), Reason: "戦略: 買い増し"},
		{Symbol: "9984", Quantity: dec("100"), Reason: "戦略: 新規"},
	}
	stops := []domain.TargetPosition{{Symbol: "7203", Quantity: decimal.Zero, Reason: "ストップ抵触"}}

	merged := ApplyStopPriority(strategy, stops)
	if len(merged) != 2 {
		t.Fatalf("銘柄数: %+v", merged)
	}
	// 銘柄コード順に並ぶ（map の反復順に左右されない）
	if merged[0].Symbol != "7203" || !merged[0].Quantity.IsZero() {
		t.Errorf("ストップが戦略に勝つべき: %+v", merged[0])
	}
	if merged[1].Symbol != "9984" || !merged[1].Quantity.Equal(dec("100")) {
		t.Errorf("抵触していない銘柄は戦略のまま: %+v", merged[1])
	}
}

func TestTriggered(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", StopPrice: dec("960"), EntryPrice: dec("1000")})
	sb.Set(Stop{Symbol: "6758", StopPrice: dec("960"), EntryPrice: dec("1000")})
	got := sb.Triggered(map[string]decimal.Decimal{"7203": dec("960"), "6758": dec("961")})
	if len(got) != 1 {
		t.Fatalf("抵触は 7203 だけ（<= で判定）: %+v", got)
	}
	if _, ok := got["7203"]; !ok {
		t.Error("7203 が抵触扱いになっていない")
	}
}
