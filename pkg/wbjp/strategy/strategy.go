// Package strategy は売買戦略を集めたもの。
//
// 設計方針:
//
//	戦略は「注文を出さない」。売買の意見（domain.Signal）を返すだけにする。
//	注文への変換、数量の決定、リスク判定はすべてエンジン側の責務とする。
//	こうすると戦略は I/O もブローカーも時計も知らない純粋な関数に近づき、
//	モックなしで単体テストできる。バックテストと本番で同じコードが動くのも、
//	この分離があってこそ。
//
// 全銘柄横断であること:
//
//	OnBars は 1 銘柄ではなく Context（全銘柄の足）を受け取る。銘柄をまたぐ
//	判断——モメンタムの順位付け、ベンチマークとの比較、保有枠の配分——は
//	1 銘柄ずつ呼ぶ形では原理的に書けないため。
package strategy

import (
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

const (
	// directionFloor は意見を出すときの direction の下限。
	// entry_threshold（既定 0.3）以上でないと、順位最下位の銘柄が
	// 閾値に届かず「意見を出したのに永久に建たない」状態になる。
	directionFloor = 0.3
	// tradingDays は年率換算に使う年間営業日数。
	tradingDays = 252
)

// Strategy は売買戦略。
//
// OnBars は意見のある銘柄ぶんだけ Signal を返す。意見が無い銘柄については
// Signal を返さない。direction=0 を返すのとは意味が違う: 前者は「何も
// 言わない」、後者は「中立だと積極的に主張する」であり、合成時の扱いが変わる。
type Strategy interface {
	// Name は設定ファイルから参照する識別子。
	Name() string
	// Describe はログ用の説明。パラメータを含めると調査が楽になる。
	Describe() string
	// WarmupBars は判断に必要な過去の足の本数。足りない銘柄は
	// 例外にせず黙って飛ばす。
	WarmupBars() int
	// OnBars は判断を行い、売買の意見を返す。
	OnBars(ctx *Context) ([]domain.Signal, error)
}

// Screener は「なぜ落ちたか」を説明できる戦略。screen --show-failed が使う。
//
// 全戦略が実装するわけではない（単純な指標戦略は落選理由という概念が薄い）。
type Screener interface {
	Screen(ctx *Context, symbol string) ScreenResult
}

// ScreenResult は 1 銘柄のスクリーニング結果。
//
// Failed が空でなくても各条件の値を残すので、「なぜ落ちたか」を表示できる。
type ScreenResult struct {
	Symbol string
	Close  float64
	// Setup は通ったセットアップの名前（複数の入口を持つ戦略で使う）。
	Setup string
	// Score は順位付けに使う 0〜1 のスコア。
	Score float64
	// Failed は不合格の理由。空なら合格。
	Failed []string
	// Values は判定に使った値。meta と表示に流す。
	Values map[string]float64
}

// Passed は全条件を満たしたか。
func (r ScreenResult) Passed() bool { return len(r.Failed) == 0 }

// Meta は Signal の meta に載せる形へ写す。
func (r ScreenResult) Meta() map[string]any {
	meta := make(map[string]any, len(r.Values)+3)
	for k, v := range r.Values {
		meta[k] = v
	}
	meta["close"] = r.Close
	meta["score"] = r.Score
	if r.Setup != "" {
		meta["setup"] = r.Setup
	}
	return meta
}

func (r *ScreenResult) fail(reason string) { r.Failed = append(r.Failed, reason) }

func (r *ScreenResult) set(key string, value float64) {
	if r.Values == nil {
		r.Values = map[string]float64{}
	}
	r.Values[key] = value
}

// eachSymbol は「銘柄ごとに独立に判断する」戦略の共通処理。
//
// ウォームアップ不足の銘柄を飛ばす処理を 1 か所に集める。意見が無ければ
// eval は nil を返す。
func eachSymbol(ctx *Context, warmup int, eval func(symbol string, v View) *domain.Signal) []domain.Signal {
	var out []domain.Signal
	for _, sym := range ctx.Symbols() {
		v, ok := ctx.Bars(sym)
		if !ok || v.Len() < warmup {
			continue
		}
		if sig := eval(sym, v); sig != nil {
			out = append(out, *sig)
		}
	}
	return out
}

// signal は Signal を組み立てる。direction / confidence が範囲外なら
// 丸めて通す（戦略側の計算誤差で意見が丸ごと消えるほうが害が大きい）。
func signal(name, symbol string, direction, confidence float64, reason string, meta map[string]any) *domain.Signal {
	sig, err := domain.NewSignal(name, symbol,
		math.Max(-1, math.Min(1, direction)),
		clamp01(confidence), reason, meta)
	if err != nil {
		return nil
	}
	return &sig
}

// scoreToDirection は 0〜1 のスコアを direction（下限〜1.0）へ写す。
//
// サイジングは direction の高い順に枠を埋めるので、スコアがそのまま
// 採用順位になる。
func scoreToDirection(score float64) float64 {
	return math.Round((directionFloor+(1.0-directionFloor)*clamp01(score))*10000) / 10000
}

// benchmarkOK は地合いフィルタ。ベンチマークの終値がその SMA を上回るか。
//
// ベンチマークの足が無いときは false を返す。地合いを判断できないなら
// 黙って通すより止める（市場全体の下げ局面で建てるのが最も痛い）。
func benchmarkOK(ctx *Context, benchmark string, smaPeriod int) bool {
	if benchmark == "" {
		return true
	}
	v, ok := ctx.Bars(benchmark)
	if !ok || v.Len() < smaPeriod+1 {
		return false
	}
	value := last(v.SMA(smaPeriod))
	return !math.IsNaN(value) && last(v.Closes()) > value
}

// benchmarkROC はベンチマークの lookback 日騰落率（%）。判断できなければ NaN。
func benchmarkROC(ctx *Context, benchmark string, lookback int) float64 {
	if benchmark == "" {
		return math.NaN()
	}
	v, ok := ctx.Bars(benchmark)
	if !ok || v.Len() < lookback+1 {
		return math.NaN()
	}
	return last(v.ROC(lookback))
}
