package execute

import (
	"fmt"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dtquotes "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/quotes"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// Remaining は今日まだ建てられる件数（N − 建てた数）。負になりうる（余りをロングに回した
// 前の回が N を超えて建てた）。様子見（資金 0）とショートの一時停止（margin.paused）ではショートは 0。
func Remaining(cfg config.Config, placed Placed, watchOnly bool) (long, short int) {
	long = cfg.Capital.Positions() - placed.Long
	if cfg.Margin.Enabled && !watchOnly && !cfg.Margin.Paused {
		short = cfg.Margin.Positions() - placed.Short
	}
	return long, short
}

// DoneForToday は今日の建玉が全部建っていて、再実行が何もしなくてよいか。
//
// ショートの余りをロングに回す設定（margin.spill_to_long）では、ロングの枠は気配を見て
// ショートを決めるまで分からない——前の回が回した余りで N を超えて建てていても、その一部が
// 通らなかったなら残りがある。だから件数だけでは「済み」と言わない（SizeDay が金額で決める）。
func DoneForToday(cfg config.Config, placed Placed, watchOnly bool) bool {
	if placed.Total() == 0 {
		return false
	}
	long, short := Remaining(cfg, placed, watchOnly)
	if long > 0 || short > 0 {
		return false
	}
	return !(cfg.Margin.Enabled && cfg.Margin.SpillToLong && !watchOnly)
}

// Sizing は 1 つの脚の今回の件数と 1 注文の予算。
type Sizing struct {
	N         int
	Budget    decimal.Decimal
	Weighting string
}

// SizingNote は件数と予算を決める途中の出来事（人向けの 1 行とログ）。
type SizingNote struct {
	// Text は画面に出す 1 行（空なら出さない）。
	Text string
	// Level は "info" / "warn"（空ならログに残さない）。
	Level  string
	Code   string
	Msg    string
	Fields map[string]any
}

// EmitNotes は途中の出来事を画面とログに出す。
func EmitNotes(env Env, notes []SizingNote) {
	for _, n := range notes {
		if n.Text != "" {
			env.printf("%s\n", n.Text)
		}
		switch n.Level {
		case "info":
			env.Report.Info(n.Code, n.Msg, n.Fields)
		case "warn":
			env.Report.Warn(n.Code, n.Msg, n.Fields)
		}
	}
}

// SizingInput は今日の件数と予算を決める材料。
type SizingInput struct {
	Cfg     config.Config
	Verdict regime.Verdict
	// Placed は今日すでに建てた件数と金額（PlacedToday）。
	Placed Placed
	// TiedLong / TiedShort は持ち越しが拘束している資金（TiedCapital）。
	TiedLong, TiedShort decimal.Decimal
	// WatchOnly は様子見（資金 0）。WatchRows はそのとき見せる候補の数。
	WatchOnly bool
	WatchRows int
}

// DaySizing は SizeDay の結果。
type DaySizing struct {
	// Long / Short は今回の件数と予算。Short は ShortOpen のときだけ意味を持つ。
	Long, Short Sizing
	// ShortMultiplier はショートの資金の倍率（0 ならこの日はショートを建てない）。
	ShortMultiplier decimal.Decimal
	// ShortOpen はショートの順位付けと選定をするか（倍率が正で、今日の枠が残っている）。
	ShortOpen bool
	// Weak は資産曲線で縮められた日か。
	Weak  bool
	Notes []SizingNote

	in SizingInput
	// longDayN / longDayBudget はロングの 1 日の件数と予算（縮小・ショック・拘束の後、余りの前。
	// 今日建てた分を含む全体）。
	longDayN      int
	longDayBudget decimal.Decimal
	// shortTotal はショートの 1 日の総予算（倍率と拘束の後）。余りはここから今日使った分を引く。
	shortTotal decimal.Decimal
}

// SizeDay は今日の件数と 1 注文の予算を決める。open の順序（検証と同じ）:
//
//  1. 縮小（資産曲線。margin では long_shrink のときだけ）→ ショック日の倍率
//  2. 持ち越しの拘束（CapByTied）
//  3. ショートの倍率（通常日／弱い日 × ショック）と拘束
//  4. ショートの余りをロングへ（WithSpill。ショートの選定の後）
//
// 件数と予算は**その日の全体**で決めてから、今日すでに建てた分を引く——件数は
// 「全体 − 建てた数」、資金は「全体 × 予算 − 建てた金額」で頭打ちにする。再実行が
// 前の回の使った資金を数え忘れて上限を超えないため。1 回目（建てた分が無い）の結果は
// 引く前と同じ（バックテスト・evaluate と同じ式）。
func SizeDay(in SizingInput) DaySizing {
	cfg, v, placed := in.Cfg, in.Verdict, in.Placed
	d := DaySizing{in: in, Weak: v.Weak()}

	// --- ロング ---
	dayN := cfg.Capital.Positions()
	budget := cfg.Capital.BudgetPerOrder()
	weighting := cfg.Capital.Weighting
	if in.WatchOnly {
		// 様子見モードでは「買うとしたら」の上位を目安の予算で見せる
		dayN, budget, weighting = in.WatchRows, cfg.Capital.OrderBudget, "equal"
	}
	if d.Weak && (!cfg.Margin.Enabled || cfg.Margin.LongShrink) {
		budget = budget.Mul(decimal.NewFromFloat(v.Scale)).Round(0)
		d.note(SizingNote{Text: fmt.Sprintf("%s（1 注文 %s 円）", v.ScaleReason, cli.Yen(budget))})
	} else if d.Weak {
		d.note(SizingNote{Text: strings.Split(v.ScaleReason, "→")[0] + "→ ロングは縮めず、ショートを建てる合図にする"})
	}
	// ショック日（regime.shock_*）: 縮小の後にロングの予算へ倍率を掛ける（検証と同じ順序）
	if v.Shock && !in.WatchOnly {
		budget = budget.Mul(decimal.NewFromFloat(v.ShockLong)).Round(0)
		d.note(SizingNote{
			Text:  fmt.Sprintf("%s（ロング 1 注文 %s 円）", v.ShockReason, cli.Yen(budget)),
			Level: "info", Code: "daytrade.regime", Msg: "ショック日",
			Fields: map[string]any{"reason": v.ShockReason, "long_scale": v.ShockLong, "short_scale": v.ShockShort},
		})
	}
	// 持ち越しの拘束: 残りの資金で建てられる件数に減らす。1 注文の予算に満たなくても残りがあれば
	// 1 件を小さく建てる——一部が拘束されただけで一日を休むのは機会損失
	if in.TiedLong.IsPositive() && !in.WatchOnly {
		before := dayN
		dayN, budget = CapByTied(dayN, 0, cfg.Capital.MaxCapital, in.TiedLong, budget)
		d.note(SizingNote{
			Text: fmt.Sprintf("持ち越しがロングの資金 %s 円を拘束 → 今日は %d 件（1 注文 %s 円）",
				cli.Yen(in.TiedLong), max(dayN-placed.Long, 0), cli.Yen(budget)),
			Level: "warn", Code: "daytrade.carry", Msg: "持ち越しの拘束資金でロングを縮める",
			Fields: map[string]any{"tied": in.TiedLong.String(), "n_before": before, "n": dayN, "budget": budget.String()},
		})
	}
	d.longDayN, d.longDayBudget = dayN, budget
	if in.WatchOnly {
		d.Long = Sizing{N: dayN, Budget: budget, Weighting: weighting}
	} else {
		n, b := d.longAfterPlaced(dayN, budget)
		d.Long = Sizing{N: n, Budget: b, Weighting: weighting}
	}

	// --- ショート（[margin]）: 使わなかった資金をロングに回すため、選定はロングより先 ---
	if cfg.Margin.Enabled && cfg.Margin.Positions() > 0 && !in.WatchOnly {
		d.ShortMultiplier = cfg.Margin.MultiplierNormal
		if d.Weak {
			d.ShortMultiplier = cfg.Margin.MultiplierLongWeak
		}
		if v.Shock {
			d.ShortMultiplier = d.ShortMultiplier.Mul(decimal.NewFromFloat(v.ShockShort))
		}
		// 一時停止中はショートを建てないので「ショートだけ休む」日も倍率を残し、枠をロングへ回す
		if v.ShortOff && !cfg.Margin.Paused {
			d.ShortMultiplier = decimal.Zero
			d.note(SizingNote{
				Text:  v.ShortOffReason,
				Level: "info", Code: "daytrade.regime", Msg: "ショートだけ休む",
				Fields: map[string]any{"reason": v.ShortOffReason},
			})
		}
	}
	if !d.ShortMultiplier.IsPositive() {
		return d
	}
	if cfg.Margin.Paused {
		d.note(SizingNote{
			Text:  "ショートは一時停止中（margin.paused）。枠はロングに回す",
			Level: "info", Code: "daytrade.regime", Msg: "ショートの一時停止",
			Fields: map[string]any{"reason": "margin.paused"},
		})
	}
	shortDayN := cfg.Margin.Positions()
	shortBudget := cfg.Margin.BudgetPerOrder().Mul(d.ShortMultiplier).Round(0)
	_, remainingShort := Remaining(cfg, placed, in.WatchOnly)
	if in.TiedShort.IsPositive() {
		before := shortDayN
		shortDayN, shortBudget = CapByTied(shortDayN, 0, cfg.Margin.MaxCapital, in.TiedShort, shortBudget)
		if remainingShort > 0 {
			d.note(SizingNote{
				Text: fmt.Sprintf("持ち越しがショートの資金 %s 円を拘束 → 今日は %d 件（1 注文 %s 円）",
					cli.Yen(in.TiedShort), max(shortDayN-placed.Short, 0), cli.Yen(shortBudget)),
				Level: "warn", Code: "daytrade.carry", Msg: "持ち越しの拘束資金でショートを縮める",
				Fields: map[string]any{"tied": in.TiedShort.String(), "n_before": before, "n": shortDayN, "budget": shortBudget.String()},
			})
		}
	}
	d.shortTotal = shortBudget.Mul(decimal.NewFromInt(int64(shortDayN)))
	if remainingShort <= 0 {
		return d
	}
	d.ShortOpen = true
	// 今日すでに使った資金: 建てたショートと、前の回がロングへ回して使った余り（ロングの
	// 1 日の枠を超えて建てた分）。長短の合計は margin.max_capital を超えない
	left := d.shortTotal.Sub(placed.ShortAmount)
	if cfg.Margin.SpillToLong {
		if overflow := placed.LongAmount.Sub(d.longDayBudget.Mul(decimal.NewFromInt(int64(d.longDayN)))); overflow.IsPositive() {
			left = left.Sub(overflow)
		}
	}
	n, b := capByAmount(shortDayN-placed.Short, left, shortBudget)
	d.Short = Sizing{N: n, Budget: b, Weighting: cfg.Margin.Weighting}
	return d
}

// longAfterPlaced はロングの 1 日の件数と予算（dayN × budget）から、今日すでに建てた分を引いた
// 今回の件数と予算。件数は dayN − 建てた数、資金は dayN × budget − 建てた金額で頭打ち。
func (d DaySizing) longAfterPlaced(dayN int, budget decimal.Decimal) (int, decimal.Decimal) {
	placed := d.in.Placed
	if placed.Long == 0 && !placed.LongAmount.IsPositive() {
		return dayN, budget
	}
	left := budget.Mul(decimal.NewFromInt(int64(dayN))).Sub(placed.LongAmount)
	return capByAmount(dayN-placed.Long, left, budget)
}

// WithSpill はショートの余り（margin.spill_to_long）をロングに回した後のロングの件数と予算。
//
// 余り = ショートの 1 日の総予算（倍率と拘束の後）−（今日すでに建てたショートの金額 + 今回の
// ショートの選定）。前の回の余りをもう一度足さない——再実行が「残りの件数 × 1 注文の予算」を
// 余りとして数え直すと、ショートの使用額と余りの合計が margin.max_capital を超える。
// ロングは「1 日の件数 + 余り」の全体を決めてから、今日建てた件数と金額を引く。
// 回さなかった（設定が偽・倍率 0・余りなし）なら spill はゼロで、ロングは SizeDay のまま。
func (d DaySizing) WithSpill(shortPicks []selection.Pick) (long Sizing, spill decimal.Decimal, notes []SizingNote) {
	cfg := d.in.Cfg
	long = d.Long
	if !cfg.Margin.SpillToLong || !d.ShortMultiplier.IsPositive() || d.in.WatchOnly {
		return long, decimal.Zero, nil
	}
	used := d.in.Placed.ShortAmount
	for _, pk := range shortPicks {
		used = used.Add(pk.Amount())
	}
	spill = d.shortTotal.Sub(used)
	if !spill.IsPositive() {
		return long, decimal.Zero, nil
	}
	dayN, budget := selection.SpillInto(d.longDayN, d.longDayBudget, cfg.Capital.BudgetPerOrder(), spill, cfg.Capital.MaxPositions)
	long.N, long.Budget = d.longAfterPlaced(dayN, budget)
	notes = append(notes, SizingNote{
		Text:  fmt.Sprintf("ショートの余り %s 円をロングに回す → N=%d、1 注文 %s 円", cli.Yen(spill), long.N, cli.Yen(long.Budget)),
		Level: "info", Code: "daytrade.regime", Msg: "ショートの余りをロングへ",
		Fields: map[string]any{"spill": spill.String(), "n": long.N, "budget": long.Budget.String(), "n_day": dayN},
	})
	return long, spill, notes
}

func (d *DaySizing) note(n SizingNote) { d.Notes = append(d.Notes, n) }

// RankQuotes は順位付けに使う気配——今日建てた銘柄と、台帳外として返済に回した銘柄を落とし、
// signal.skip_opened なら既に寄っている銘柄も落とす。落とした「寄っていた」銘柄を返す。
//
// 除外は**順に重ねる**。寄っている銘柄の除外を元の気配からやり直すと、建てた銘柄が
// 候補に戻り、同じ日に同じ銘柄を重ねて建てる。
func RankQuotes(quotes map[string]selection.Quote, placed map[string]domain.Side, swept map[string]struct{},
	skipOpened bool) (kept map[string]selection.Quote, opened []string) {
	kept = quotes
	if len(placed) > 0 {
		// 同じ日に建てた銘柄は重ねて建てない（再実行は残りの枚数を別の銘柄で埋める）
		kept = dtquotes.DropSymbols(kept, placed)
	}
	if len(swept) > 0 {
		// 返済注文が今日の下に記録されるので、同じ銘柄を今日建てると引けの手仕舞いが
		// それを「発注済み」と取り違えて当日の建玉を残す
		kept = dtquotes.DropSymbols(kept, swept)
	}
	if skipOpened {
		kept, opened = dtquotes.DropOpened(kept)
	}
	return kept, opened
}
