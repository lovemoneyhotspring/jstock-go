package risk

import (
	"fmt"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

// Stop は 1 建玉ぶんのストップ。
//
// 逆指値はブローカーに置かない（日本株 API が非対応）。ストップ価格を
// ローカルに持ち、足が更新されるたびに評価して決済注文を組み立てる。
type Stop struct {
	Symbol      string
	StopPrice   decimal.Decimal
	EntryPrice  decimal.Decimal
	CreatedOn   string
	Trailing    bool
	ATRMultiple decimal.Decimal
	// TrailingPct が設定されていれば ATR ではなく「最高終値 × (1 − 比率)」で追従する。
	TrailingPct  *decimal.Decimal
	HighestClose *decimal.Decimal
	// InitialStopPrice は設定時の初期ストップ。1R（初期リスク）の基準として、
	// ストップを動かした後も変わらない。nil は旧レコード。
	InitialStopPrice *decimal.Decimal
	// InitialQuantity は設定時の建玉数。2 段階利確で「元の何%を売ったか」の基準。
	InitialQuantity *decimal.Decimal
	// ScaledOut は 1 段目の利確（部分手仕舞い）が済んだか。
	ScaledOut bool
}

func (s *Stop) IsTriggered(price decimal.Decimal) bool {
	return price.LessThanOrEqual(s.StopPrice)
}

// RiskPerShare は現在のストップまでの距離。
func (s *Stop) RiskPerShare() decimal.Decimal {
	return s.EntryPrice.Sub(s.StopPrice)
}

// InitialRisk は 1R = 建値 − 初期ストップ。利確目標・建値移動の基準。
//
// 現在のストップを使うとトレーリングで縮んだぶん R が膨らみ、利確が
// 早まってしまう。基準は建てたときのまま固定する。
func (s *Stop) InitialRisk() decimal.Decimal {
	base := s.StopPrice
	if s.InitialStopPrice != nil {
		base = *s.InitialStopPrice
	}
	return s.EntryPrice.Sub(base)
}

// TradingDaysHeld は建ててからの営業日数。
func (s *Stop) TradingDaysHeld(asOf time.Time) int {
	created, err := time.Parse("2006-01-02", s.CreatedOn)
	if err != nil {
		return 0
	}
	return tradingDaysHeld(created, asOf)
}

type StopBook struct {
	stops map[string]*Stop
}

func NewStopBook(stops map[string]*Stop) *StopBook {
	if stops == nil {
		stops = make(map[string]*Stop)
	}
	return &StopBook{stops: stops}
}

func (sb *StopBook) All() map[string]Stop {
	res := make(map[string]Stop)
	for k, v := range sb.stops {
		res[k] = *v
	}
	return res
}

func (sb *StopBook) Get(symbol string) (Stop, bool) {
	stop, ok := sb.stops[symbol]
	if !ok {
		return Stop{}, false
	}
	return *stop, true
}

func (sb *StopBook) Set(stop Stop) {
	copied := stop
	sb.stops[stop.Symbol] = &copied
}

func (sb *StopBook) Remove(symbol string) {
	delete(sb.stops, symbol)
}

func (sb *StopBook) Len() int { return len(sb.stops) }

// EnsureOptions は Ensure の任意項目。設定（[stops]）から作る。
type EnsureOptions struct {
	// ATRMultiple は初期ストップ幅の ATR 倍率（sizing.atr_stop_multiple）。
	ATRMultiple decimal.Decimal
	Trailing    bool
	// InitialStopPct が設定されていれば ATR を使わず建値からの比率で幅を決める。
	InitialStopPct *decimal.Decimal
	// TrailingATRMultiple はトレーリング時の幅。nil なら ATRMultiple と同じ。
	// 初期は狭く・追従は広く（Chandelier Exit）としたいときに使う。
	TrailingATRMultiple *decimal.Decimal
	TrailingPct         *decimal.Decimal
}

// EnsureOptionsFrom は [stops] と sizing.atr_stop_multiple から Ensure の
// 引数を組み立てる。設定を書いたのに効かない事故を防ぐため、結線は
// ここ一箇所に集める。
func EnsureOptionsFrom(stops wbjpcfg.StopsConfig, atrStopMultiple decimal.Decimal) EnsureOptions {
	return EnsureOptions{
		ATRMultiple:         atrStopMultiple,
		Trailing:            stops.Trailing,
		InitialStopPct:      stops.InitialStopPct,
		TrailingATRMultiple: stops.TrailingATRMultiple,
		TrailingPct:         stops.TrailingPct,
	}
}

// Ensure は建玉に対してストップを用意し、消えた建玉のぶんを片付ける。
//
// 互換のため残している薄いラッパ。initial_stop_pct や trailing_pct を
// 効かせるには EnsureWithOptions を使う。
func (sb *StopBook) Ensure(
	positions map[string]domain.Position,
	atr map[string]decimal.Decimal,
	today string,
	atrMultiple decimal.Decimal,
	trailing bool,
) {
	sb.EnsureWithOptions(positions, atr, today, EnsureOptions{
		ATRMultiple: atrMultiple,
		Trailing:    trailing,
	})
}

// EnsureWithOptions は建玉に対してストップを用意し、消えた建玉のぶんを片付ける。
//
// ストップを持たない建玉があるのは危険な状態なので、取得価格から自動で設定する。
func (sb *StopBook) EnsureWithOptions(
	positions map[string]domain.Position,
	atr map[string]decimal.Decimal,
	today string,
	opts EnsureOptions,
) {
	atrMultiple := opts.ATRMultiple
	if atrMultiple.LessThanOrEqual(decimal.Zero) {
		atrMultiple = decimal.RequireFromString("2.0")
	}
	// トレーリング幅の指定が無ければ初期ストップと同じ倍率で追従する。
	trailMultiple := atrMultiple
	if opts.TrailingATRMultiple != nil && opts.TrailingATRMultiple.GreaterThan(decimal.Zero) {
		trailMultiple = *opts.TrailingATRMultiple
	}

	for sym, pos := range positions {
		if pos.Quantity.LessThanOrEqual(decimal.Zero) {
			continue
		}
		if _, exists := sb.stops[sym]; exists {
			continue
		}

		var dist decimal.Decimal
		if opts.InitialStopPct != nil {
			dist = pos.CostPrice.Mul(*opts.InitialStopPct)
		} else {
			atrVal, hasATR := atr[sym]
			if !hasATR || atrVal.LessThanOrEqual(decimal.Zero) {
				// ATR が無い銘柄にストップ無しの建玉を残すのは危険だが、
				// 適当な価格を置くほうがもっと危険。ここでは作らない。
				continue
			}
			dist = atrVal.Mul(atrMultiple)
		}

		stopPrice := pos.CostPrice.Sub(dist)
		if stopPrice.LessThanOrEqual(decimal.Zero) {
			continue
		}

		lastP := pos.LastPrice
		initQty := pos.Quantity
		initStop := stopPrice

		sb.stops[sym] = &Stop{
			Symbol:           sym,
			StopPrice:        stopPrice,
			EntryPrice:       pos.CostPrice,
			CreatedOn:        today,
			Trailing:         opts.Trailing,
			ATRMultiple:      trailMultiple,
			TrailingPct:      opts.TrailingPct,
			HighestClose:     &lastP,
			InitialStopPrice: &initStop,
			InitialQuantity:  &initQty,
			ScaledOut:        false,
		}
	}

	// 建玉がなくなった銘柄のストップは削除
	for sym := range sb.stops {
		pos, exists := positions[sym]
		if !exists || pos.Quantity.LessThanOrEqual(decimal.Zero) {
			delete(sb.stops, sym)
		}
	}
}

// UpdateTrailing はトレーリングストップを引き上げる。
//
// **下げることは絶対にしない。** 下げると損失が青天井になる。
func (sb *StopBook) UpdateTrailing(closes, atr map[string]decimal.Decimal) {
	for sym, stop := range sb.stops {
		if !stop.Trailing {
			continue
		}

		closePrice, ok := closes[sym]
		if !ok {
			continue
		}

		highest := closePrice
		if stop.HighestClose != nil && stop.HighestClose.GreaterThan(closePrice) {
			highest = *stop.HighestClose
		}
		stop.HighestClose = &highest

		var candidate decimal.Decimal
		if stop.TrailingPct != nil {
			candidate = highest.Mul(decimal.NewFromInt(1).Sub(*stop.TrailingPct))
		} else {
			atrVal, hasATR := atr[sym]
			if !hasATR || atrVal.LessThanOrEqual(decimal.Zero) {
				continue
			}
			candidate = highest.Sub(atrVal.Mul(stop.ATRMultiple))
		}

		if candidate.GreaterThan(stop.StopPrice) {
			stop.StopPrice = candidate
		}
	}
}

// UpdateBreakeven は含み益が afterR に達した建玉のストップを建値へ引き上げる。
//
// 「勝ちトレードを負けに変えない」ための一手。建値より上には動かさない
// （それはトレーリングの仕事）。afterR が nil なら何もしない。
func (sb *StopBook) UpdateBreakeven(closes map[string]decimal.Decimal, afterR *decimal.Decimal) {
	if afterR == nil {
		return
	}
	for sym, stop := range sb.stops {
		closePrice, ok := closes[sym]
		if !ok || stop.StopPrice.GreaterThanOrEqual(stop.EntryPrice) {
			continue
		}
		risk := stop.InitialRisk()
		if risk.LessThanOrEqual(decimal.Zero) {
			continue
		}
		highest := closePrice
		if stop.HighestClose != nil && stop.HighestClose.GreaterThan(closePrice) {
			highest = *stop.HighestClose
		}
		stop.HighestClose = &highest
		if highest.Sub(stop.EntryPrice).Div(risk).GreaterThanOrEqual(*afterR) {
			stop.StopPrice = stop.EntryPrice
		}
	}
}

// Triggered は抵触したストップを返す。
func (sb *StopBook) Triggered(closes map[string]decimal.Decimal) map[string]Stop {
	res := make(map[string]Stop)
	for sym, stop := range sb.stops {
		if closePrice, ok := closes[sym]; ok && stop.IsTriggered(closePrice) {
			res[sym] = *stop
		}
	}
	return res
}

// ExitTargets は抵触した建玉の手仕舞い目標（0株）を作る。
//
// この目標は戦略のシグナルより優先される。損切りは戦略の意見に
// 関係なく実行しなければ意味がない。
func (sb *StopBook) ExitTargets(closes map[string]decimal.Decimal) []domain.TargetPosition {
	var targets []domain.TargetPosition

	for sym, stop := range sb.Triggered(closes) {
		targets = append(targets, domain.TargetPosition{
			Symbol:   sym,
			Quantity: decimal.Zero,
			Reason:   fmt.Sprintf("ストップ抵触: 終値 %s <= ストップ %s（日足のため翌営業日の寄付で決済）", closes[sym], stop.StopPrice),
		})
	}

	return sortTargets(targets)
}

// TimeExitTargets は時間切れの手仕舞い目標。
//
//   - staleDays 営業日たっても含み益ゼロ以下 → 前提が崩れている
//   - maxDays 営業日 → 資金効率のため強制決済
//
// どちらも nil で無効。
func (sb *StopBook) TimeExitTargets(
	closes map[string]decimal.Decimal,
	asOf string,
	staleDays *int,
	maxDays *int,
) []domain.TargetPosition {
	if staleDays == nil && maxDays == nil {
		return nil
	}

	asOfTime, err := time.Parse("2006-01-02", asOf)
	if err != nil {
		return nil
	}
	var targets []domain.TargetPosition

	for sym, stop := range sb.stops {
		closePrice, ok := closes[sym]
		if !ok {
			continue
		}
		held := stop.TradingDaysHeld(asOfTime)

		var reason string
		switch {
		case maxDays != nil && held >= *maxDays:
			reason = fmt.Sprintf("最大保有期間 %d 営業日に到達（全株決済）", *maxDays)
		case staleDays != nil && held >= *staleDays && closePrice.LessThanOrEqual(stop.EntryPrice):
			reason = fmt.Sprintf("%d 営業日たっても含み益なし（終値 %s <= 建値 %s）", held, closePrice, stop.EntryPrice)
		}
		if reason != "" {
			targets = append(targets, domain.TargetPosition{
				Symbol:   sym,
				Quantity: decimal.Zero,
				Reason:   reason,
			})
		}
	}

	return sortTargets(targets)
}

// TakeProfitTargets は 2 段階利確の 1 段目。含み益が targetR に達した
// 建玉の一部を利確する。
//
// まだ利確していない建玉だけが対象（ScaledOut で一度きり）。利確と同時に
// ストップを建値へ引き上げる（下げることはしない）。残りの手仕舞いは
// RunnerTargets に委ねる。
func (sb *StopBook) TakeProfitTargets(
	closes map[string]decimal.Decimal,
	quantities map[string]decimal.Decimal,
	lotSizes map[string]decimal.Decimal,
	targetR *decimal.Decimal,
	fraction decimal.Decimal,
	defaultLotSize decimal.Decimal,
) []domain.TargetPosition {
	if targetR == nil {
		return nil
	}
	if defaultLotSize.LessThanOrEqual(decimal.Zero) {
		defaultLotSize = decimal.NewFromInt(1)
	}

	var targets []domain.TargetPosition
	for sym, stop := range sb.stops {
		if stop.ScaledOut || stop.InitialQuantity == nil {
			continue
		}
		closePrice, hasClose := closes[sym]
		currentQty, hasQty := quantities[sym]
		if !hasClose || !hasQty || currentQty.LessThanOrEqual(decimal.Zero) {
			continue
		}
		risk := stop.InitialRisk()
		if risk.LessThanOrEqual(decimal.Zero) {
			continue
		}
		gainR := closePrice.Sub(stop.EntryPrice).Div(risk)
		if gainR.LessThan(*targetR) {
			continue
		}

		lot, ok := lotSizes[sym]
		if !ok || lot.LessThanOrEqual(decimal.Zero) {
			lot = defaultLotSize
		}
		remaining, err := marketrules.RoundToLot(stop.InitialQuantity.Mul(decimal.NewFromInt(1).Sub(fraction)), lot)
		if err != nil {
			continue
		}
		if remaining.GreaterThan(currentQty) {
			remaining = currentQty
		}

		// 利確できた建玉は「もう負けにしない」。建値より下のストップは引き上げる。
		stop.ScaledOut = true
		if stop.StopPrice.LessThan(stop.EntryPrice) {
			stop.StopPrice = stop.EntryPrice
		}

		if remaining.LessThan(currentQty) {
			targets = append(targets, domain.TargetPosition{
				Symbol:   sym,
				Quantity: remaining,
				Reason: fmt.Sprintf("利確 +%sR 到達、%s%% を手仕舞い（残りはトレンド追従）",
					gainR.Round(1), fraction.Mul(decimal.NewFromInt(100)).Round(0)),
			})
		}
	}

	return sortTargets(targets)
}

// RunnerTargets は 2 段階利確の 2 段目。利確済みの建玉をトレンド追従で
// 保有し続け、移動平均を割ったら残りを手仕舞う。
//
// always なら利確前の建玉にも適用する（移動平均割れそのものを合図にする）。
//
// **なぜ「保有継続」も明示するのか**
//
//	サイジングは毎日 目標株数を計算し直す。何も言わないと、利確で減らした
//	株数が翌日には満額へ買い戻されてしまう。残数をストップ優先の目標として
//	固定し、移動平均を割ったときだけ手仕舞いに切り替える。
func (sb *StopBook) RunnerTargets(
	closes map[string]decimal.Decimal,
	quantities map[string]decimal.Decimal,
	trendValues map[string]decimal.Decimal,
	always bool,
) []domain.TargetPosition {
	var targets []domain.TargetPosition
	for sym, stop := range sb.stops {
		if !stop.ScaledOut && !always {
			continue
		}
		closePrice, hasClose := closes[sym]
		currentQty, hasQty := quantities[sym]
		if !hasClose || !hasQty || currentQty.LessThanOrEqual(decimal.Zero) {
			continue
		}

		trend, hasTrend := trendValues[sym]
		if hasTrend && closePrice.LessThan(trend) {
			targets = append(targets, domain.TargetPosition{
				Symbol:   sym,
				Quantity: decimal.Zero,
				Reason:   fmt.Sprintf("終値 %s が移動平均 %s を割り込み、残りを手仕舞い", closePrice, trend),
			})
			continue
		}
		targets = append(targets, domain.TargetPosition{
			Symbol:   sym,
			Quantity: currentQty,
			Reason:   "利確後の残り玉を維持（トレンド追従中）",
		})
	}

	return sortTargets(targets)
}

// ApplyStopPriority はストップによる手仕舞いを戦略の目標より優先して重ねる。
//
// 同じ銘柄について戦略が「買い増し」と言っていても、ストップに抵触して
// いれば手仕舞いが勝つ。
func ApplyStopPriority(strategyTargets, stopTargets []domain.TargetPosition) []domain.TargetPosition {
	merged := make(map[string]domain.TargetPosition, len(strategyTargets)+len(stopTargets))
	for _, t := range strategyTargets {
		merged[t.Symbol] = t
	}
	for _, t := range stopTargets {
		merged[t.Symbol] = t
	}
	targets := make([]domain.TargetPosition, 0, len(merged))
	for _, t := range merged {
		targets = append(targets, t)
	}
	return sortTargets(targets)
}

// sortTargets は結果を銘柄順に並べる。map の反復順は不定なので、
// 並べないと同じ入力でも発注順やログが日によって変わる。
func sortTargets(targets []domain.TargetPosition) []domain.TargetPosition {
	sort.Slice(targets, func(i, j int) bool { return targets[i].Symbol < targets[j].Symbol })
	return targets
}

// tradingDaysHeld は建ててからの営業日数（土日を除く。祝日は数える）。
//
// 暦日で数えると連休を挟んだときに時間切れが早まる。「何日持ったか」は
// 市場が開いた日数で数えるのが判断の意図に合う。
func tradingDaysHeld(createdOn, asOf time.Time) int {
	if !asOf.After(createdOn) {
		return 0
	}
	days := 0
	for current := createdOn; current.Before(asOf); {
		current = current.AddDate(0, 0, 1)
		if wd := current.Weekday(); wd != time.Saturday && wd != time.Sunday {
			days++
		}
	}
	return days
}
