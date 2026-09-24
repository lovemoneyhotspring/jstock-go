package selection

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func candidate(symbol string, prevClose float64, vol *float64) universe.Candidate {
	return universe.Candidate{
		Code: symbol + "0", Symbol: symbol, Name: symbol + " 株式会社",
		PrevClose: prevClose, Vol20: vol, Eligible: true, ShortEligible: true, Shortable: true,
	}
}

func quote(symbol string, price float64) Quote {
	return Quote{Symbol: symbol, Price: decimal.NewFromFloat(price), At: time.Now().UTC(), Source: "test"}
}

func TestSharesFor(t *testing.T) {
	lot := decimal.NewFromInt(100)
	// 予算 100 万 ÷ 1000 円 = 1000 株
	if got := SharesFor(decimal.NewFromInt(1_000_000), decimal.NewFromInt(1000), lot); !got.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("SharesFor = %s, want 1000", got)
	}
	// 単元に切り捨て（1050 株は買えない）
	if got := SharesFor(decimal.NewFromInt(1_050_000), decimal.NewFromInt(1000), lot); !got.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("単元に切り捨てられていない: %s", got)
	}
	// 1 単元に届かなければ 0
	if got := SharesFor(decimal.NewFromInt(50_000), decimal.NewFromInt(1000), lot); !got.IsZero() {
		t.Errorf("1 単元に届かないのに %s", got)
	}
}

func TestRankOrdersByGapAscending(t *testing.T) {
	cfg := config.Default().Signal
	candidates := []universe.Candidate{
		candidate("1000", 1000, nil), // gap -5%
		candidate("2000", 1000, nil), // gap -2%
		candidate("3000", 1000, nil), // gap +1% → 条件外（max_gap = 0）
	}
	quotes := map[string]Quote{
		"1000": quote("1000", 950), "2000": quote("2000", 980), "3000": quote("3000", 1010),
	}
	ranked := Rank(candidates, quotes, cfg)
	if len(ranked) != 2 {
		t.Fatalf("順位表 %d 件, want 2（ギャップアップは外れる）", len(ranked))
	}
	if ranked[0].Symbol != "1000" || ranked[1].Symbol != "2000" {
		t.Errorf("ギャップの小さい順になっていない: %s, %s", ranked[0].Symbol, ranked[1].Symbol)
	}
	if ranked[0].Rank != 1 || ranked[1].Rank != 2 {
		t.Errorf("順位が 1 始まりの連番でない: %d, %d", ranked[0].Rank, ranked[1].Rank)
	}
}

// 取得元が基準値段（前日終値）を返すなら、plan の前日終値よりそちらでギャップを出す。
// 株式分割の日は plan の値が調整前で、そのままだと −50% のギャップに見える。
func TestRankPrefersQuotePrevClose(t *testing.T) {
	cfg := config.Default().Signal
	candidates := []universe.Candidate{candidate("1000", 2000, nil)} // アーカイブは分割前の 2000 円
	split := quote("1000", 990)
	split.PrevClose = decimal.NewFromInt(1000) // ブローカーの基準値段は分割後の 1000 円
	ranked := Rank(candidates, map[string]Quote{"1000": split}, cfg)
	if len(ranked) != 1 {
		t.Fatalf("順位表 %d 件, want 1", len(ranked))
	}
	if !ranked[0].Gap.Equal(decimal.RequireFromString("-0.01")) {
		t.Errorf("ギャップ = %s, want -0.01（基準値段 1000 円で計算）", ranked[0].Gap)
	}
	if !ranked[0].PrevClose.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("前日終値 = %s, want 1000", ranked[0].PrevClose)
	}
	// 基準値段が無ければ plan の値のまま
	plain := quote("1000", 1980)
	ranked = Rank(candidates, map[string]Quote{"1000": plain}, cfg)
	if len(ranked) != 1 || !ranked[0].Gap.Equal(decimal.RequireFromString("-0.01")) {
		t.Errorf("基準値段が無いのに plan の前日終値を使っていない: %+v", ranked)
	}
}

func TestRankSkipsLimitDown(t *testing.T) {
	cfg := config.Default().Signal
	// 前日終値 1000 円の制限値幅は ±300 円 → 700 円がストップ安
	candidates := []universe.Candidate{candidate("1000", 1000, nil)}
	quotes := map[string]Quote{"1000": quote("1000", 700)}
	if got := Rank(candidates, quotes, cfg); len(got) != 0 {
		t.Errorf("ストップ安を除外していない: %+v", got)
	}
	cfg.SkipLimitDown = false
	if got := Rank(candidates, quotes, cfg); len(got) != 1 {
		t.Errorf("skip_limit_down = false でも除外している")
	}
}

func TestRankShortOrdersByGapDescending(t *testing.T) {
	m := config.Default().Margin
	m.Enabled = true
	candidates := []universe.Candidate{
		candidate("1000", 1000, nil), // gap +6%
		candidate("2000", 1000, nil), // gap +12%
		candidate("3000", 1000, nil), // gap +2% → min_gap 5% 未満で外れる
	}
	quotes := map[string]Quote{
		"1000": quote("1000", 1060), "2000": quote("2000", 1120), "3000": quote("3000", 1020),
	}
	ranked := RankShort(candidates, quotes, m)
	if len(ranked) != 2 {
		t.Fatalf("順位表 %d 件, want 2", len(ranked))
	}
	if ranked[0].Symbol != "2000" {
		t.Errorf("ギャップの大きい順になっていない: %s", ranked[0].Symbol)
	}
}

func TestRankShortSkipsLimitUp(t *testing.T) {
	m := config.Default().Margin
	m.Enabled = true
	// 1000 円の制限値幅 +300 円 → 1300 円がストップ高
	candidates := []universe.Candidate{candidate("1000", 1000, nil)}
	quotes := map[string]Quote{"1000": quote("1000", 1300)}
	if got := RankShort(candidates, quotes, m); len(got) != 0 {
		t.Errorf("ストップ高を除外していない: %+v", got)
	}
}

func TestWeightsInverseVol(t *testing.T) {
	low, high := 0.01, 0.04
	rows := []Ranked{{Vol: &low}, {Vol: &high}}
	w := Weights(rows, "inverse_vol")
	// 下限 2% が効くので 1/0.02 と 1/0.04 → 2:1
	if w[0] <= w[1] {
		t.Errorf("荒い銘柄の方が重い: %v", w)
	}
	if sum := w[0] + w[1]; sum < 0.999 || sum > 1.001 {
		t.Errorf("重みの合計が 1 でない: %f", sum)
	}
	// ボラが無い銘柄は下限で扱う（重みが暴れない）
	w = Weights([]Ranked{{Vol: nil}, {Vol: nil}}, "inverse_vol")
	if w[0] != 0.5 || w[1] != 0.5 {
		t.Errorf("ボラ無しが等分にならない: %v", w)
	}
	w = Weights(rows, "equal")
	if w[0] != 0.5 || w[1] != 0.5 {
		t.Errorf("equal が等分でない: %v", w)
	}
}

func TestPickFromTakesTopNAndSizes(t *testing.T) {
	cfg := config.Default().Signal
	candidates := []universe.Candidate{
		candidate("1000", 1000, nil),
		candidate("2000", 1000, nil),
		candidate("3000", 1000, nil),
	}
	quotes := map[string]Quote{
		"1000": quote("1000", 900), "2000": quote("2000", 950), "3000": quote("3000", 980),
	}
	ranked := Rank(candidates, quotes, cfg)
	picks := PickFrom(ranked, PickOptions{
		N: 2, Budget: decimal.NewFromInt(500_000), Weighting: "equal", Side: domain.SideBuy,
	})
	if len(picks) != 2 {
		t.Fatalf("選定 %d 件, want 2", len(picks))
	}
	if picks[0].Symbol != "1000" {
		t.Errorf("上位から取っていない: %s", picks[0].Symbol)
	}
	// 総予算 100 万 × 0.5 = 50 万 ÷ 900 円 = 555.5 → 500 株
	if !picks[0].Quantity.Equal(decimal.NewFromInt(500)) {
		t.Errorf("株数 %s, want 500", picks[0].Quantity)
	}
	if picks[0].Side != domain.SideBuy {
		t.Errorf("Side = %s", picks[0].Side)
	}
}

// 同じ 33 業種は MaxPerSector 銘柄まで。超えた銘柄は落ちて次点が繰り上がる。
// 業種が取れない銘柄（Sector が空）は数に入れない。
func TestPickFromLimitsPerSector(t *testing.T) {
	cfg := config.Default().Signal
	sector := func(c universe.Candidate, s string) universe.Candidate { c.Sector = s; return c }
	candidates := []universe.Candidate{
		sector(candidate("1000", 1000, nil), "2050"), // 建設
		sector(candidate("2000", 1000, nil), "2050"), // 建設（上限で落ちる）
		sector(candidate("3000", 1000, nil), ""),     // 業種が取れない
		sector(candidate("4000", 1000, nil), "3600"), // 機械（繰り上がる）
	}
	quotes := map[string]Quote{
		"1000": quote("1000", 900), "2000": quote("2000", 920),
		"3000": quote("3000", 940), "4000": quote("4000", 960),
	}
	ranked := Rank(candidates, quotes, cfg)
	picks := PickFrom(ranked, PickOptions{
		N: 3, Budget: decimal.NewFromInt(500_000), Weighting: "equal", Side: domain.SideBuy,
		MaxPerSector: 1,
	})
	var got []string
	for _, p := range picks {
		got = append(got, p.Symbol)
	}
	want := []string{"1000", "3000", "4000"}
	if len(got) != len(want) {
		t.Fatalf("選定 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("選定 = %v, want %v", got, want)
		}
	}
	// 上限 0（既定）なら今までどおり上位から 3 銘柄
	picks = PickFrom(ranked, PickOptions{
		N: 3, Budget: decimal.NewFromInt(500_000), Weighting: "equal", Side: domain.SideBuy,
	})
	if len(picks) != 3 || picks[1].Symbol != "2000" {
		t.Errorf("上限 0 で挙動が変わっている: %+v", picks)
	}
}

// 理由は PickFrom と同じ判定から出る。予算超え・業種の上限・N が埋まった後が、選ばれた銘柄と食い違わない。
func TestPickReasonsMatchesPickFrom(t *testing.T) {
	cfg := config.Default().Signal
	sector := func(c universe.Candidate, s string) universe.Candidate { c.Sector = s; return c }
	candidates := []universe.Candidate{
		sector(candidate("1000", 10000, nil), "3650"), // 1 単元 90 万円は予算 50 万円を超える
		sector(candidate("2000", 1000, nil), "3650"),
		sector(candidate("3000", 1000, nil), "3650"), // 業種の上限（2000 と同じ業種）
		sector(candidate("4000", 1000, nil), "3600"),
		sector(candidate("5000", 1000, nil), "3300"), // N = 2 が埋まった後
	}
	quotes := map[string]Quote{
		"1000": quote("1000", 9000), "2000": quote("2000", 910), "3000": quote("3000", 920),
		"4000": quote("4000", 930), "5000": quote("5000", 940),
	}
	ranked := Rank(candidates, quotes, cfg)
	opts := PickOptions{
		N: 2, Budget: decimal.NewFromInt(500_000), Weighting: "equal", Side: domain.SideBuy,
		MaxPerSector: 1,
	}
	picks := PickFrom(ranked, opts)
	got := PickReasons(ranked, opts, picks)
	want := map[string]string{
		"1000": ReasonOverBudget, "2000": ReasonPicked, "3000": ReasonSectorCap,
		"4000": ReasonPicked, "5000": ReasonBeyondN,
	}
	for symbol, reason := range want {
		if got[symbol] != reason {
			t.Errorf("%s: 理由 = %q, want %q（全体 %v）", symbol, got[symbol], reason, got)
		}
	}
	if len(picks) != 2 {
		t.Fatalf("選定 %d 件, want 2", len(picks))
	}
	for _, p := range picks {
		if got[p.Symbol] != ReasonPicked {
			t.Errorf("選ばれた %s の理由が picked でない: %q", p.Symbol, got[p.Symbol])
		}
	}
}

func TestPickFromSkipsUnaffordable(t *testing.T) {
	cfg := config.Default().Signal
	// 1 単元 100 万円の銘柄は予算 10 万円では買えない → 次点が繰り上がる
	candidates := []universe.Candidate{
		candidate("1000", 12000, nil),
		candidate("2000", 1000, nil),
	}
	quotes := map[string]Quote{"1000": quote("1000", 10000), "2000": quote("2000", 950)}
	ranked := Rank(candidates, quotes, cfg)
	picks := PickFrom(ranked, PickOptions{
		N: 1, Budget: decimal.NewFromInt(100_000), Weighting: "equal", Side: domain.SideBuy,
	})
	if len(picks) != 1 || picks[0].Symbol != "2000" {
		t.Errorf("予算に届かない銘柄を外していない: %+v", picks)
	}
}

// 売建は成行で出せる 50 単元で頭打ち（超える数量はブローカーが拒否する）。買いは切らない。
func TestPickFromCapsShortAtMarketLimit(t *testing.T) {
	ranked := []Ranked{{Rank: 1, Symbol: "1000", Code: "10000", PrevClose: decimal.NewFromInt(100), Price: decimal.NewFromInt(108)}}
	opts := PickOptions{N: 1, Budget: decimal.NewFromInt(670_000), Weighting: "equal", Side: domain.SideSell}
	picks := PickFrom(ranked, opts)
	if len(picks) != 1 || !picks[0].Quantity.Equal(decimal.NewFromInt(5000)) {
		t.Errorf("売建の株数 = %+v, want 5000", picks)
	}
	opts.Side = domain.SideBuy
	picks = PickFrom(ranked, opts)
	if len(picks) != 1 || !picks[0].Quantity.Equal(decimal.NewFromInt(6200)) {
		t.Errorf("買いの株数 = %+v, want 6200（上限は売建だけ）", picks)
	}
}

func TestPickFromZeroN(t *testing.T) {
	if got := PickFrom([]Ranked{{Symbol: "1000"}}, PickOptions{N: 0}); got != nil {
		t.Errorf("N=0 で選定している: %+v", got)
	}
}

func TestPickFee(t *testing.T) {
	p := Pick{Price: decimal.NewFromInt(1000), Quantity: decimal.NewFromInt(100)}
	if !p.Amount().Equal(decimal.NewFromInt(100_000)) {
		t.Errorf("Amount = %s", p.Amount())
	}
	// 10 万円の往復は 20 万円 → 176 円。片道はその半分
	if !p.Fee().Equal(decimal.NewFromInt(88)) {
		t.Errorf("Fee = %s, want 88", p.Fee())
	}
}

func TestPickFromCapsEachOrderAtMaxAmount(t *testing.T) {
	// 候補 1 銘柄・N=3・1 注文 67 万: 既定は総予算 200 万を 1 銘柄に寄せる
	ranked := []Ranked{{Rank: 1, Symbol: "1000", Price: decimal.NewFromInt(1000), Gap: decimal.RequireFromString("0.08")}}
	budget := decimal.NewFromInt(670_000)
	pooled := PickFrom(ranked, PickOptions{N: 3, Budget: budget, Weighting: "equal", Side: domain.SideSell})
	if len(pooled) != 1 || !pooled[0].Quantity.Equal(decimal.NewFromInt(2000)) {
		t.Fatalf("総予算の按分 = %v, want 2,000 株（201 万 ÷ 1,000 円）", pooled)
	}
	// 上限を付けると 1 注文は 67 万まで
	capped := PickFrom(ranked, PickOptions{N: 3, Budget: budget, Weighting: "equal", Side: domain.SideSell,
		MaxAmount: budget})
	if len(capped) != 1 || !capped[0].Quantity.Equal(decimal.NewFromInt(600)) {
		t.Fatalf("上限付き = %v, want 600 株（67 万 ÷ 1,000 円）", capped)
	}
}

// rank_by = gap_vol は「ギャップ ÷ 20 日ボラ」の小さい順。1% しか動かない銘柄の −3% は
// 5% 動く銘柄の −5% より極端（−3.0 vs −1.0）。ボラの無い銘柄は末尾。
func TestRankByGapVolNormalizesByVolatility(t *testing.T) {
	cfg := config.Default().Signal
	cfg.RankBy = config.RankByGapVol
	lowVol, highVol := 0.01, 0.05
	candidates := []universe.Candidate{
		candidate("1000", 1000, &highVol), // gap -5%, vol 5% → -1.0
		candidate("2000", 1000, &lowVol),  // gap -3%, vol 1% → -3.0（最も極端）
		candidate("3000", 1000, nil),      // gap -4%, ボラ不明 → 末尾
	}
	quotes := map[string]Quote{
		"1000": quote("1000", 950), "2000": quote("2000", 970), "3000": quote("3000", 960),
	}
	ranked := Rank(candidates, quotes, cfg)
	got := []string{ranked[0].Symbol, ranked[1].Symbol, ranked[2].Symbol}
	if got[0] != "2000" || got[1] != "1000" || got[2] != "3000" {
		t.Errorf("正規化ギャップの順になっていない: %v", got)
	}

	// 既定（gap）は生のギャップ順のまま
	cfg.RankBy = config.RankByGap
	ranked = Rank(candidates, quotes, cfg)
	if ranked[0].Symbol != "1000" || ranked[1].Symbol != "3000" || ranked[2].Symbol != "2000" {
		t.Errorf("gap ではギャップの小さい順のはず: %s %s %s", ranked[0].Symbol, ranked[1].Symbol, ranked[2].Symbol)
	}
}

func TestRankKeyRejectsMissingVol(t *testing.T) {
	if _, ok := RankKey(config.RankByGapVol, -0.03, nil); ok {
		t.Error("ボラ不明は ok=false（末尾）")
	}
	tiny := 0.001
	if k, _ := RankKey(config.RankByGapVol, -0.02, &tiny); k != -0.02/VolFloor {
		t.Errorf("ボラは VolFloor で下から押さえる: %g", k)
	}
}

func yieldOf(v float64) *float64 { return &v }

func TestByEarnYieldPicksCheapestFromPool(t *testing.T) {
	// ギャップ順に A B C D（A が最も深い）。益回りは D > B > A > C。
	// value_pool = 2 なら母数は上位 4 の A〜D で、そこから益回り上位 2 の D B を採る。
	pool := []Ranked{
		{Symbol: "A", EarnYield: yieldOf(0.03)},
		{Symbol: "B", EarnYield: yieldOf(0.06)},
		{Symbol: "C", EarnYield: yieldOf(0.01)},
		{Symbol: "D", EarnYield: yieldOf(0.09)},
	}
	got := ByEarnYield(pool, 2, 2)
	if len(got) != 2 || got[0].Symbol != "D" || got[1].Symbol != "B" {
		t.Fatalf("ByEarnYield = %v, want D B", symbolsOf(got))
	}
	// value_pool が 0 / 1 ならギャップ順のまま先頭 2 件
	for _, vp := range []int{0, 1} {
		got := ByEarnYield(pool, 2, vp)
		if len(got) != 2 || got[0].Symbol != "A" || got[1].Symbol != "B" {
			t.Errorf("value_pool=%d: %v, want A B", vp, symbolsOf(got))
		}
	}
}

func TestByEarnYieldPutsUnknownLast(t *testing.T) {
	// 益回りが取れない銘柄は末尾（判定できない銘柄を優先しない）。同値は元の順位を保つ。
	pool := []Ranked{
		{Symbol: "A"},
		{Symbol: "B", EarnYield: yieldOf(0.02)},
		{Symbol: "C", EarnYield: yieldOf(0.02)},
		{Symbol: "D", EarnYield: yieldOf(0.05)},
	}
	got := ByEarnYield(pool, 3, 2)
	if len(got) != 3 || got[0].Symbol != "D" || got[1].Symbol != "B" || got[2].Symbol != "C" {
		t.Fatalf("ByEarnYield = %v, want D B C", symbolsOf(got))
	}
}

func symbolsOf(rows []Ranked) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Symbol)
	}
	return out
}

// Keep は建て済みの銘柄だけを落とし、順位・既存規則の順位・予測値は 1 回目のまま保つ。
func TestKeepPreservesRanks(t *testing.T) {
	score := 0.7
	ranked := []Ranked{
		{Symbol: "A", Rank: 1, RuleRank: 2, Score: &score},
		{Symbol: "B", Rank: 2, RuleRank: 1},
		{Symbol: "C", Rank: 3, RuleRank: 3},
	}
	kept := Keep(ranked, map[string]Quote{"A": {}, "C": {}})
	if len(kept) != 2 || kept[0].Symbol != "A" || kept[1].Symbol != "C" {
		t.Fatalf("残った銘柄 = %+v", kept)
	}
	if kept[1].Rank != 3 || kept[1].RuleRank != 3 {
		t.Errorf("順位が振り直された: %+v", kept[1])
	}
	if kept[0].Score == nil || *kept[0].Score != score {
		t.Errorf("予測値が消えた: %+v", kept[0])
	}
}

// 規則 R（weighting = turnover）: 順位順に min(売買代金 × 比, 総額 ÷ NameDivisor) を載せ、総額を使い切ったら止める。
// 1 単元が上限に収まらない銘柄・売買代金の無い銘柄は飛ばして次点を繰り上げる。
func TestPickFromTurnoverAllocatesInRankOrder(t *testing.T) {
	row := func(rank int, symbol string, price, turnover float64) Ranked {
		return Ranked{Rank: rank, Symbol: symbol, Code: symbol + "0", Price: decimal.NewFromFloat(price),
			PrevClose: decimal.NewFromFloat(price * 1.03), Turnover: turnover}
	}
	ranked := []Ranked{
		row(1, "1001", 1000, 3e8),  // 0.2% = 60 万 → 600 株
		row(2, "1002", 2000, 1e10), // 0.2% = 2,000 万 → 上限 100 万 → 500 株
		row(3, "1003", 5000, 1e8),  // 0.2% = 20 万 < 1 単元 50 万 → 飛ばす
		row(4, "1004", 100, 0),     // 売買代金が無い → 飛ばす
	}
	for i := 5; i <= 12; i++ {
		ranked = append(ranked, row(i, "20"+string(rune('0'+i/10))+string(rune('0'+i%10)), 1000, 1e10))
	}
	opts := TurnoverOptions(PickOptions{
		N: 20, Budget: decimal.NewFromInt(350_000), Weighting: config.WeightingTurnover, Side: domain.SideBuy,
	}, config.Capital{TurnoverRatio: decimal.RequireFromString("0.002"), NameDivisor: 7})
	picks := PickFrom(ranked, opts)
	// 総額 700 万、上限 100 万: 60 万 + 100 万 × 6 + 残り 40 万 = 700 万で 8 銘柄
	total := decimal.Zero
	for _, p := range picks {
		total = total.Add(p.Amount())
		if p.Amount().GreaterThan(decimal.NewFromInt(1_000_000)) {
			t.Errorf("%s に %s 円（上限 100 万を超えた）", p.Symbol, p.Amount())
		}
	}
	if !total.Equal(decimal.NewFromInt(7_000_000)) {
		t.Errorf("総額 %s, want 7000000", total)
	}
	if len(picks) != 8 {
		t.Fatalf("選定 %d 件, want 8: %+v", len(picks), picks)
	}
	if picks[0].Symbol != "1001" || !picks[0].Quantity.Equal(decimal.NewFromInt(600)) {
		t.Errorf("1 位 %s %s 株, want 1001 600 株", picks[0].Symbol, picks[0].Quantity)
	}
	if picks[1].Symbol != "1002" || !picks[1].Quantity.Equal(decimal.NewFromInt(500)) {
		t.Errorf("2 位 %s %s 株, want 1002 500 株", picks[1].Symbol, picks[1].Quantity)
	}
	for _, p := range picks {
		if p.Symbol == "1003" || p.Symbol == "1004" {
			t.Errorf("%s は飛ばすはず", p.Symbol)
		}
	}
	if last := picks[len(picks)-1]; !last.Amount().Equal(decimal.NewFromInt(400_000)) {
		t.Errorf("最後の銘柄 %s 円, want 残りの 40 万", last.Amount())
	}
	reasons := PickReasons(ranked, opts, picks)
	if reasons["1003"] != ReasonOverBudget || reasons["1004"] != ReasonOverBudget {
		t.Errorf("理由 1003=%s 1004=%s, want over_budget", reasons["1003"], reasons["1004"])
	}
	for _, p := range picks {
		if reasons[p.Symbol] != ReasonPicked {
			t.Errorf("%s の理由 %s, want picked", p.Symbol, reasons[p.Symbol])
		}
	}
}

// 規則 R でも業種の上限と 1 銘柄の上限（max_order）は効く。
func TestPickFromTurnoverKeepsSectorCapAndMaxAmount(t *testing.T) {
	ranked := []Ranked{
		{Rank: 1, Symbol: "1001", Price: decimal.NewFromInt(1000), Turnover: 1e10, Sector: "3050"},
		{Rank: 2, Symbol: "1002", Price: decimal.NewFromInt(1000), Turnover: 1e10, Sector: "3050"},
		{Rank: 3, Symbol: "1003", Price: decimal.NewFromInt(1000), Turnover: 1e10, Sector: "3100"},
	}
	opts := TurnoverOptions(PickOptions{
		N: 20, Budget: decimal.NewFromInt(350_000), Weighting: config.WeightingTurnover, Side: domain.SideBuy,
		MaxPerSector: 1, MaxAmount: decimal.NewFromInt(800_000),
	}, config.Capital{TurnoverRatio: decimal.RequireFromString("0.002"), NameDivisor: 7})
	picks := PickFrom(ranked, opts)
	if len(picks) != 2 || picks[0].Symbol != "1001" || picks[1].Symbol != "1003" {
		t.Fatalf("選定 %+v, want 1001 と 1003（同業の 1002 は落ちる）", picks)
	}
	for _, p := range picks {
		if !p.Amount().Equal(decimal.NewFromInt(800_000)) {
			t.Errorf("%s に %s 円, want max_order の 80 万", p.Symbol, p.Amount())
		}
	}
}
