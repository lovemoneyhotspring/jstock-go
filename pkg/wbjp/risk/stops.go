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
// ストップ価格をローカルに持ち、足が更新されるたびに評価して決済注文を組み立てる。
// 立花証券 e支店 API は逆指値を受け付ける（domain.OrderRequest.WithStop /
// broker.StopCorrector）が、wbjp はまだブローカーに置いていない。置く場合は
// 期日が最長 10 営業日なので、run のたびに「今のストップ価格で逆指値が立っているか」を
// 突き合わせて置き直す形になる。
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

// RetainHeld は保有していない銘柄のストップを外し、外した銘柄を昇順で返す。
//
// 手仕舞った銘柄のストップを残すと、次に同じ銘柄を建てたとき古いストップ
// （古い建値・古い作成日・古い最高値）を引き継ぎ、R の計算が狂い、建てた直後に
// 時間切れで手仕舞う。建玉が確かに分かっているとき（照会に成功したとき）だけ呼ぶ。
func (sb *StopBook) RetainHeld(positions map[string]domain.Position) []string {
	var removed []string
	for sym := range sb.stops {
		if pos, ok := positions[sym]; ok && pos.Quantity.GreaterThan(decimal.Zero) {
			continue
		}
		removed = append(removed, sym)
	}
	sort.Strings(removed)
	for _, sym := range removed {
		delete(sb.stops, sym)
	}
	return removed
}

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

// Ensure は建玉に対してストップを用意する（手仕舞った銘柄のストップは RetainHeld で外す）。
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

// EnsureWithOptions は建玉に対してストップを用意する（手仕舞った銘柄のストップは RetainHeld で外す）。
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
	// 建玉が無くなった銘柄のストップは外さない。外すのは RetainHeld の 1 か所だけ
	// （建玉を確かに照会できたときに呼び出し側が決める）。
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
// まだ利確していない建玉だけが対象（ScaledOut で一度きり）。利確が済んだら
// ストップを建値へ引き上げる（下げることはしない）。残りの手仕舞いは
// RunnerTargets に委ねる。
//
// **利確が「済んだ」とみなす時点**（ScaledOut と建値への引き上げを確定する時点）
//
//	assumeFilled（backtest。出した売りは翌寄りで必ず約定する）なら、利確を決めた時点。
//	そうでなければ（本番）、保有が実際に利確後の株数（InitialQuantity 基準の残り）まで
//	減ったのを見た時点。売りの約定を待たずに確定すると、指値が届かず失効した・当日買付の柵・
//	max_orders_per_day・発注失敗で売れなかったとき、利確が二度と出ず、RunnerTargets が
//	全株を維持に固定する（2026-09-24 のレビュー）。確定するまでは毎回同じ残り株数の
//	目標を出し直す（InitialQuantity 基準なので冪等）。
//
//	本番の確定にも含み益が targetR 以上であることを求める。株数だけで決めると、地合いや
//	サイジングで減らした建玉まで「利確済み」になり、含み損のままストップが建値へ上がって
//	即座に全株を手仕舞いうる。売りが約定した後、次の回までに含み益が targetR を割ると
//	確定が遅れる（その間は利確前の扱い）。
func (sb *StopBook) TakeProfitTargets(
	closes map[string]decimal.Decimal,
	quantities map[string]decimal.Decimal,
	lotSizes map[string]decimal.Decimal,
	targetR *decimal.Decimal,
	fraction decimal.Decimal,
	defaultLotSize decimal.Decimal,
	assumeFilled bool,
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
		if !assumeFilled && !currentQty.GreaterThan(remaining) {
			// 保有が利確後の株数まで減っている（売りが約定した）→ 利確済みとして確定する
			stop.markScaledOut()
			continue
		}
		if remaining.GreaterThan(currentQty) {
			remaining = currentQty
		}
		if assumeFilled {
			stop.markScaledOut()
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

// markScaledOut は利確を確定する。利確できた建玉は「もう負けにしない」ので、
// 建値より下のストップは建値へ引き上げる。
func (s *Stop) markScaledOut() {
	s.ScaledOut = true
	if s.StopPrice.LessThan(s.EntryPrice) {
		s.StopPrice = s.EntryPrice
	}
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

// ExitInputs は ExitPlan の材料。
type ExitInputs struct {
	// Closes は判断に使う終値（足が古い・読めない銘柄は入れない）。
	Closes map[string]decimal.Decimal
	// Quantities は現在の保有株数。
	Quantities map[string]decimal.Decimal
	LotSizes   map[string]decimal.Decimal
	// AsOf は判断日（YYYY-MM-DD）。時間切れの営業日数を数える。
	AsOf string
	// Bars は銘柄の判断日までの足。残り玉の手仕舞い線（trend_exit_sma）に使う。
	// nil なら線は無し（移動平均割れでは手仕舞わない）。
	Bars func(symbol string) []domain.Bar
	// AssumeFilled は出した売りが必ず約定する前提（backtest。翌寄りで約定させる）。
	// true なら利確を決めた時点で ScaledOut と建値への引き上げを確定する。本番は false
	// （保有が減ったのを見てから確定する。TakeProfitTargets）。
	AssumeFilled bool
}

// ExitPlan はストップ由来の目標（損切り・時間切れ・利確・残り玉）をまとめて返す。
//
// 本番（cmd/wbjp/run.go）と backtest（engine.RunBacktest）は必ずここを通す。
// 並べ方がずれると検証と本番の結果が食い違う（2026-09-24 のレビュー W1:
// 本番だけが残り玉の目標を足していた）。
//
// 同じ銘柄に複数の目標が出たら、株数の少ないほう（手仕舞いの強いほう）を採る
// （MergeStopTargets）。後から足した目標が勝つ形だと、同じ回に利確が ScaledOut を
// 立てたとき、残り玉の「維持（利確前の株数）」が損切り・利確を打ち消していた。
//
// TakeProfitTargets は利確が済んだ建玉（本番は保有が減ったのを見てから、backtest は
// 決めた時点）のストップを建値へ引き上げ ScaledOut を立てる。呼び出し側は**この後で**
// ストップを保存すること（W2）。
func (sb *StopBook) ExitPlan(cfg wbjpcfg.StopsConfig, in ExitInputs) []domain.TargetPosition {
	var targets []domain.TargetPosition
	targets = append(targets, sb.ExitTargets(in.Closes)...)
	targets = append(targets, sb.TimeExitTargets(in.Closes, in.AsOf, cfg.StaleExitDays, cfg.MaxHoldDays)...)
	targets = append(targets, sb.TakeProfitTargets(in.Closes, in.Quantities, in.LotSizes,
		cfg.TakeProfitR, cfg.TakeProfitFraction, marketrules.DefaultLotSize, in.AssumeFilled)...)
	targets = append(targets, sb.RunnerTargets(in.Closes, in.Quantities, sb.trendValues(cfg, in.Bars), cfg.TrendExitAlways)...)
	return MergeStopTargets(targets)
}

// trendValues は残り玉の手仕舞い線の直近値。ストップのある銘柄だけ計算する。
func (sb *StopBook) trendValues(cfg wbjpcfg.StopsConfig, bars func(string) []domain.Bar) map[string]decimal.Decimal {
	if bars == nil || cfg.TrendExitSMA == nil || *cfg.TrendExitSMA <= 0 {
		return nil
	}
	out := make(map[string]decimal.Decimal, len(sb.stops))
	for sym := range sb.stops {
		if v, ok := TrendValue(bars(sym), cfg); ok {
			out[sym] = v
		}
	}
	return out
}

// MergeStopTargets は同じ銘柄のストップ由来の目標を 1 つにする。株数の少ないほうが勝つ
// （手仕舞い 0 株 ＞ 利確の残り ＞ 残り玉の維持）。同数なら先に並んだほう（理由を残す）。
func MergeStopTargets(targets []domain.TargetPosition) []domain.TargetPosition {
	merged := make(map[string]domain.TargetPosition, len(targets))
	for _, t := range targets {
		if prev, ok := merged[t.Symbol]; ok && !t.Quantity.LessThan(prev.Quantity) {
			continue
		}
		merged[t.Symbol] = t
	}
	out := make([]domain.TargetPosition, 0, len(merged))
	for _, t := range merged {
		out = append(out, t)
	}
	return sortTargets(out)
}

// ApplyStopPriority はストップによる手仕舞いを戦略の目標より優先して重ねる。
//
// 同じ銘柄について戦略が「買い増し」と言っていても、ストップに抵触して
// いれば手仕舞いが勝つ。ストップ由来の目標どうしは MergeStopTargets で
// 株数の少ないほうを採る（並べた順で結果が変わらない）。
func ApplyStopPriority(strategyTargets, stopTargets []domain.TargetPosition) []domain.TargetPosition {
	merged := make(map[string]domain.TargetPosition, len(strategyTargets)+len(stopTargets))
	for _, t := range strategyTargets {
		merged[t.Symbol] = t
	}
	for _, t := range MergeStopTargets(stopTargets) {
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
