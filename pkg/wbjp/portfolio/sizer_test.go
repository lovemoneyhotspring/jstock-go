package portfolio

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func sig(symbol string, direction float64) domain.CombinedSignal {
	return domain.CombinedSignal{Symbol: symbol, Direction: direction}
}

func held(symbol, qty, cost string) domain.Position {
	return domain.Position{
		Symbol:            symbol,
		Quantity:          dec(qty),
		AvailableQuantity: dec(qty),
		CostPrice:         dec(cost),
		LastPrice:         dec(cost),
	}
}

func equalWeightCfg(maxPositions int) wbjpcfg.SizingConfig {
	return wbjpcfg.SizingConfig{
		Method:          "equal_weight",
		RiskPerTrade:    dec("0.01"),
		ATRStopMultiple: dec("2"),
		FixedNotional:   decimal.NewFromInt(300_000),
		MaxPositions:    maxPositions,
	}
}

func baseCtx() SizingContext {
	return SizingContext{
		Equity:      decimal.NewFromInt(1_000_000),
		BuyingPower: decimal.NewFromInt(1_000_000),
		Prices: map[string]decimal.Decimal{
			"1111": dec("1000"), "2222": dec("1000"), "3333": dec("1000"), "4444": dec("1000"),
		},
		ATR:            map[string]decimal.Decimal{"1111": dec("20"), "2222": dec("20"), "3333": dec("20"), "4444": dec("20")},
		DefaultLotSize: decimal.NewFromInt(100),
	}
}

func targetsBySymbol(targets []domain.TargetPosition) map[string]domain.TargetPosition {
	m := make(map[string]domain.TargetPosition, len(targets))
	for _, t := range targets {
		m[t.Symbol] = t
	}
	return m
}

// --- max_positions --------------------------------------------------------

func TestSizeLimitsNewEntriesToMaxPositions(t *testing.T) {
	sizer, err := NewSizer(equalWeightCfg(2))
	if err != nil {
		t.Fatal(err)
	}
	signals := map[string]domain.CombinedSignal{
		"1111": sig("1111", 0.9), "2222": sig("2222", 0.8),
		"3333": sig("3333", 0.7), "4444": sig("4444", 0.6),
	}
	targets := sizer.Size(signals, baseCtx(), 0.3, 0.1)
	if len(targets) != 2 {
		t.Fatalf("max_positions = 2 なのに %d 銘柄建てようとしている: %+v", len(targets), targets)
	}
	// シグナルの強い順に採る
	if targets[0].Symbol != "1111" || targets[1].Symbol != "2222" {
		t.Errorf("強い順に採るべき: %+v", targets)
	}
}

func TestSizeExistingHoldingsConsumeSlots(t *testing.T) {
	sizer, _ := NewSizer(equalWeightCfg(2))
	ctx := baseCtx()
	ctx.Positions = map[string]domain.Position{"4444": held("4444", "100", "1000")}

	signals := map[string]domain.CombinedSignal{
		"1111": sig("1111", 0.9), "2222": sig("2222", 0.8), "4444": sig("4444", 0.5),
	}
	got := targetsBySymbol(sizer.Size(signals, ctx, 0.3, 0.1))
	if _, ok := got["4444"]; !ok {
		t.Error("保有中の銘柄が結果から消えている")
	}
	if _, ok := got["1111"]; !ok {
		t.Error("残り 1 枠は最強シグナルに割り当てるべき")
	}
	if _, ok := got["2222"]; ok {
		t.Errorf("保有が枠を消費していない: %+v", got)
	}
}

func TestSizeFullBookBlocksAllNewEntries(t *testing.T) {
	sizer, _ := NewSizer(equalWeightCfg(1))
	ctx := baseCtx()
	ctx.Positions = map[string]domain.Position{"4444": held("4444", "100", "1000")}
	got := targetsBySymbol(sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.9)}, ctx, 0.3, 0.1))
	if _, ok := got["1111"]; ok {
		t.Error("保有上限に達しているのに新規を建てた")
	}
}

// --- exit_threshold / ヒステリシス ----------------------------------------

func TestSizeExitBelowExitThreshold(t *testing.T) {
	sizer, _ := NewSizer(equalWeightCfg(5))
	ctx := baseCtx()
	ctx.Positions = map[string]domain.Position{"1111": held("1111", "100", "1000")}

	got := targetsBySymbol(sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.05)}, ctx, 0.3, 0.1))
	if !got["1111"].Quantity.IsZero() {
		t.Errorf("exit_threshold を下回れば手仕舞い: %+v", got["1111"])
	}
}

func TestSizeHoldsBetweenThresholds(t *testing.T) {
	// 閾値の間は現状維持。ここで売買すると往復売買で手数料に削られる。
	sizer, _ := NewSizer(equalWeightCfg(5))
	ctx := baseCtx()
	ctx.Positions = map[string]domain.Position{"1111": held("1111", "100", "1000")}

	got := targetsBySymbol(sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.2)}, ctx, 0.3, 0.1))
	if !got["1111"].Quantity.Equal(dec("100")) {
		t.Errorf("保有継続になっていない: %+v", got["1111"])
	}
}

func TestSizeNoEntryBetweenThresholds(t *testing.T) {
	sizer, _ := NewSizer(equalWeightCfg(5))
	got := sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.2)}, baseCtx(), 0.3, 0.1)
	if len(got) != 0 {
		t.Errorf("entry_threshold 未満で新規建てしてはいけない: %+v", got)
	}
}

func TestSizeExitsVanishedSignal(t *testing.T) {
	// シグナルが届かなかった保有銘柄を落とすと「消えた建玉」が放置される。
	sizer, _ := NewSizer(equalWeightCfg(5))
	ctx := baseCtx()
	ctx.Positions = map[string]domain.Position{"1111": held("1111", "100", "1000")}

	got := targetsBySymbol(sizer.Size(nil, ctx, 0.3, 0.1))
	if !got["1111"].Quantity.IsZero() {
		t.Errorf("シグナル消滅で手仕舞いになるべき: %+v", got["1111"])
	}
	if got["1111"].Reason == "" {
		t.Error("理由が残っていない")
	}
}

// --- 保有中の再サイジング抑制 ---------------------------------------------

func TestSizeDoesNotResizeHeldPositions(t *testing.T) {
	// 毎日計算し直すと ATR や資産の変化ぶんが「意図しない部分売買」として
	// 板に出る。強いシグナルが続いても株数は動かさない。
	sizer, _ := NewSizer(equalWeightCfg(5))
	ctx := baseCtx()
	ctx.Equity = decimal.NewFromInt(5_000_000) // 資産が 5 倍になっても
	ctx.Positions = map[string]domain.Position{"1111": held("1111", "100", "1000")}

	got := targetsBySymbol(sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.9)}, ctx, 0.3, 0.1))
	if !got["1111"].Quantity.Equal(dec("100")) {
		t.Errorf("保有中に株数を計算し直してはいけない: %+v", got["1111"])
	}
}

// --- 方式ごとの株数 -------------------------------------------------------

func TestSizeEqualWeightCapsByBuyingPower(t *testing.T) {
	sizer, _ := NewSizer(equalWeightCfg(2))
	ctx := baseCtx()
	ctx.Equity = decimal.NewFromInt(1_000_000) // 1 枠 50 万円
	ctx.BuyingPower = decimal.NewFromInt(300_000)

	got := targetsBySymbol(sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.9)}, ctx, 0.3, 0.1))
	// 買付余力 30 万 × 0.99 = 297,000 → 1000 円で 297 株 → 単元丸めで 200 株
	if !got["1111"].Quantity.Equal(dec("200")) {
		t.Errorf("買付余力で頭を押さえるべき: %+v", got["1111"])
	}
}

func TestSizeAtrRisk(t *testing.T) {
	cfg := equalWeightCfg(5)
	cfg.Method = "atr_risk"
	sizer, _ := NewSizer(cfg)

	got := targetsBySymbol(sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.9)}, baseCtx(), 0.3, 0.1))
	// 許容損失 100万×1% = 1万円 / 損切り幅 ATR20×2 = 40 → 250 株 → 単元丸めで 200 株
	if !got["1111"].Quantity.Equal(dec("200")) {
		t.Errorf("atr_risk の株数: %+v", got["1111"])
	}
}

func TestSizeAtrRiskSkipsMissingATR(t *testing.T) {
	cfg := equalWeightCfg(5)
	cfg.Method = "atr_risk"
	sizer, _ := NewSizer(cfg)
	ctx := baseCtx()
	ctx.ATR = nil

	if got := sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.9)}, ctx, 0.3, 0.1); len(got) != 0 {
		t.Errorf("ATR が無い銘柄は見送るべき: %+v", got)
	}
}

func TestSizeFixedNotional(t *testing.T) {
	cfg := equalWeightCfg(5)
	cfg.Method = "fixed_notional"
	sizer, _ := NewSizer(cfg)

	got := targetsBySymbol(sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.9)}, baseCtx(), 0.3, 0.1))
	// 30 万円 / 1000 円 = 300 株
	if !got["1111"].Quantity.Equal(dec("300")) {
		t.Errorf("fixed_notional の株数: %+v", got["1111"])
	}
}

func TestSizeSkipsBelowOneLot(t *testing.T) {
	cfg := equalWeightCfg(5)
	cfg.Method = "fixed_notional"
	cfg.FixedNotional = decimal.NewFromInt(10_000)
	sizer, _ := NewSizer(cfg)

	if got := sizer.Size(map[string]domain.CombinedSignal{"1111": sig("1111", 0.9)}, baseCtx(), 0.3, 0.1); len(got) != 0 {
		t.Errorf("単元株に満たないなら見送る: %+v", got)
	}
}

func TestNewSizerRejectsUnknownMethod(t *testing.T) {
	if _, err := NewSizer(wbjpcfg.SizingConfig{Method: "martingale"}); err == nil {
		t.Error("未知の方式は起動時に弾くべき")
	}
}

func TestSizeIsDeterministic(t *testing.T) {
	// map の反復順に左右されると、同じ入力でも日によって採用銘柄が変わる。
	sizer, _ := NewSizer(equalWeightCfg(2))
	signals := map[string]domain.CombinedSignal{
		"1111": sig("1111", 0.5), "2222": sig("2222", 0.5),
		"3333": sig("3333", 0.5), "4444": sig("4444", 0.5),
	}
	first := sizer.Size(signals, baseCtx(), 0.3, 0.1)
	for i := 0; i < 20; i++ {
		got := sizer.Size(signals, baseCtx(), 0.3, 0.1)
		if len(got) != len(first) {
			t.Fatalf("結果の件数が揺れた: %+v vs %+v", first, got)
		}
		for j := range got {
			if got[j].Symbol != first[j].Symbol {
				t.Fatalf("採用銘柄が揺れた: %+v vs %+v", first, got)
			}
		}
	}
}
