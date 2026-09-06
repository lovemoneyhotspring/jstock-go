package strategy

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// MarginBalance は信用需給（信用倍率＝買残 ÷ 売残）の銘柄横断の分位で意見を出す。
//
// 根拠（docs/research/2026-09-jp-level-bounce.md の心理データ節）: 売買代金上位 300 と
// 801〜1100 位の 2016〜2026 年で、週次の信用倍率を週ごとに五分位に分けると、下位（売り長）は
// 20 日後の超過リターンが −0.5〜−0.8%、上位（買い長）は +0.1〜+0.5%。11 年中 9〜11 年で同じ向きで、
// 過去 20 日・120 日のモメンタムで層別しても残る。俗説の「買残が多いと上値が重い」は出ない。
// 信用の売り方が情報を持っている、と読む。
//
// mode:
//
//	filter … 下位 avoid_below の銘柄にだけ −1 を出す（他の戦略の買いを打ち消す）。
//	         weighted_vote では他の戦略の重みの合計以上の重みを付けないと打ち消せない。
//	tilt   … 上位 favor_above に分位に応じた買い意見も出す。単独で使うと
//	         「買い長の上位から順に買う」ファクター戦略になる。
//
// 保有中の銘柄が下位に落ちても、exit_held を立てない限り黙る（手仕舞いは他の戦略とストップに任せる）。
type MarginBalance struct {
	Mode       string
	AvoidBelow float64
	FavorAbove float64
	ExitHeld   bool
}

// MarginBalanceOptions は MarginBalance の設定。
type MarginBalanceOptions struct {
	Mode       string
	AvoidBelow float64
	FavorAbove float64
	ExitHeld   bool
}

// DefaultMarginBalanceOptions は研究の五分位に合わせた既定値。
func DefaultMarginBalanceOptions() MarginBalanceOptions {
	return MarginBalanceOptions{Mode: "filter", AvoidBelow: 0.2, FavorAbove: 0.8}
}

// NewMarginBalance は信用需給戦略を作る。
func NewMarginBalance(o MarginBalanceOptions) (*MarginBalance, error) {
	if o.Mode != "filter" && o.Mode != "tilt" {
		return nil, fmt.Errorf("mode は filter / tilt: %s", o.Mode)
	}
	if !(0 <= o.AvoidBelow && o.AvoidBelow < 1) {
		return nil, fmt.Errorf("avoid_below は 0 以上 1 未満: %g", o.AvoidBelow)
	}
	if !(o.AvoidBelow < o.FavorAbove && o.FavorAbove <= 1) {
		return nil, fmt.Errorf("avoid_below < favor_above ≦ 1 を満たすこと: %g, %g", o.AvoidBelow, o.FavorAbove)
	}
	return &MarginBalance{Mode: o.Mode, AvoidBelow: o.AvoidBelow, FavorAbove: o.FavorAbove, ExitHeld: o.ExitHeld}, nil
}

func (s *MarginBalance) Name() string    { return "margin_balance" }
func (s *MarginBalance) WarmupBars() int { return 1 }
func (s *MarginBalance) Describe() string {
	return fmt.Sprintf("%s(mode=%s, avoid<%.0f%%, favor≥%.0f%%, exit_held=%t)",
		s.Name(), s.Mode, s.AvoidBelow*100, s.FavorAbove*100, s.ExitHeld)
}

func (s *MarginBalance) OnBars(ctx *Context) ([]domain.Signal, error) {
	var out []domain.Signal
	for _, sym := range ctx.Symbols() {
		rank, ok := ctx.MarginRatioRank(sym)
		if !ok {
			continue
		}
		rec, _ := ctx.Margin(sym)
		meta := map[string]any{"margin_rank": rank, "margin_ratio": rec.Ratio(), "margin_date": rec.Date}
		switch {
		case rank < s.AvoidBelow:
			if ctx.HasPosition(sym) && !s.ExitHeld {
				continue
			}
			if sig := signal(s.Name(), sym, -1.0, 1.0,
				fmt.Sprintf("売り長: 信用倍率 %.2f は下位 %.0f%%（%s 時点）", rec.Ratio(), rank*100, rec.Date), meta); sig != nil {
				out = append(out, *sig)
			}
		case s.Mode == "tilt" && rank >= s.FavorAbove:
			score := (rank - s.FavorAbove) / (1 - s.FavorAbove)
			if sig := signal(s.Name(), sym, scoreToDirection(score), 1.0,
				fmt.Sprintf("買い長: 信用倍率 %.2f は上位 %.0f%%（%s 時点）", rec.Ratio(), (1-rank)*100, rec.Date), meta); sig != nil {
				out = append(out, *sig)
			}
		}
	}
	return out, nil
}
