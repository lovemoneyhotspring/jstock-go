// Package selection は 9:00 の判断: 気配からギャップを出し、下位 N 銘柄を予算に合わせて
// 株数にする（Python 版の daytrade.select。select は Go の予約語なので名前を変えた）。
//
// 純粋関数だけ。plan の候補（前夜）と気配（当日）を受け取り、注文の元になる Pick を返す。
// バックテストは同じ順位付けをパネルに対して行う。
package selection

import (
	"math"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/fees"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	"github.com/shopspring/decimal"
)

// VolFloor はボラが取れない・極端に小さい銘柄に使う下限（日次 2%）。重みが暴れないため。
const VolFloor = 0.02

// Quote は気配・現在値。Price は寄付値（板寄せ後）か、寄り前なら気配値。
type Quote struct {
	Symbol string
	Price  decimal.Decimal
	At     time.Time
	// Source は取得元。ログと「遅延の気配を使っていないか」の判断に使う。
	Source  string
	Delayed bool
	// PrevClose は取得元が返す当日の基準値段（前日終値）。正なら plan の前日終値より
	// 優先する——株式分割・併合の日は plan の値（アーカイブの終値）が調整前で、
	// そのままだと −50% のようなギャップに見えて候補の先頭に来る。
	PrevClose decimal.Decimal
	// Opened は**もう寄っている**（時価問合の始値が入っている）。偽なら Price は
	// 寄り前の気配値で、成行はこれから始まる板寄せで約定する。
	// signal.skip_opened の判定に使う（daytrade/quotes.DropOpened）。
	Opened bool
}

// Pick は建てる銘柄 1 つ。Side が BUY なら寄付で買う（ロング）、SELL なら売建てる（ショート）。
type Pick struct {
	Symbol    string
	Code      string
	Name      string
	PrevClose decimal.Decimal
	Price     decimal.Decimal
	Gap       decimal.Decimal
	Quantity  decimal.Decimal
	Rank      int
	Side      domain.Side
}

// Amount は約定代金の見込み。
func (p Pick) Amount() decimal.Decimal { return p.Price.Mul(p.Quantity) }

// Fee は片道の手数料（見込み。定額コースは 1 日の合計で決まるので、この注文だけの日として）。
func (p Pick) Fee() decimal.Decimal { return fees.OrderFeeEstimate(p.Amount()) }

// Ranked はギャップ順に並べた候補 1 つ（数量はまだ決めていない）。
type Ranked struct {
	Rank      int
	Symbol    string
	Code      string
	Name      string
	PrevClose decimal.Decimal
	Price     decimal.Decimal
	Gap       decimal.Decimal
	// Vol は 20 日の日次ボラ（無ければ nil）。
	Vol *float64
}

// LimitDownPrice は前日終値を基準値段とするストップ安の値段。
func LimitDownPrice(prevClose decimal.Decimal) decimal.Decimal {
	low, _, err := marketrules.PriceLimitRange(prevClose)
	if err != nil {
		return decimal.Zero
	}
	return low
}

// LimitUpPrice は前日終値を基準値段とするストップ高の値段。
func LimitUpPrice(prevClose decimal.Decimal) decimal.Decimal {
	_, high, err := marketrules.PriceLimitRange(prevClose)
	if err != nil {
		return decimal.Zero
	}
	return high
}

// SharesFor は予算内で買える株数（単元に切り捨て）。1 単元に届かなければ 0。
func SharesFor(budget, price, lot decimal.Decimal) decimal.Decimal {
	if lot.LessThanOrEqual(decimal.Zero) {
		lot = marketrules.DefaultLotSize
	}
	if price.LessThanOrEqual(decimal.Zero) || budget.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	return budget.Div(price.Mul(lot)).Floor().Mul(lot)
}

// Rank は候補（Eligible が真）に気配を当て、ギャップの小さい順に並べる。
//
// 気配が無い・ギャップが条件外・ストップ安の銘柄は入らない。
func Rank(candidates []universe.Candidate, quotes map[string]Quote, cfg config.Signal) []Ranked {
	return rankBy(candidates, quotes, gapFilter{
		min: cfg.MinGap, max: cfg.MaxGap,
		skipLimit: cfg.SkipLimitDown, limitDown: true,
		rankBy: cfg.RankBy,
	})
}

// RankShort は信用売りの候補を、ギャップの**大きい**順に並べる（Rank の鏡像）。
//
// candidates は前夜の plan がショート専用の母集団で判定した行だけを渡すこと。
func RankShort(candidates []universe.Candidate, quotes map[string]Quote, m config.Margin) []Ranked {
	return rankBy(candidates, quotes, gapFilter{
		min: m.MinGap, max: m.MaxGap,
		skipLimit: m.SkipLimitUp, limitDown: false,
		descending: true,
	})
}

type gapFilter struct {
	min, max decimal.Decimal
	// skipLimit が真なら制限値幅に触れている銘柄を落とす。
	// limitDown が真ならストップ安（買わない）、偽ならストップ高（売らない）。
	skipLimit  bool
	limitDown  bool
	descending bool
	// rankBy は並べる鍵（config.RankByGap / RankByGapVol。空は gap）。
	rankBy string
}

// RankKey は並べ替えの鍵。gap_vol はギャップ ÷ max(20 日ボラ, VolFloor)。
// ボラが無い銘柄は ok=false（並びの末尾）。バックテスト（backtest.pickDay）も同じ鍵を使う。
func RankKey(rankBy string, gap float64, vol *float64) (float64, bool) {
	if rankBy != config.RankByGapVol {
		return gap, true
	}
	if vol == nil {
		return 0, false
	}
	return gap / math.Max(*vol, VolFloor), true
}

func rankBy(candidates []universe.Candidate, quotes map[string]Quote, f gapFilter) []Ranked {
	var scored []Ranked
	for _, c := range candidates {
		quote, ok := quotes[c.Symbol]
		if !ok || quote.Price.LessThanOrEqual(decimal.Zero) || c.PrevClose <= 0 {
			continue
		}
		prev := decimal.NewFromFloat(c.PrevClose)
		if quote.PrevClose.GreaterThan(decimal.Zero) {
			prev = quote.PrevClose
		}
		gap := quote.Price.Div(prev).Sub(decimal.NewFromInt(1))
		// 帯は常に [min, max)。ロングは max = 0（ギャップダウン）、ショートは min = 0.05。
		if gap.LessThan(f.min) || gap.GreaterThanOrEqual(f.max) {
			continue
		}
		if f.skipLimit {
			if f.limitDown && quote.Price.LessThanOrEqual(LimitDownPrice(prev)) {
				continue
			}
			if !f.limitDown && quote.Price.GreaterThanOrEqual(LimitUpPrice(prev)) {
				continue
			}
		}
		scored = append(scored, Ranked{
			Symbol:    c.Symbol,
			Code:      c.Code,
			Name:      c.Name,
			PrevClose: prev,
			Price:     quote.Price,
			Gap:       gap.Round(4),
			Vol:       c.Vol20,
		})
	}
	// 同じ鍵なら銘柄コード順（順位を実行ごとに揺らさない）。鍵の無い銘柄は末尾。
	sort.SliceStable(scored, func(i, j int) bool {
		a, b := scored[i], scored[j]
		ka, oka := RankKey(f.rankBy, a.Gap.InexactFloat64(), a.Vol)
		kb, okb := RankKey(f.rankBy, b.Gap.InexactFloat64(), b.Vol)
		if oka != okb {
			return oka
		}
		if ka != kb {
			if f.descending {
				return ka > kb
			}
			return ka < kb
		}
		return a.Symbol < b.Symbol
	})
	for i := range scored {
		scored[i].Rank = i + 1
	}
	return scored
}

// Weights は N 銘柄への配分比（合計 1）。inverse_vol は 20 日ボラの逆数、equal は等分。
func Weights(rows []Ranked, weighting string) []float64 {
	if len(rows) == 0 {
		return nil
	}
	raw := make([]float64, len(rows))
	total := 0.0
	for i, r := range rows {
		if weighting == "inverse_vol" {
			vol := VolFloor
			if r.Vol != nil && *r.Vol > VolFloor {
				vol = *r.Vol
			}
			raw[i] = 1.0 / vol
		} else {
			raw[i] = 1.0
		}
		total += raw[i]
	}
	out := make([]float64, len(rows))
	for i := range raw {
		out[i] = raw[i] / total
	}
	return out
}

// PickOptions は Pick の指定。
type PickOptions struct {
	// N は選ぶ銘柄数。
	N int
	// Budget は 1 注文の予算。総予算は Budget × N。
	Budget decimal.Decimal
	// Weighting は equal / inverse_vol。
	Weighting string
	// LotSizes は銘柄ごとの単元株数（無ければ 100）。
	LotSizes map[string]decimal.Decimal
	// Side は建てる向き。
	Side domain.Side
	// MaxAmount は 1 銘柄の金額の上限（候補が N に満たない日に総予算を 1 銘柄に寄せない）。
	// ゼロなら上限なし＝総予算 Budget × N を按分する（既定）。
	MaxAmount decimal.Decimal
}

// Pick は順位表の上位 N 銘柄を選び、株数を決める。
//
// まず「1 単元が Budget に収まる」銘柄を順位順に N 個取る（届かない銘柄は次点を繰り上げ）。
// 次に Weighting で総予算 Budget × N を按分し、単元に切り捨てる。inverse_vol で按分が
// 小さすぎて 1 単元に届かない銘柄は落ちる（N が減る）。
//
// 売建（Side が SELL）は成行で出せる上限（50 単元。空売り価格規制の適用除外の範囲）で
// 頭打ちにする——超える数量はブローカーが拒否し、その日はその銘柄を建てられずに終わるため。
// 低位株では予算を使い切れないが、建てないよりよい。
func PickFrom(ranked []Ranked, opts PickOptions) []Pick {
	if opts.N < 1 {
		return nil
	}
	side := opts.Side
	if side == "" {
		side = domain.SideBuy
	}
	var chosen []Ranked
	for _, r := range ranked {
		if len(chosen) >= opts.N {
			break
		}
		if SharesFor(opts.Budget, r.Price, lotOf(opts.LotSizes, r.Symbol)).GreaterThan(decimal.Zero) {
			chosen = append(chosen, r)
		}
	}
	total := opts.Budget.Mul(decimal.NewFromInt(int64(opts.N)))
	weights := Weights(chosen, opts.Weighting)
	var picks []Pick
	for i, r := range chosen {
		lot := lotOf(opts.LotSizes, r.Symbol)
		amount := total.Mul(decimal.NewFromFloat(weights[i]))
		if opts.MaxAmount.GreaterThan(decimal.Zero) && amount.GreaterThan(opts.MaxAmount) {
			amount = opts.MaxAmount
		}
		quantity := SharesFor(amount, r.Price, lot)
		if quantity.LessThanOrEqual(decimal.Zero) {
			continue
		}
		if limit := decimal.NewFromInt(int64(broker.ShortSaleMarketLimit)); side == domain.SideSell && quantity.GreaterThan(limit) {
			quantity = limit.Div(lot).Floor().Mul(lot)
		}
		picks = append(picks, Pick{
			Symbol:    r.Symbol,
			Code:      r.Code,
			Name:      r.Name,
			PrevClose: r.PrevClose,
			Price:     r.Price,
			Gap:       r.Gap,
			Quantity:  quantity,
			Rank:      r.Rank,
			Side:      side,
		})
	}
	return picks
}

func lotOf(lots map[string]decimal.Decimal, symbol string) decimal.Decimal {
	if lot, ok := lots[symbol]; ok && lot.GreaterThan(decimal.Zero) {
		return lot
	}
	return marketrules.DefaultLotSize
}

// SpillInto はショートの余り spill をロングに回したときの銘柄数と 1 注文の予算。
//
// n / budget はその日のロングの銘柄数と 1 注文の予算（縮小やショックの倍率を掛けた後）、
// baseBudget は倍率を掛ける前の 1 注文の予算（capital.order_budget から決まる値）。
// 銘柄数は (baseBudget × n + spill) ÷ baseBudget を切り捨てて、元の n 以上・maxN 以下に
// 収める（検証の simulateMarginSpill と同じ式）。総予算 budget × n + spill をその銘柄数で割った
// ものが新しい 1 注文の予算。
func SpillInto(n int, budget, baseBudget, spill decimal.Decimal, maxN int) (int, decimal.Decimal) {
	if spill.LessThanOrEqual(decimal.Zero) || n < 1 || baseBudget.LessThanOrEqual(decimal.Zero) {
		return n, budget
	}
	total := budget.Mul(decimal.NewFromInt(int64(n))).Add(spill)
	nDay := int(baseBudget.Mul(decimal.NewFromInt(int64(n))).Add(spill).Div(baseBudget).Floor().IntPart())
	if nDay < n {
		nDay = n
	}
	if maxN > 0 && nDay > maxN {
		nDay = maxN
	}
	return nDay, total.Div(decimal.NewFromInt(int64(nDay))).Floor()
}

// ThresholdPrice は「寄付がこの値未満なら候補」の閾値（前日終値 × (1 + maxGap)）。
func ThresholdPrice(prevClose, maxGap decimal.Decimal) decimal.Decimal {
	return prevClose.Mul(decimal.NewFromInt(1).Add(maxGap)).Round(1)
}
