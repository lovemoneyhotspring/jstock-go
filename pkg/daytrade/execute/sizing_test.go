package execute

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/margincap"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func yenOf(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

// sizingConfig はロング 300 万 N3・ショート 300 万 N3（どちらも 1 注文 100 万）。
func sizingConfig(margin, spill bool) config.Config {
	cfg := config.Default()
	cfg.Capital.MaxCapital, cfg.Capital.OrderBudget, cfg.Capital.MaxPositions = yenOf(3_000_000), yenOf(1_000_000), 10
	cfg.Margin.Enabled = margin
	cfg.Margin.MaxCapital, cfg.Margin.OrderBudget = yenOf(3_000_000), yenOf(1_000_000)
	cfg.Margin.MultiplierNormal, cfg.Margin.MultiplierLongWeak = decimal.NewFromInt(1), decimal.NewFromInt(1)
	cfg.Margin.SpillToLong = spill
	return cfg
}

func tradeDay() regime.Verdict {
	return regime.Verdict{Trade: true, Scale: 1, ShockLong: 1, ShockShort: 1}
}

func shortPick(amount int64) selection.Pick {
	return selection.Pick{Symbol: "S", Side: domain.SideSell, Price: yenOf(amount / 100), Quantity: yenOf(100)}
}

func TestSizeDay(t *testing.T) {
	cases := []struct {
		name       string
		cfg        config.Config
		verdict    regime.Verdict
		placed     Placed
		tiedLong   int64
		watchOnly  bool
		shortPicks []selection.Pick
		wantLong   Sizing
		wantShort  *Sizing
		wantSpill  int64
		wantDone   bool
	}{
		{name: "1 回目", cfg: sizingConfig(false, false), verdict: tradeDay(),
			wantLong: Sizing{N: 3, Budget: yenOf(1_000_000)}},
		{name: "再実行で N を建て終えている", cfg: sizingConfig(false, false), verdict: tradeDay(),
			placed:   Placed{Long: 3, LongAmount: yenOf(3_000_000)},
			wantLong: Sizing{N: 0, Budget: yenOf(1_000_000)}, wantDone: true},
		{name: "再実行で残り 2 件", cfg: sizingConfig(false, false), verdict: tradeDay(),
			placed:   Placed{Long: 1, LongAmount: yenOf(1_000_000)},
			wantLong: Sizing{N: 2, Budget: yenOf(1_000_000)}},
		// 候補が 2 銘柄しか無く 1 銘柄 150 万で建てた回の再実行。件数では 1 件残るが資金は使い切っている
		{name: "再実行で件数は残るが資金を使い切っている", cfg: sizingConfig(false, false), verdict: tradeDay(),
			placed:   Placed{Long: 2, LongAmount: yenOf(3_000_000)},
			wantLong: Sizing{N: 0, Budget: yenOf(1_000_000)}},
		{name: "様子見", cfg: func() config.Config { c := sizingConfig(true, true); c.Capital.MaxCapital = decimal.Zero; return c }(),
			verdict: tradeDay(), watchOnly: true,
			wantLong: Sizing{N: watchRowsForTest, Budget: yenOf(1_000_000), Weighting: "equal"}},
		{name: "弱い日は縮める", cfg: sizingConfig(false, false),
			verdict:  regime.Verdict{Trade: true, Scale: 0.5, ScaleReason: "損益 → 縮小", ShockLong: 1, ShockShort: 1},
			wantLong: Sizing{N: 3, Budget: yenOf(500_000)}},
		{name: "持ち越しの拘束", cfg: sizingConfig(false, false), verdict: tradeDay(), tiedLong: 1_500_000,
			wantLong: Sizing{N: 1, Budget: yenOf(1_000_000)}},
		// 1 回目: ショートが 100 万しか使わなければ余り 200 万で N=5（selection.SpillInto と同じ）
		{name: "余りをロングへ（1 回目）", cfg: sizingConfig(true, true), verdict: tradeDay(),
			shortPicks: []selection.Pick{shortPick(1_000_000)},
			wantLong:   Sizing{N: 5, Budget: yenOf(1_000_000)}, wantShort: &Sizing{N: 3, Budget: yenOf(1_000_000)},
			wantSpill: 2_000_000},
		// 前の回が余りで 4 件建て、N（3）を超えている。1 件は通らなかった → 残り 1 件
		{name: "前の回の余りが N を超えた再実行", cfg: sizingConfig(true, true), verdict: tradeDay(),
			placed:    Placed{Long: 4, LongAmount: yenOf(4_000_000), Short: 3, ShortAmount: yenOf(1_000_000)},
			wantLong:  Sizing{N: 1, Budget: yenOf(1_000_000)},
			wantSpill: 2_000_000},
		// 前の回: ショート 1 件（150 万）+ ロング 2 件（225 万）で締め切り。再実行でショートの候補なし。
		// 余りは 300 − 150 = 150 万（「残り 2 件 × 100 万 = 200 万」を数え直さない）→ 1 日 4 件 × 112.5 万、
		// 残り 2 件。長短の合計がショートの枠を超えない
		{name: "余りの再実行", cfg: sizingConfig(true, true), verdict: tradeDay(),
			placed:    Placed{Long: 2, LongAmount: yenOf(2_250_000), Short: 1, ShortAmount: yenOf(1_500_000)},
			wantLong:  Sizing{N: 2, Budget: yenOf(1_125_000)},
			wantShort: &Sizing{N: 1, Budget: yenOf(1_000_000)},
			wantSpill: 1_500_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := SizingInput{Cfg: c.cfg, Verdict: c.verdict, Placed: c.placed, TiedLong: yenOf(c.tiedLong),
				WatchOnly: c.watchOnly, WatchRows: watchRowsForTest}
			d := SizeDay(in)
			long, spill, _ := d.WithSpill(c.shortPicks)
			wantWeighting := c.wantLong.Weighting
			if wantWeighting == "" {
				wantWeighting = c.cfg.Capital.Weighting
			}
			if long.N != c.wantLong.N || !long.Budget.Equal(c.wantLong.Budget) || long.Weighting != wantWeighting {
				t.Errorf("ロング = %+v, want %+v", long, c.wantLong)
			}
			if !spill.Equal(yenOf(c.wantSpill)) {
				t.Errorf("余り = %s, want %d", spill, c.wantSpill)
			}
			if c.wantShort != nil && (!d.ShortOpen || d.Short.N != c.wantShort.N || !d.Short.Budget.Equal(c.wantShort.Budget)) {
				t.Errorf("ショート = %+v（open=%v）, want %+v", d.Short, d.ShortOpen, *c.wantShort)
			}
			if got := DoneForToday(c.cfg, c.placed, c.watchOnly); got != c.wantDone {
				t.Errorf("DoneForToday = %v, want %v", got, c.wantDone)
			}
			if c.cfg.Margin.SpillToLong && !c.watchOnly {
				// 長短の合計は「ロングの枠 + ショートの枠」を超えない
				total := c.placed.LongAmount.Add(c.placed.ShortAmount).
					Add(long.Budget.Mul(decimal.NewFromInt(int64(long.N))))
				for _, pk := range c.shortPicks {
					total = total.Add(pk.Amount())
				}
				if limit := c.cfg.Capital.MaxCapital.Add(c.cfg.Margin.MaxCapital); total.GreaterThan(limit) {
					t.Errorf("長短の合計 %s が枠 %s を超える", total, limit)
				}
			}
		})
	}
}

// ショートだけ休む日（regime.us_skip_legs = "short"）はショートを建てず、ロングは通常どおり。
// 余りもロングへ回さない（ショック日のショート 0 倍と同じ扱い）
func TestSizeDayShortOff(t *testing.T) {
	v := tradeDay()
	v.ShortOff, v.ShortOffReason = true, "前夜の S&P500 が小幅高 → ショートだけ休む"
	d := SizeDay(SizingInput{Cfg: sizingConfig(true, true), Verdict: v, WatchRows: watchRowsForTest})
	if d.ShortOpen || !d.ShortMultiplier.IsZero() {
		t.Errorf("ショートを建てようとしている: open=%v multiplier=%s", d.ShortOpen, d.ShortMultiplier)
	}
	long, spill, _ := d.WithSpill(nil)
	if long.N != 3 || !long.Budget.Equal(yenOf(1_000_000)) || !spill.IsZero() {
		t.Errorf("ロング = %+v 余り %s, want N=3 / 100 万 / 余り 0", long, spill)
	}
}

// ショートの一時停止（margin.paused）: ショートは建てず、枠（300 万）を毎日ロングへ回す。
// 「ショートだけ休む」日も回す。ショック日の倍率 0 は回す元が無い。再実行は回した分を建て終えていれば何もしない
func TestSizeDayPaused(t *testing.T) {
	cfg := sizingConfig(true, true)
	cfg.Margin.Paused = true
	shortOff := tradeDay()
	shortOff.ShortOff, shortOff.ShortOffReason = true, "前夜の S&P500 が小幅高 → ショートだけ休む"
	shock := tradeDay()
	shock.Shock, shock.ShockShort = true, 0
	for _, c := range []struct {
		name      string
		verdict   regime.Verdict
		placed    Placed
		wantN     int
		wantSpill int64
	}{
		{name: "通常の日", verdict: tradeDay(), wantN: 6, wantSpill: 3_000_000},
		{name: "ショートだけ休む日", verdict: shortOff, wantN: 6, wantSpill: 3_000_000},
		{name: "ショック日", verdict: shock, wantN: 3, wantSpill: 0},
		{name: "再実行で回した分まで建て終えている", verdict: tradeDay(),
			placed: Placed{Long: 6, LongAmount: yenOf(6_000_000)}, wantN: 0, wantSpill: 3_000_000},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := SizeDay(SizingInput{Cfg: cfg, Verdict: c.verdict, Placed: c.placed})
			if d.ShortOpen || d.Short.N != 0 {
				t.Errorf("ショートを建てようとしている: open=%v short=%+v", d.ShortOpen, d.Short)
			}
			long, spill, _ := d.WithSpill(nil)
			if long.N != c.wantN || !spill.Equal(yenOf(c.wantSpill)) {
				t.Errorf("ロング N=%d 余り %s, want N=%d 余り %d", long.N, spill, c.wantN, c.wantSpill)
			}
			if _, short := Remaining(cfg, c.placed, false); short != 0 {
				t.Errorf("一時停止中のショートの残り = %d", short)
			}
		})
	}
}

// 1 回目（建てた分が無い）の件数と予算は、引く前の式（selection.SpillInto / CapByTied）と同じ。
// バックテストと evaluate の再構成が同じ式を使っているので、ここがずれると検証と本番が食い違う。
func TestSizeDayFirstRunMatchesBacktestFormula(t *testing.T) {
	cfg := sizingConfig(true, true)
	for _, used := range []int64{0, 400_000, 1_000_000, 2_999_000} {
		d := SizeDay(SizingInput{Cfg: cfg, Verdict: tradeDay()})
		var picks []selection.Pick
		if used > 0 {
			picks = []selection.Pick{shortPick(used)}
		}
		long, spill, _ := d.WithSpill(picks)
		wantN, wantBudget := selection.SpillInto(3, yenOf(1_000_000), yenOf(1_000_000), yenOf(3_000_000-used), 10)
		if long.N != wantN || !long.Budget.Equal(wantBudget) || !spill.Equal(yenOf(3_000_000-used)) {
			t.Errorf("used %d: (%d, %s, spill %s), want (%d, %s)", used, long.N, long.Budget, spill, wantN, wantBudget)
		}
	}
}

// 建てた銘柄を落とした後に「寄っている」銘柄を落とす。元の気配から落とし直すと
// 建てた銘柄が候補に戻り、同じ日に重ねて建てる。
func TestRankQuotesKeepsExclusionsWithSkipOpened(t *testing.T) {
	quotes := map[string]selection.Quote{
		"A": {Symbol: "A", Opened: true},
		"B": {Symbol: "B"}, // 今日建てた
		"C": {Symbol: "C"}, // 台帳外として返済に回した
		"D": {Symbol: "D"},
	}
	placed := map[string]domain.Side{"B": domain.SideBuy}
	swept := map[string]struct{}{"C": {}}
	for _, skipOpened := range []bool{true, false} {
		kept, opened := RankQuotes(quotes, placed, swept, skipOpened)
		if _, ok := kept["B"]; ok {
			t.Errorf("skip_opened=%v: 建てた銘柄が候補に戻った", skipOpened)
		}
		if _, ok := kept["C"]; ok {
			t.Errorf("skip_opened=%v: 返済に回した銘柄が候補に戻った", skipOpened)
		}
		if _, ok := kept["D"]; !ok {
			t.Errorf("skip_opened=%v: 残すべき銘柄が落ちた", skipOpened)
		}
		_, hasA := kept["A"]
		if skipOpened && (hasA || len(opened) != 1 || opened[0] != "A") {
			t.Errorf("寄っている銘柄の除外: kept=%v opened=%v", kept, opened)
		}
		if !skipOpened && (!hasA || len(opened) != 0) {
			t.Errorf("skip_opened=false で落としている: kept=%v opened=%v", kept, opened)
		}
	}
}

const watchRowsForTest = 5

// TestSizeDayLiveConfigPaused は**本番の設定そのまま**（config/daytrade_margin）で、ショートの
// 一時停止中に建てる形を固定する。仮の設定だけで試すと、order_budget や max_capital を変えた
// ときに「枠がロングへ回る」形が崩れても気付けない（2026-09-18 のレビュー）。
func TestSizeDayLiveConfigPaused(t *testing.T) {
	cfg, err := config.Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if !cfg.Margin.Paused {
		t.Skip("margin.paused = false（停止を解除したら、この試験は通常日の形に書き替える）")
	}
	normal := regime.Verdict{Trade: true, Scale: 1}
	shortOff := regime.Verdict{Trade: true, Scale: 1, ShortOff: true, ShortOffReason: "米国小幅高"}
	shock := regime.Verdict{Trade: true, Scale: 1, Shock: true, ShockLong: 1.5, ShockShort: 0}
	for _, c := range []struct {
		name                  string
		verdict               regime.Verdict
		wantN                 int
		wantBudget, wantSpill int64
		wantTotalAtMost       int64
	}{
		// 規則 R（weighting = turnover）: N = max_positions = 10 で、N × 1 注文が総額。
		// 枠 200 万がロングに回って総額 700 万（長短合計は停止前と同じ）。保証金の比
		// （margin.capacity_ratio）は open の applyMarginCap が当てるので、ここは設定の値のまま
		{name: "通常の日", verdict: normal, wantN: 10, wantBudget: 699_999, wantSpill: 1_999_998, wantTotalAtMost: 6_999_990},
		// 米国小幅高の日もショートは停止のまま倍率を残すので、通常日と同じ形
		{name: "米国小幅高の日", verdict: shortOff, wantN: 10, wantBudget: 699_999, wantSpill: 1_999_998, wantTotalAtMost: 6_999_990},
		// ショック日はショート ×0 なので回す枠が無い（ロング 500 万 × 1.5 = 750 万を 10 位まで）
		{name: "ショック日", verdict: shock, wantN: 10, wantBudget: 750_000, wantSpill: 0, wantTotalAtMost: 7_500_000},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := SizeDay(SizingInput{Cfg: cfg, Verdict: c.verdict})
			if d.ShortOpen {
				t.Errorf("一時停止中なのにショートを建てようとしている: %+v", d.Short)
			}
			long, spill, _ := d.WithSpill(nil)
			if long.N != c.wantN || !long.Budget.Equal(yenOf(c.wantBudget)) || !spill.Equal(yenOf(c.wantSpill)) {
				t.Errorf("ロング N=%d 1 注文 %s 余り %s, want N=%d 1 注文 %d 余り %d",
					long.N, long.Budget, spill, c.wantN, c.wantBudget, c.wantSpill)
			}
			total := long.Budget.Mul(decimal.NewFromInt(int64(long.N)))
			if total.GreaterThan(yenOf(c.wantTotalAtMost)) {
				t.Errorf("建玉の合計 %s が %d を超えた", total, c.wantTotalAtMost)
			}
		})
	}
}

// 規則 R ＋ margin.capacity_ratio: 朝の保証金で決め直した設定から、平日（ショートの枠を回した後）も
// ショック日（×1.5 の後）も、ロングの総額が保証金から導いた上限を超えない。2026-09-24 朝の建可能額で確かめる。
func TestSizeDayRatioStaysWithinCapacity(t *testing.T) {
	base, err := config.Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if !base.Margin.CapacityRatio.IsPositive() || !base.Margin.Paused {
		t.Skip("capacity_ratio が無いか、ショートの停止を解除した設定（通常日の形に書き替える）")
	}
	for _, c := range []struct {
		name      string
		sinkidate int64
		verdict   regime.Verdict
		wantTotal int64 // ロングの総額（N × 1 注文）
		atMost    int64
	}{
		// 平日: ロング 494 万 + 回したショート 198 万 = 691 万（上限 = 建可能額 11,150,590 × 62% = 6,913,365）
		{name: "平日", sinkidate: 11_150_590, verdict: regime.Verdict{Trade: true, Scale: 1},
			wantTotal: 6_913_350, atMost: 6_913_365},
		// ショック日: ロング 494 万 × 1.5 = 741 万 < 上限 803 万（ショートは ×0 で回す枠なし）
		{name: "ショック日", sinkidate: 11_150_590, verdict: regime.Verdict{Trade: true, Scale: 1, Shock: true, ShockLong: 1.5},
			wantTotal: 7_407_170, atMost: 8_028_424},
		// 天井に達した口座のショック日: ロング 800 万 × 1.5 = 1,200 万 → 天井 1,000 万で頭打ち
		{name: "天井のショック日", sinkidate: 20_000_000, verdict: regime.Verdict{Trade: true, Scale: 1, Shock: true, ShockLong: 1.5},
			wantTotal: 10_000_000, atMost: 10_000_000},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, res := margincap.Apply(base, margincap.Snapshot{Day: "2026-09-24", SinyouSinkidate: yenOf(c.sinkidate)})
			if !res.Ratio {
				t.Fatalf("比で決め直していない: %+v", res)
			}
			d := SizeDay(SizingInput{Cfg: cfg, Verdict: c.verdict})
			long, _, _ := d.WithSpill(nil)
			total := long.Budget.Mul(decimal.NewFromInt(int64(long.N)))
			if long.N != base.Capital.MaxPositions {
				t.Errorf("N = %d, want max_positions %d", long.N, base.Capital.MaxPositions)
			}
			if !total.Equal(yenOf(c.wantTotal)) {
				t.Errorf("ロングの総額 %s（N %d × %s）, want %d", total, long.N, long.Budget, c.wantTotal)
			}
			if total.GreaterThan(yenOf(c.atMost)) {
				t.Errorf("ロングの総額 %s が保証金の上限 %d を超えた", total, c.atMost)
			}
		})
	}
}

// 規則 R の再実行: 1 件でも建てた日は余り（上限で頭打ち・載らない銘柄を飛ばした残り）で買い足さない。
// 寄る前の回が丸ごと失敗した日（建てた分 0）は満額で建てる。
func TestSizeDayTurnoverRerunDoesNotTopUp(t *testing.T) {
	cfg, err := config.Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if cfg.Capital.Weighting != config.WeightingTurnover {
		t.Skip("規則 R でない設定")
	}
	normal := regime.Verdict{Trade: true, Scale: 1}
	after := SizeDay(SizingInput{Cfg: cfg, Verdict: normal, Placed: Placed{Long: 10, LongAmount: yenOf(6_860_000)}})
	long, _, _ := after.WithSpill(nil)
	if long.N != 0 || after.Long.N != 0 {
		t.Errorf("建てた後の回が N=%d（余り回し後 %d）, want 0", after.Long.N, long.N)
	}
	first := SizeDay(SizingInput{Cfg: cfg, Verdict: normal})
	long, _, _ = first.WithSpill(nil)
	if long.N != cfg.Capital.MaxPositions {
		t.Errorf("建てていない回が N=%d, want %d", long.N, cfg.Capital.MaxPositions)
	}
}

// 規則 R で寄る前の回が一部しか送れなかった朝、9:00 の回の「残り」はロング 0（買い足さないので）。
// 件数の差（10 − 7 = 3）を出すと、点検で「3 件建て損ねた」と読み違える（2026-09-25 のレビュー）
func TestRemainingTurnoverPartialPreopen(t *testing.T) {
	cfg, err := config.Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if cfg.Capital.Weighting != config.WeightingTurnover {
		t.Skip("規則 R でない設定")
	}
	partial := Placed{Long: 7, LongAmount: yenOf(4_800_000)}
	if long, _ := Remaining(cfg, partial, false); long != 0 {
		t.Errorf("一部建てた後の残り %d, want 0", long)
	}
	if long, _ := Remaining(cfg, Placed{}, false); long != cfg.Capital.MaxPositions {
		t.Errorf("建てていない回の残り %d, want %d", long, cfg.Capital.MaxPositions)
	}
	// 9:00 の回は「済み」で抜けない（気配と順位表を残す）
	if DoneForToday(cfg, partial, false) {
		t.Error("一部建てた後の回が発注済みで抜けた（順位表と open_run が残らない）")
	}
	eq := cfg
	eq.Capital.Weighting = "equal"
	if long, _ := Remaining(eq, partial, false); long != eq.Capital.Positions()-7 {
		t.Errorf("等金額の残り %d, want %d", long, eq.Capital.Positions()-7)
	}
}

// 規則 R の予算は総額と 1 銘柄の上限で見せる（1 注文 = 総額 ÷ N は実際の金額でないため）
func TestLongBudgetText(t *testing.T) {
	r := config.Capital{Weighting: config.WeightingTurnover, NameDivisor: 7}
	if got, want := LongBudgetText(r, 10, yenOf(758_240)), "総額 7,582,400 円・1 銘柄まで 1,083,200 円"; got != want {
		t.Errorf("規則 R: %q, want %q", got, want)
	}
	if got, want := LongBudgetText(config.Capital{Weighting: "equal"}, 3, yenOf(1_666_666)), "1 注文 1,666,666 円"; got != want {
		t.Errorf("等金額: %q, want %q", got, want)
	}
}
