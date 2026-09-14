package execute

import (
	"errors"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 信用建玉を照会できない朝: open は止める（持ち越しを知らずに建てると二重）、close は続ける
// （止めると今日の建玉が丸ごと持ち越しになる）。
func TestSettleCarriedPolicyOnMarginQueryFailure(t *testing.T) {
	env, rep := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	exitID := recordDeadExit(t, env, d, "7203", domain.SideBuy, 300)
	b := &stubBroker{
		getOrder:  filledLookup(map[string]int64{entryID: 300, exitID: 0}),
		positions: []domain.Position{margin("7203", -300)},
		marginErr: errors.New("信用が取れない"),
		balance:   richBalance(),
	}

	if _, err := SettleCarried(env, b, SettleAtOpen); err == nil {
		t.Error("open が信用建玉を照会できないのに止まらない")
	}

	s, err := SettleCarried(env, b, SettleAtClose)
	if err != nil {
		t.Fatalf("close が止まった: %v", err)
	}
	if s.CheckErr == nil || len(s.Carried) != 0 {
		t.Errorf("判定できなかったことが残っていない: %+v", s)
	}
	if len(b.placed) != 0 {
		t.Errorf("判定できないのに返済を送った: %d", len(b.placed))
	}
	found := false
	for _, e := range rep.errors {
		found = found || strings.HasPrefix(e, "daytrade.carry_check_failed")
	}
	if !found {
		t.Errorf("carry_check_failed の Error ログが無い: %v", rep.errors)
	}
}

// 照会できなかった注文がある間は、台帳外の信用建玉の返済（UnrecordedMargin）をしない。
// 建っていたかもしれない玉を台帳外と読んで返済すると、建っていなかった場合に反対建玉を作る。
func TestSettleCarriedSkipsSweepWhileUnconfirmed(t *testing.T) {
	env, rep := newEnv(t)
	d := env.Day.AddDate(0, 0, -1)
	req, _ := domain.NewOrderRequest("e-unknown", "6758", domain.SideBuy, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeMarginOpen)
	if err := env.Ledger.Record(req, d, string(domain.OrderStatusSubmitted), nil, nil); err != nil {
		t.Fatal(err)
	}
	b := &stubBroker{
		positions: []domain.Position{margin("6758", 100), margin("9984", 100)}, // 9984 は台帳外
		balance:   richBalance(),
	}
	s, err := SettleCarried(env, b, SettleAtOpen)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Unconfirmed) != 1 || len(s.Unrecorded) != 0 || len(b.placed) != 0 {
		t.Fatalf("照会できない注文があるのに台帳外を返済した: unconfirmed=%v unrecorded=%v placed=%d",
			s.Unconfirmed, s.Unrecorded, len(b.placed))
	}
	if len(rep.alerts) != 1 || !strings.Contains(rep.alerts[0], "照会できません") {
		t.Errorf("照会できないことを知らせていない: %v", rep.alerts)
	}

	// 照会できれば台帳外の 9984 を返済に回す（柵が効いていたのは unconfirmed のせいだった）
	price := decimal.NewFromInt(1000)
	b.getOrder = func(id string) (*domain.Order, error) {
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusFilled,
			FilledQuantity: decimal.NewFromInt(100), AvgFillPrice: &price}, nil
	}
	if s, err = SettleCarried(env, b, SettleAtOpen); err != nil {
		t.Fatal(err)
	}
	if len(s.Unrecorded) != 1 || s.Unrecorded[0].Target.Entry.Symbol != "9984" {
		t.Errorf("台帳外の建玉を拾えていない: %+v", s.Unrecorded)
	}
}

// 返済が通らなければ通知する（持ち越しが残る）。
func TestSettleCarriedAlertsOnRepaymentFailure(t *testing.T) {
	env, rep := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	exitID := recordDeadExit(t, env, d, "7203", domain.SideBuy, 300)
	b := &stubBroker{
		getOrder:  filledLookup(map[string]int64{entryID: 300, exitID: 0}),
		positions: []domain.Position{margin("7203", -300)},
		balance:   richBalance(),
		place: func(domain.OrderRequest) (*domain.OrderAck, error) {
			return nil, &broker.OrderRejectedError{Message: "11029"}
		},
	}
	s, err := SettleCarried(env, b, SettleAtOpen)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Carried) != 1 || len(s.Failures) != 1 {
		t.Fatalf("carried=%v failures=%v", s.Carried, s.Failures)
	}
	if len(rep.alerts) != 1 || !strings.Contains(rep.alerts[0], "通らず") {
		t.Errorf("返済の失敗を知らせていない: %v", rep.alerts)
	}
}
