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
