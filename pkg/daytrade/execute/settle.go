package execute

import (
	"fmt"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
)

// SettlePhase は持ち越しを片付ける場面。判定できなかったときの方針が場面で逆になる。
type SettlePhase struct {
	// Phrase は台帳の理由に残す言葉。
	Phrase string
	// ContinueOnError が真なら、判定できなくても error を返さず当日の処理を続けさせる。
	ContinueOnError bool
}

var (
	// SettleAtOpen は寄付。判定できなければ**止める**——持ち越しを知らずに新規に建てると二重になりうる。
	SettleAtOpen = SettlePhase{Phrase: "翌寄りで持ち越しを手仕舞い"}
	// SettleAtClose は引け（朝に寄らず失効した返済の保険）。判定できなくても**当日の手仕舞いは
	// 続ける**——ここで止めると今日の建玉が丸ごと持ち越しになる。
	SettleAtClose = SettlePhase{Phrase: "引けで持ち越しを手仕舞い", ContinueOnError: true}
)

// Settlement は SettleCarried の結果。ダイジェストへの記録は呼び出し側が行う。
type Settlement struct {
	// Carried は判定した持ち越し（返済に回した台帳外の信用建玉を含む）。
	Carried []Carried
	// Held は照会した建玉。発注直前の台帳外の検査と引けの保険の確認も同じものを使う
	// （1 実行の建玉照会を 1 回＝現物と信用で 2 電文にする）。
	Held broker.LegPositions
	// Unconfirmed は照会できなかった注文。空でなければ台帳外の判定はしていない。
	Unconfirmed []string
	// Unrecorded は台帳外として返済に回した信用建玉（Carried にも入っている）。
	Unrecorded []Carried
	// Failures は通らなかった返済。
	Failures []string
	// CheckErr は判定できなかった理由（ContinueOnError のときだけ。Carried は空）。
	CheckErr error
}

// SettleCarried は前営業日以前の持ち越しと台帳外の信用建玉を判定し、成行で手仕舞う
// （open / close 共用）。
//
// 信用はデイトレでしか使わないので、台帳が説明できない信用建玉も自分の玉として返済する
// （UnrecordedMargin）。現物は積立の保有かもしれないので触らない。
//
// 照会できなかった注文がある間は**台帳外かどうかを決めない**——建っていたかもしれない
// 玉を「台帳外」と読んで返済すると、建っていなかった場合に反対建玉を作る。照会できなかった
// ことと、通らなかった返済は通知する（次の実行が同じ判定でもう一度送る。台帳は建てた日の
// 下に記録され、冪等）。
func SettleCarried(env Env, b broker.Broker, phase SettlePhase) (Settlement, error) {
	s := Settlement{Held: broker.PositionsByLeg(b)}
	fail := func(err error) (Settlement, error) {
		s.Carried, s.Unrecorded = nil, nil
		if !phase.ContinueOnError {
			return s, err
		}
		s.CheckErr = err
		env.printf("持ち越しを判定できません。当日の手仕舞いだけ行います: %v\n", err)
		env.Report.Error("daytrade.carry_check_failed", "持ち越しを判定できず当日の手仕舞いだけ行う",
			map[string]any{"day": env.dayText(), "error": err.Error()})
		return s, nil
	}

	carried, unconfirmed, err := CarriedPositions(env, b, s.Held)
	if err != nil {
		return fail(err)
	}
	s.Unconfirmed = unconfirmed
	if len(unconfirmed) > 0 {
		env.Report.Alert("デイトレ: 持ち越しの建玉を照会できません。口座を確認してください", strings.Join(unconfirmed, "\n"))
	} else {
		unrecorded, err := UnrecordedMargin(env, s.Held, carried)
		if err != nil {
			return fail(err)
		}
		if len(unrecorded) > 0 {
			env.Report.Alert(fmt.Sprintf("デイトレ: 台帳に無い信用建玉 %d 件を返済します", len(unrecorded)),
				strings.Join(CarriedLines(unrecorded), "\n"))
			s.Unrecorded = unrecorded
			carried = append(carried, unrecorded...)
		}
	}
	s.Carried = carried
	if len(carried) == 0 {
		return s, nil
	}
	lines := CarriedLines(carried)
	env.printf("持ち越し %d 件を成行で手仕舞います: %s\n", len(carried), strings.Join(lines, "、"))
	env.Report.Warn("daytrade.carry", "持ち越しを手仕舞う", map[string]any{
		"count": len(carried), "positions": lines, "phrase": phase.Phrase})
	if failures := ReturnCarried(env, b, carried, phase.Phrase); len(failures) > 0 {
		env.Report.Alert(fmt.Sprintf("デイトレ: %d 件の持ち越しの手仕舞いが通らず", len(failures)), strings.Join(failures, "\n"))
		s.Failures = failures
	}
	return s, nil
}

// CarriedLines は持ち越しを人向けの 1 行ずつにする。
func CarriedLines(carried []Carried) []string {
	lines := make([]string, 0, len(carried))
	for _, c := range carried {
		lines = append(lines, c.String())
	}
	return lines
}
