// Package portfolio は合成シグナルを「目標株数」に変換する。
//
// 重要な前提:
//
//	このシステムは現物のみを扱う。空売りはしない。したがって弱気シグナルは
//	「売り建て」ではなく「保有していれば手仕舞う」という意味になり、
//	目標株数は常に 0 以上。
//
// ヒステリシス:
//
//	entry_threshold を超えたら新規に建て、exit_threshold を下回ったら
//	手仕舞う。2 つの閾値を分けるのは、シグナルが閾値の周りで揺れるたびに
//	売買を繰り返して手数料で削られるのを防ぐため。
package portfolio

import (
	"fmt"
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

// SizingContext はサイジングに必要な材料。
type SizingContext struct {
	// Equity は総資産（現金＋評価額）。配分の基準。
	Equity decimal.Decimal
	// BuyingPower は買付余力。これを超える目標は作らない。
	BuyingPower decimal.Decimal
	// Prices は銘柄 → 直近終値。
	Prices map[string]decimal.Decimal
	// ATR は銘柄 → ATR。atr_risk 方式で損切り幅の見積りに使う。
	ATR       map[string]decimal.Decimal
	LotSizes  map[string]decimal.Decimal
	Positions map[string]domain.Position
	// DefaultLotSize は例外の無い銘柄の売買単位。東証は 100 株。
	DefaultLotSize decimal.Decimal
}

func (c SizingContext) LotSize(symbol string) decimal.Decimal {
	if lot, ok := c.LotSizes[symbol]; ok && lot.GreaterThan(decimal.Zero) {
		return lot
	}
	if c.DefaultLotSize.GreaterThan(decimal.Zero) {
		return c.DefaultLotSize
	}
	return marketrules.DefaultLotSize
}

func (c SizingContext) HeldQuantity(symbol string) decimal.Decimal {
	if pos, ok := c.Positions[symbol]; ok {
		return pos.Quantity
	}
	return decimal.Zero
}

func (c SizingContext) price(symbol string) (decimal.Decimal, bool) {
	price, ok := c.Prices[symbol]
	if !ok || price.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, false
	}
	return price, true
}

// Sizer は合成シグナルから目標建玉を決める。
type Sizer struct {
	cfg wbjpcfg.SizingConfig
}

// NewSizer は設定からサイジング方式を組み立てる。未知の方式は起動時に弾く。
func NewSizer(cfg wbjpcfg.SizingConfig) (*Sizer, error) {
	switch cfg.Method {
	case "equal_weight", "fixed_notional", "atr_risk":
	case "":
		cfg.Method = "atr_risk"
	default:
		return nil, fmt.Errorf("未知のサイジング方式 %q。利用可能: atr_risk / equal_weight / fixed_notional", cfg.Method)
	}
	if cfg.MaxPositions <= 0 {
		cfg.MaxPositions = 5
	}
	return &Sizer{cfg: cfg}, nil
}

func (s *Sizer) Method() string { return s.cfg.Method }

// Size は目標建玉を決める。
//
// 保有中だがシグナルが届かなかった銘柄も、手仕舞い判断のために必ず
// 結果に含める（そうしないと「消えた建玉」が放置される）。
func (s *Sizer) Size(
	signals map[string]domain.CombinedSignal,
	ctx SizingContext,
	entryThreshold, exitThreshold float64,
) []domain.TargetPosition {
	candidates := s.selectCandidates(signals, ctx, entryThreshold)

	symbols := make([]string, 0, len(signals)+len(ctx.Positions))
	seen := make(map[string]struct{}, len(signals)+len(ctx.Positions))
	for sym := range signals {
		seen[sym] = struct{}{}
	}
	for sym := range ctx.Positions {
		seen[sym] = struct{}{}
	}
	for sym := range seen {
		symbols = append(symbols, sym)
	}
	sort.Strings(symbols)

	var targets []domain.TargetPosition
	for _, sym := range symbols {
		signal, hasSignal := signals[sym]
		held := ctx.HeldQuantity(sym)
		direction := 0.0
		if hasSignal {
			direction = signal.Direction
		}

		// 弱気・意見なしなら手仕舞う（空売りはできない）
		if direction < exitThreshold {
			if held.GreaterThan(decimal.Zero) {
				targets = append(targets, domain.TargetPosition{
					Symbol:   sym,
					Quantity: decimal.Zero,
					Reason:   exitReason(hasSignal, direction, exitThreshold),
				})
			}
			continue
		}

		// 閾値の間は現状維持（往復売買を避けるヒステリシス）
		if direction < entryThreshold {
			if held.GreaterThan(decimal.Zero) {
				targets = append(targets, domain.TargetPosition{
					Symbol:   sym,
					Quantity: held,
					Reason:   fmt.Sprintf("保有継続 (強さ %.2f)", direction),
				})
			}
			continue
		}

		if _, ok := candidates[sym]; !ok && held.IsZero() {
			continue // 保有上限（sizing.max_positions）に達しており、新規には建てない
		}

		// 保有中は株数を計算し直さない。ATR や資産の変化で目標株数が日々
		// ずれ、その差分が「意図しない部分売買」として板に出る。建玉の
		// 増減はストップ管理（利確・手仕舞い）だけが決める。
		if held.GreaterThan(decimal.Zero) {
			targets = append(targets, domain.TargetPosition{
				Symbol:   sym,
				Quantity: held,
				Reason:   fmt.Sprintf("保有継続 (強さ %.2f)", direction),
			})
			continue
		}

		quantity := s.quantity(sym, ctx)
		rounded, err := marketrules.RoundToLot(quantity, ctx.LotSize(sym))
		if err != nil || rounded.LessThanOrEqual(decimal.Zero) {
			continue // 単元株に満たないため見送り
		}

		targets = append(targets, domain.TargetPosition{
			Symbol:   sym,
			Quantity: rounded,
			Reason:   fmt.Sprintf("%s (強さ %.2f)", s.cfg.Method, direction),
		})
	}

	return targets
}

// selectCandidates は保有上限の枠内に収まる新規候補を選ぶ。
//
// シグナルの強い順に採る。既に保有している銘柄は枠を消費する。これが
// 無いと閾値を超えた銘柄すべてを建ててしまい、max_positions が
// 「等分割の分母」でしかなくなる。
func (s *Sizer) selectCandidates(
	signals map[string]domain.CombinedSignal,
	ctx SizingContext,
	entryThreshold float64,
) map[string]struct{} {
	selected := make(map[string]struct{})
	for sym, pos := range ctx.Positions {
		if pos.Quantity.GreaterThan(decimal.Zero) {
			selected[sym] = struct{}{}
		}
	}
	available := s.cfg.MaxPositions - len(selected)
	if available <= 0 {
		return selected
	}

	fresh := make([]string, 0, len(signals))
	for sym, sig := range signals {
		if _, heldAlready := selected[sym]; heldAlready {
			continue
		}
		if sig.Direction >= entryThreshold {
			fresh = append(fresh, sym)
		}
	}
	// 強い順。同点は銘柄コード順にして、日によって採用が入れ替わらないようにする。
	sort.Slice(fresh, func(i, j int) bool {
		di, dj := signals[fresh[i]].Direction, signals[fresh[j]].Direction
		if di != dj {
			return di > dj
		}
		return fresh[i] < fresh[j]
	})
	if len(fresh) > available {
		fresh = fresh[:available]
	}
	for _, sym := range fresh {
		selected[sym] = struct{}{}
	}
	return selected
}

// quantity は 1 銘柄あたりの目標株数（単元株に丸める前）。
func (s *Sizer) quantity(symbol string, ctx SizingContext) decimal.Decimal {
	price, ok := ctx.price(symbol)
	if !ok {
		return decimal.Zero
	}
	maxPositions := decimal.NewFromInt(int64(s.cfg.MaxPositions))

	switch s.cfg.Method {
	case "fixed_notional":
		return s.cfg.FixedNotional.Div(price)

	case "equal_weight":
		budget := ctx.Equity.Div(maxPositions)
		// 評価額ベースの予算は含み益があると現金を超える。最後の 1 枠が
		// 「買付余力不足」で毎回弾かれ、資金が遊んだまま止まるので、
		// 手数料の余裕を残して買付余力に収める。
		capped := ctx.BuyingPower.Mul(decimal.RequireFromString("0.99"))
		if capped.LessThan(budget) {
			budget = capped
		}
		if budget.LessThanOrEqual(decimal.Zero) {
			return decimal.Zero
		}
		return budget.Div(price)

	default: // atr_risk
		atr, hasATR := ctx.ATR[symbol]
		if !hasATR || atr.LessThanOrEqual(decimal.Zero) {
			// ATR が取れない銘柄は損切り幅を決められない。事故を避けて見送る。
			return decimal.Zero
		}
		stopDistance := atr.Mul(s.cfg.ATRStopMultiple)
		if stopDistance.LessThanOrEqual(decimal.Zero) {
			return decimal.Zero
		}
		quantity := ctx.Equity.Mul(s.cfg.RiskPerTrade).Div(stopDistance)
		// 1銘柄が資産を食い尽くさないよう、金額でも頭を押さえる
		maxByCash := ctx.Equity.Div(maxPositions).Div(price)
		if maxByCash.LessThan(quantity) {
			return maxByCash
		}
		return quantity
	}
}

func exitReason(hasSignal bool, direction float64, exitThreshold float64) string {
	if !hasSignal {
		return "シグナルが消滅したため手仕舞い"
	}
	return fmt.Sprintf("強さ %.2f が手仕舞い閾値 %.2f を下回った", direction, exitThreshold)
}

// SizePosition は 1 銘柄ぶんの目標株数を計算する。
//
// Deprecated: 保有銘柄数の上限（sizing.max_positions）・手仕舞い閾値
// （strategies.exit_threshold）・保有中の再サイジング抑制は、
// ポートフォリオ全体を見ないと判断できない。新しい呼び出しは
// Sizer.Size を使うこと。
func SizePosition(
	sig domain.CombinedSignal,
	equity decimal.Decimal,
	price decimal.Decimal,
	atr decimal.Decimal,
	lotSize decimal.Decimal,
	cfg wbjpcfg.SizingConfig,
) (domain.TargetPosition, error) {
	if sig.Direction <= 0 {
		return domain.TargetPosition{Symbol: sig.Symbol, Quantity: decimal.Zero, Reason: "シグナルなしまたは売り"}, nil
	}

	var targetQty decimal.Decimal
	var reason string

	switch cfg.Method {
	case "fixed_notional":
		rawQty := cfg.FixedNotional.Div(price).Floor()
		targetQty, _ = marketrules.RoundToLot(rawQty, lotSize)
		reason = fmt.Sprintf("fixed_notional (%s円)", cfg.FixedNotional)

	case "equal_weight":
		maxPos := decimal.NewFromInt(int64(cfg.MaxPositions))
		alloc := equity.Div(maxPos)
		rawQty := alloc.Div(price).Floor()
		targetQty, _ = marketrules.RoundToLot(rawQty, lotSize)
		reason = fmt.Sprintf("equal_weight (%d銘柄等分, 割当 %s円)", cfg.MaxPositions, alloc.Round(0))

	case "atr_risk":
		fallthrough
	default:
		// 許容損失 = equity * risk_per_trade
		allowedLoss := equity.Mul(cfg.RiskPerTrade)
		// 1株あたり損切り幅 = atr * atr_stop_multiple
		stopWidth := atr.Mul(cfg.ATRStopMultiple)
		if stopWidth.LessThanOrEqual(decimal.Zero) {
			stopWidth = price.Mul(decimal.RequireFromString("0.05")) // フォールバック 5%
		}
		rawQty := allowedLoss.Div(stopWidth).Floor()
		targetQty, _ = marketrules.RoundToLot(rawQty, lotSize)
		reason = fmt.Sprintf("atr_risk (許容損失 %s円 / 損切り幅 %s円)", allowedLoss.Round(0), stopWidth.Round(0))
	}

	return domain.TargetPosition{
		Symbol:   sig.Symbol,
		Quantity: targetQty,
		Reason:   reason,
	}, nil
}
