package execute

import (
	"errors"
	"strings"
	"testing"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/window"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
)

// writeBars は from〜to の毎日に、同じ終値の足を書く。
func writeBars(t *testing.T, store *data.BarStore, symbol, from, to string, closePrice int64) {
	t.Helper()
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		t.Fatal(err)
	}
	c := dec(closePrice)
	var bars []domain.Bar
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		bar, err := domain.NewBar(symbol, d.Format("2006-01-02"), c, c, c, c, dec(1000))
		if err != nil {
			t.Fatal(err)
		}
		bars = append(bars, bar)
	}
	if err := store.Write(symbol, bars); err != nil {
		t.Fatal(err)
	}
}

// planConfig は 1306.T を定額で積み立てる設定。
func planConfig(budget int64, w window.TradingWindow) *accumcfg.AccumConfig {
	return &accumcfg.AccumConfig{
		Execution: accumcfg.ExecutionConfig{OrderType: "limit"},
		Tactics: []accumcfg.TacticEntry{{
			ID: "A", Tactic: "constant", Symbols: []string{"1306.T"},
			MonthlyBudget: dec(budget), Window: w,
		}},
	}
}

// recordOrder は 1306 の注文を 1 件、指定の状態・計画月・額で台帳に入れる。
func recordOrder(t *testing.T, led *ledger.Ledger, id, status string, planMonth *string, amount int64) domain.OrderRequest {
	t.Helper()
	req := newRequest(t, id, "1306", 100)
	amt := dec(amount)
	mkt := domain.MarketJP
	if err := led.Record(req, status, nil, planMonth, &amt, &mkt); err != nil {
		t.Fatal(err)
	}
	return req
}

func strPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }

// --- PlanOrders --------------------------------------------------------

func TestPlanOrders(t *testing.T) {
	at := func(day string, hour, minute int) time.Time {
		d, err := time.ParseInLocation("2006-01-02", day, clock.Tokyo)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, clock.Tokyo)
	}
	// 2026-09-14 は月曜
	monday := at("2026-09-14", 14, 30)

	cases := []struct {
		name         string
		now          time.Time
		from, to     string // 足の範囲。空なら足を書かない
		budget       int64
		setup        func(t *testing.T, cfg *accumcfg.AccumConfig, led *ledger.Ledger)
		ignoreWindow bool
		// noStart なら開始日を台帳に入れない（既定は前年から積み立てている銘柄）
		noStart   bool
		markStart bool
		wantRows  int
		wantQty   int64  // 0 なら注文なし
		wantNote  string // 含むべき文字列。空なら Note なし
		check     func(t *testing.T, po PlannedOrder)
	}{
		{
			name: "通常の日は予算ぶんを指値で出す", now: monday,
			from: "2026-08-25", to: "2026-09-14", budget: 200_000,
			wantRows: 1, wantQty: 100,
			check: func(t *testing.T, po PlannedOrder) {
				// 終値 1000 × (1 + 0.01) = 1010。当日の足（09-14）は判断に使わない
				if po.LimitPrice == nil || !po.LimitPrice.Equal(dec(1010)) {
					t.Errorf("指値 = %v, want 1010", po.LimitPrice)
				}
				if !po.Amount.Equal(dec(200_000)) || !po.Target.Equal(dec(200_000)) || !po.Placed.IsZero() {
					t.Errorf("額 = due %s / target %s / placed %s", po.Amount, po.Target, po.Placed)
				}
				if po.JudgedOn != "2026-09-13" || po.Month != "2026-09-01" || po.Tactic != "constant" {
					t.Errorf("判断の材料 = %s / %s / %s", po.JudgedOn, po.Month, po.Tactic)
				}
				wantID := domain.MakeClientOrderID("2026-09-14", "1306", domain.SideBuy, dec(100))
				if po.Request.ClientOrderID != wantID {
					t.Errorf("client_order_id = %s, want %s（日付・銘柄・数量で決まり、再実行で一致する）", po.Request.ClientOrderID, wantID)
				}
				if po.Request.OrderType != domain.OrderTypeLimit || po.Request.Side != domain.SideBuy {
					t.Errorf("注文の種類 = %s / %s", po.Request.OrderType, po.Request.Side)
				}
			},
		},
		{
			name: "足が無ければ見送りの行だけ残す", now: monday, budget: 200_000,
			wantRows: 1, wantNote: "足データなし",
			check: func(t *testing.T, po PlannedOrder) {
				if !po.Failed {
					t.Error("失敗として印が付いていない（通知されない。A6）")
				}
			},
		},
		{
			name: "今月分を発注済みなら何も作らない", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 200_000,
			setup: func(t *testing.T, _ *accumcfg.AccumConfig, led *ledger.Ledger) {
				recordOrder(t, led, "済み", string(domain.OrderStatusFilled), strPtr("2026-09-01"), 200_000)
			},
			wantRows: 0,
		},
		{
			name: "差額が基本目標未満でリリース日でもなければ持ち越す", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 200_000,
			setup: func(t *testing.T, _ *accumcfg.AccumConfig, led *ledger.Ledger) {
				recordOrder(t, led, "一部", string(domain.OrderStatusFilled), strPtr("2026-09-01"), 150_000)
			},
			wantRows: 1, wantNote: "持ち越し",
			check: func(t *testing.T, po PlannedOrder) {
				if !po.Amount.Equal(dec(50_000)) {
					t.Errorf("差額 = %s, want 50000", po.Amount)
				}
			},
		},
		{
			name: "入金日の翌日なら小さな差額も出す", now: at("2026-09-02", 14, 30),
			from: "2026-08-25", to: "2026-09-02", budget: 200_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, led *ledger.Ledger) {
				cfg.Execution.LotSizeOverrides = map[string]int{"1306": 1}
				recordOrder(t, led, "一部", string(domain.OrderStatusFilled), strPtr("2026-09-01"), 150_000)
			},
			// floor(50000 / 1010) = 49 株（単元 1 株）
			wantRows: 1, wantQty: 49,
		},
		{
			name: "単元に届かなければ見送り", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 50_000,
			wantRows: 1, wantNote: "単元株数（100株）に満たない",
		},
		{
			name: "発注時間帯の外なら注文を作らない", now: at("2026-09-14", 10, 0),
			from: "2026-08-25", to: "2026-09-13", budget: 200_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, _ *ledger.Ledger) {
				cfg.Tactics[0].Window = window.Default()
			},
			wantRows: 1, wantNote: "発注時間帯の外",
		},
		{
			name: "ignoreWindow なら時間帯の外でも作る", now: at("2026-09-14", 10, 0),
			from: "2026-08-25", to: "2026-09-13", budget: 200_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, _ *ledger.Ledger) {
				cfg.Tactics[0].Window = window.Default()
			},
			ignoreWindow: true,
			wantRows:     1, wantQty: 100,
		},
		{
			name: "最終足が max_stale_days を超えたら見送り", now: monday,
			from: "2026-08-25", to: "2026-09-07", budget: 200_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, _ *ledger.Ledger) {
				cfg.Execution.MaxStaleDays = 6
			},
			wantRows: 1, wantNote: "古いため見送り",
		},
		{
			name: "max_stale_days ちょうどなら出す", now: monday,
			from: "2026-08-25", to: "2026-09-08", budget: 200_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, _ *ledger.Ledger) {
				cfg.Execution.MaxStaleDays = 6
			},
			wantRows: 1, wantQty: 100,
		},
		{
			name: "月の初日は今月の確定足が無いので作らない", now: at("2026-09-01", 14, 30),
			from: "2026-08-25", to: "2026-09-01", budget: 200_000,
			wantRows: 0,
		},
		{
			name: "無効な戦略は飛ばす", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 200_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, _ *ledger.Ledger) {
				disabled := false
				cfg.Tactics[0].Enabled = &disabled
			},
			wantRows: 0,
		},
		{
			name: "成行の設定なら成行で出す", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 200_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, _ *ledger.Ledger) {
				cfg.Execution.OrderType = "market"
			},
			wantRows: 1, wantQty: 100,
			check: func(t *testing.T, po PlannedOrder) {
				if po.Request.OrderType != domain.OrderTypeMarket {
					t.Errorf("注文の種類 = %s, want market", po.Request.OrderType)
				}
				if po.Request.LimitPrice != nil {
					t.Errorf("成行に指値が載っている: %s", po.Request.LimitPrice)
				}
			},
		},
		{
			// 設定の表記（"1306.T"）で書いた売買単位の上書きが発注の表記（"1306"）で効く（A5）。
			// 以前は一度も当たらず、既定の 100 株で丸めて見送りになっていた
			name: "売買単位の上書きは足の表記のキーでも効く", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 50_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, _ *ledger.Ledger) {
				cfg.Execution.LotSizeOverrides = map[string]int{"1306.T": 10}
			},
			// floor(50000 / 1010) = 49 → 10 株単位で 40 株
			wantRows: 1, wantQty: 40,
		},
		{
			// 開始日の記録が無ければ今日（9/14）を開始日として日割りする。dry-run は台帳に残さない（A4）
			name: "開始日が無ければ今日から日割りし、dry-run は記録しない", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 300_000, noStart: true,
			// 9 月は 30 日、9/14 から 17 日 → 300000 × 17/30 = 170000 → floor(170000/1010)=168 → 100 株
			wantRows: 1, wantQty: 100,
			check: func(t *testing.T, po PlannedOrder) {
				if !po.Target.Equal(dec(170_000)) || !strings.Contains(po.Reason, "日割り") {
					t.Errorf("目標 = %s（%s）, want 170000 の日割り", po.Target, po.Reason)
				}
			},
		},
		{
			name: "本発注の run は開始日を台帳に残す", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 300_000, noStart: true, markStart: true,
			wantRows: 1, wantQty: 100,
			check: func(t *testing.T, po PlannedOrder) {
				if !po.Target.Equal(dec(170_000)) {
					t.Errorf("目標 = %s, want 170000", po.Target)
				}
			},
		},
		{
			name: "記録済みの開始日を使う", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 300_000, noStart: true,
			setup: func(t *testing.T, _ *accumcfg.AccumConfig, led *ledger.Ledger) {
				if err := led.MarkStarted("1306", "2026-09-10"); err != nil {
					t.Fatal(err)
				}
			},
			// 9/10 から 21 日 → 300000 × 21/30 = 210000 → floor(210000/1010)=207 → 200 株
			wantRows: 1, wantQty: 200,
		},
		{
			// 判定用の足が無いとき、黙って買う銘柄自身の足で判定しない（A7）
			name: "判定用の足が無ければ見送り、失敗として残す", now: monday,
			from: "2026-08-25", to: "2026-09-13", budget: 200_000,
			setup: func(t *testing.T, cfg *accumcfg.AccumConfig, _ *ledger.Ledger) {
				cfg.Tactics[0].SignalSymbol = "^IXIC"
			},
			wantRows: 1, wantNote: "判定用の足（^IXIC）",
			check: func(t *testing.T, po PlannedOrder) {
				if !po.Failed {
					t.Error("失敗として印が付いていない（通知されない）")
				}
			},
		},
		{
			name: "前月の買い残しを当月に繰り越す", now: monday,
			from: "2026-08-01", to: "2026-09-13", budget: 200_000,
			setup: func(t *testing.T, _ *accumcfg.AccumConfig, led *ledger.Ledger) {
				recordOrder(t, led, "前月", string(domain.OrderStatusFilled), strPtr("2026-08-01"), 150_000)
			},
			// 200000 + 前月の残り 50000 = 250000 → floor(250000/1010)=247 → 200 株
			wantRows: 1, wantQty: 200,
			check: func(t *testing.T, po PlannedOrder) {
				if !po.Amount.Equal(dec(250_000)) || !po.Target.Equal(dec(250_000)) {
					t.Errorf("額 = due %s / target %s, want 250000", po.Amount, po.Target)
				}
				if !strings.Contains(po.Reason, "前月からの繰り越し 50000") {
					t.Errorf("理由に繰り越しが無い: %s", po.Reason)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := data.NewBarStore(t.TempDir())
			if tc.from != "" {
				writeBars(t, store, "1306.T", tc.from, tc.to, 1000)
			}
			led := newLedger(t)
			if !tc.noStart {
				if err := led.MarkStarted("1306", "2025-01-01"); err != nil {
					t.Fatal(err)
				}
			}
			cfg := planConfig(tc.budget, window.Unrestricted())
			if tc.setup != nil {
				tc.setup(t, cfg, led)
			}
			startedBefore, err := led.StartedOn("1306")
			if err != nil {
				t.Fatal(err)
			}

			orders, stale, err := PlanOrders(cfg, store, led, tc.now, tc.ignoreWindow, tc.markStart, nil)
			if err != nil {
				t.Fatal(err)
			}
			startedAfter, err := led.StartedOn("1306")
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case startedBefore != nil:
				if startedAfter == nil || *startedAfter != *startedBefore {
					t.Errorf("記録済みの開始日が変わった: %v → %v", *startedBefore, startedAfter)
				}
			case tc.markStart && tc.wantRows > 0:
				if want := tc.now.Format("2006-01-02"); startedAfter == nil || *startedAfter != want {
					t.Errorf("開始日 = %v, want %s（本発注の run が残す）", startedAfter, want)
				}
			case !tc.markStart:
				if startedAfter != nil {
					t.Errorf("dry-run が開始日 %s を台帳に残した", *startedAfter)
				}
			}
			if len(stale) != 0 {
				t.Errorf("判定用の銘柄が無いのに古い警告: %v", stale)
			}
			if len(orders) != tc.wantRows {
				t.Fatalf("行数 = %d, want %d: %+v", len(orders), tc.wantRows, orders)
			}
			if tc.wantRows == 0 {
				return
			}
			po := orders[0]
			if tc.wantQty == 0 {
				if po.Request != nil {
					t.Errorf("注文を作るべきでない: %+v", po.Request)
				}
			} else {
				if po.Request == nil {
					t.Fatalf("注文が無い（note: %s）", po.Note)
				}
				if !po.Quantity.Equal(dec(tc.wantQty)) || !po.Request.Quantity.Equal(dec(tc.wantQty)) {
					t.Errorf("株数 = %s, want %d", po.Quantity, tc.wantQty)
				}
			}
			if tc.wantNote == "" {
				if po.Note != "" {
					t.Errorf("見送りの note が付いている: %s", po.Note)
				}
			} else if !strings.Contains(po.Note, tc.wantNote) {
				t.Errorf("note = %q, want に %q を含む", po.Note, tc.wantNote)
			}
			if tc.check != nil {
				tc.check(t, po)
			}
		})
	}
}

// 開始日の記録が無くても注文が既にある銘柄は、最初の注文の日を開始日にする（A4）。
// 開始日を記録する経路が無かった間に発注・取り込みした銘柄（本番の台帳の 563A・1629・2559 は
// 2026-09-11 の注文だけがある）を、今日から始めたものとして日割りしない。
func TestPlanOrdersStartsFromFirstOrderWhenUnmarked(t *testing.T) {
	store := data.NewBarStore(t.TempDir())
	writeBars(t, store, "1306.T", "2026-08-25", "2026-09-13", 1000)
	cfg := planConfig(300_000, window.Unrestricted())
	led := newLedger(t)
	// 計画月の違う注文（当月・前月の額には効かない）。placed_at は実時計で入る
	recordOrder(t, led, "以前", string(domain.OrderStatusRejected), strPtr("2026-06-01"), 100_000)
	first, err := led.FirstOrderDay("1306", clock.Tokyo)
	if err != nil || first == nil {
		t.Fatalf("最初の注文日 = %v, %v", first, err)
	}

	now := time.Date(2026, 9, 14, 14, 30, 0, 0, clock.Tokyo)
	if _, _, err := PlanOrders(cfg, store, led, now, false, true, nil); err != nil {
		t.Fatal(err)
	}
	got, err := led.StartedOn("1306")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != *first {
		t.Errorf("開始日 = %v, want %s（最初の注文の日。今日 %s ではない）", got, *first, now.Format("2006-01-02"))
	}
}

// 売買単位は設定の上書き → ブローカーの銘柄情報 → 既定 100 株の順に決める。
// 設定に書かない 1 株単位の ETF（2559）を既定の 100 株で丸めると、予算が 100 株に届かず
// 毎回「単元未満」で見送りになっていた。
func TestPlanOrdersLotSizeFromBroker(t *testing.T) {
	now := time.Date(2026, 9, 14, 14, 30, 0, 0, clock.Tokyo)
	cases := []struct {
		name      string
		overrides map[string]int
		lots      map[string]decimal.Decimal
		want      int64
	}{
		// 25000 / 1010 = 24 株
		{name: "銘柄情報の 1 株単位で丸める", lots: map[string]decimal.Decimal{"1306": dec(1)}, want: 24},
		{name: "設定の上書きが銘柄情報より先", overrides: map[string]int{"1306.T": 10},
			lots: map[string]decimal.Decimal{"1306": dec(1)}, want: 20},
		{name: "どちらも無ければ既定の 100 株（単元未満で見送り）", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := data.NewBarStore(t.TempDir())
			writeBars(t, store, "1306.T", "2026-08-25", "2026-09-13", 1000)
			cfg := planConfig(25_000, window.Unrestricted())
			cfg.Execution.LotSizeOverrides = tc.overrides
			led := newLedger(t)
			if err := led.MarkStarted("1306", "2025-01-01"); err != nil {
				t.Fatal(err)
			}
			orders, _, err := PlanOrders(cfg, store, led, now, false, false, tc.lots)
			if err != nil {
				t.Fatal(err)
			}
			if len(orders) != 1 {
				t.Fatalf("行数 = %d: %+v", len(orders), orders)
			}
			po := orders[0]
			if tc.want == 0 {
				if po.Request != nil || !strings.Contains(po.Note, "単元株数") {
					t.Errorf("見送りになるべき: %+v", po)
				}
				return
			}
			if po.Request == nil || !po.Request.Quantity.Equal(dec(tc.want)) {
				t.Errorf("株数 = %v（%s）, want %d", po.Request, po.Note, tc.want)
			}
		})
	}
}

// 銘柄情報を引く対象は有効な戦略の発注できる銘柄だけ（指数・止めた戦略・重複を除く）。
func TestOrderableSymbols(t *testing.T) {
	cfg := &accumcfg.AccumConfig{Tactics: []accumcfg.TacticEntry{
		{ID: "a", Symbols: []string{"2559.T", "^N225", "1629.T"}},
		{ID: "b", Symbols: []string{"2559.T"}},
		{ID: "c", Symbols: []string{"1306.T"}, Enabled: boolPtr(false)},
	}}
	got := strings.Join(orderableSymbols(cfg), ",")
	if got != "2559,1629" {
		t.Errorf("orderableSymbols = %s, want 2559,1629", got)
	}
}

// 設定に未知の戦略があれば計画を立てない（黙って飛ばすと積立が止まったことに気付けない）。
func TestPlanOrdersRejectsUnknownTactic(t *testing.T) {
	store := data.NewBarStore(t.TempDir())
	writeBars(t, store, "1306.T", "2026-08-25", "2026-09-13", 1000)
	cfg := planConfig(200_000, window.Unrestricted())
	cfg.Tactics[0].Tactic = "no_such_tactic"

	now := time.Date(2026, 9, 14, 14, 30, 0, 0, clock.Tokyo)
	if _, _, err := PlanOrders(cfg, store, newLedger(t), now, false, false, nil); err == nil {
		t.Fatal("未知の戦略でもエラーにならない")
	}
}

// 台帳が読めなければ計画を立てない（A3）。以前は発注済み額の読み失敗を 0 と読み、
// 同じ月の予算をもう一度買う計画を立てていた。
func TestPlanOrdersFailsWhenLedgerUnreadable(t *testing.T) {
	store := data.NewBarStore(t.TempDir())
	writeBars(t, store, "1306.T", "2026-08-25", "2026-09-13", 1000)
	cfg := planConfig(200_000, window.Unrestricted())
	led := newLedger(t)
	if err := led.Close(); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 14, 14, 30, 0, 0, clock.Tokyo)
	orders, _, err := PlanOrders(cfg, store, led, now, false, false, nil)
	if err == nil {
		t.Fatalf("台帳が読めないのに計画を立てた: %+v", orders)
	}
}

// 判定用の銘柄の足が古いときは警告に留め、買う銘柄の注文は作る。
func TestPlanOrdersWarnsStaleSignalButStillPlans(t *testing.T) {
	store := data.NewBarStore(t.TempDir())
	writeBars(t, store, "1306.T", "2026-08-25", "2026-09-13", 1000)
	writeBars(t, store, "1321.T", "2026-08-25", "2026-09-01", 1000)
	cfg := planConfig(200_000, window.Unrestricted())
	cfg.Execution.MaxStaleDays = 6
	cfg.Tactics[0].SignalSymbol = "1321.T"

	now := time.Date(2026, 9, 14, 14, 30, 0, 0, clock.Tokyo)
	orders, stale, err := PlanOrders(cfg, store, newLedger(t), now, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || !strings.Contains(stale[0], "1321.T") {
		t.Errorf("古い判定用の足の警告 = %v", stale)
	}
	if len(orders) != 1 || orders[0].Request == nil {
		t.Fatalf("判定用の足が古くても買う銘柄の注文は作る: %+v", orders)
	}
}

// --- RunAccumulation ---------------------------------------------------

// runBroker は RunAccumulation が使うメソッドだけを持つブローカー。
type runBroker struct {
	broker.Broker
	orders      map[string]*domain.Order // GetOrder で引ける注文
	history     []domain.Order           // 当月の注文履歴
	buyingPower decimal.Decimal
	cost        decimal.Decimal // 見積りの金額
	balanceErr  error
	placeErr    error
	onPlace     func()                     // 受理を返す直前に呼ぶ（台帳を壊すなど）
	lots        map[string]decimal.Decimal // 銘柄情報の売買単位

	balances int
	previews int
	placed   []domain.OrderRequest
}

func (r *runBroker) GetOrder(clientOrderID string, _ *string) (*domain.Order, error) {
	return r.orders[clientOrderID], nil
}

func (r *runBroker) GetOrderHistory(time.Time, time.Time) ([]domain.Order, error) {
	return r.history, nil
}

// LotSizes は銘柄情報の売買単位（lots に無い銘柄は返さない）。
func (r *runBroker) LotSizes(symbols []string) map[string]decimal.Decimal {
	out := map[string]decimal.Decimal{}
	for _, s := range symbols {
		if lot, ok := r.lots[s]; ok {
			out[s] = lot
		}
	}
	return out
}

func (r *runBroker) GetBalance() (*domain.Balance, error) {
	r.balances++
	if r.balanceErr != nil {
		return nil, r.balanceErr
	}
	return &domain.Balance{BuyingPower: r.buyingPower}, nil
}

func (r *runBroker) Preview(domain.OrderRequest) (*domain.OrderPreview, error) {
	r.previews++
	return &domain.OrderPreview{EstimatedCost: r.cost}, nil
}

func (r *runBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	r.placed = append(r.placed, req)
	if r.placeErr != nil {
		return nil, r.placeErr
	}
	if r.orders == nil {
		r.orders = map[string]*domain.Order{}
	}
	r.orders[req.ClientOrderID] = &domain.Order{
		ClientOrderID: req.ClientOrderID, Symbol: req.Symbol, Side: req.Side,
		Quantity: req.Quantity, FilledQuantity: decimal.Zero, Status: domain.OrderStatusSubmitted,
	}
	if r.onPlace != nil {
		r.onPlace()
	}
	return &domain.OrderAck{ClientOrderID: req.ClientOrderID, Status: domain.OrderStatusSubmitted}, nil
}

func TestRunAccumulation(t *testing.T) {
	// RunAccumulation は実時計で動くので、足は「今日」を基準に置く。
	jst := clock.ToZone(clock.NowUTC(), clock.Tokyo)
	if jst.Day() == 1 {
		t.Skip("月の初日は今月の確定足が無く、発注の経路を通らない")
	}
	today := jst.Format("2006-01-02")
	monthStart := time.Date(jst.Year(), jst.Month(), 1, 0, 0, 0, 0, time.UTC)
	yesterday := time.Date(jst.Year(), jst.Month(), jst.Day()-1, 0, 0, 0, 0, time.UTC)
	thisMonth := monthStart.Format("2006-01-02")
	// 予算 200000・終値 1000 → 指値 1010 で 100 株
	orderID := domain.MakeClientOrderID(today, "1306", domain.SideBuy, dec(100))
	closedWindow := window.TradingWindow{Start: window.DefaultStart, End: window.DefaultStart, Enabled: true}

	type env struct {
		b   *runBroker
		led *ledger.Ledger
		run func() error
	}

	cases := []struct {
		name            string
		live            bool
		window          *window.TradingWindow
		setup           func(t *testing.T, e env)
		closeLedger     bool
		wantErr         string // 含むべき文字列。空ならエラーなし
		wantUnconfirmed bool
		wantBalances    int
		wantPreviews    int
		wantPlaced      int
		then            func(t *testing.T, e env)
	}{
		{
			name: "通常の日は 1 件出して台帳に残し、次の run では出さない", live: true,
			wantBalances: 1, wantPreviews: 1, wantPlaced: 1,
			then: func(t *testing.T, e env) {
				if !wasPlaced(t, e.led, orderID) {
					t.Error("発注済みとして台帳に残っていない")
				}
				if err := e.run(); err != nil {
					t.Fatal(err)
				}
				if len(e.b.placed) != 1 {
					t.Errorf("2 回目の run で再発注した: %d 件", len(e.b.placed))
				}
			},
		},
		{
			name: "dry-run は送らず dry_run で記録する", live: false,
			wantBalances: 1, wantPreviews: 1, wantPlaced: 0,
			then: func(t *testing.T, e env) {
				recent, err := e.led.Recent(1)
				if err != nil || len(recent) != 1 || recent[0].Status != ledger.DryRunStatus {
					t.Errorf("dry_run の記録 = %+v (err: %v)", recent, err)
				}
				if wasPlaced(t, e.led, orderID) {
					t.Error("dry-run を発注済みに数えると本番で出せなくなる")
				}
			},
		},
		{
			name: "今月分を発注済みなら何も出さない", live: true,
			setup: func(t *testing.T, e env) {
				recordOrder(t, e.led, "済み", string(domain.OrderStatusFilled), &thisMonth, 200_000)
			},
			wantBalances: 1, wantPreviews: 0, wantPlaced: 0,
		},
		{
			name: "拒否は REJECTED にして次の run で出し直す", live: true,
			setup: func(t *testing.T, e env) {
				e.b.placeErr = &broker.OrderRejectedError{Message: "値幅制限"}
			},
			// 拒否は失敗として返す（通知・非 0 終了。A6）
			wantErr:      "発注拒否",
			wantBalances: 1, wantPreviews: 1, wantPlaced: 1,
			then: func(t *testing.T, e env) {
				if wasPlaced(t, e.led, orderID) {
					t.Error("拒否された注文が再送を妨げている")
				}
				e.b.placeErr = nil
				if err := e.run(); err != nil {
					t.Fatal(err)
				}
				if len(e.b.placed) != 2 {
					t.Errorf("拒否の後に出し直していない: %d 件", len(e.b.placed))
				}
			},
		},
		{
			name: "送信結果不明は PENDING のまま止め、次の run でも再送しない", live: true,
			setup: func(t *testing.T, e env) {
				e.b.placeErr = &broker.BrokerError{Message: "タイムアウト"}
			},
			wantUnconfirmed: true,
			wantBalances:    1, wantPreviews: 1, wantPlaced: 1,
			then: func(t *testing.T, e env) {
				recent, err := e.led.Recent(1)
				if err != nil || len(recent) != 1 || recent[0].Status != string(domain.OrderStatusPending) {
					t.Errorf("PENDING のまま残っていない: %+v (err: %v)", recent, err)
				}
				e.b.placeErr = nil
				if err := e.run(); err != nil {
					t.Fatal(err)
				}
				if len(e.b.placed) != 1 {
					t.Errorf("届いたか分からない注文を再送した（二重発注）: %d 件", len(e.b.placed))
				}
			},
		},
		{
			// 計画月の無い行は発注済み額に数わらないので差額は立つ。それでも同じ ID は WasPlaced が弾く
			name: "同じ client_order_id が台帳にあれば WasPlaced で弾く", live: true,
			setup: func(t *testing.T, e env) {
				recordOrder(t, e.led, orderID, string(domain.OrderStatusSubmitted), nil, 101_000)
				e.b.orders = map[string]*domain.Order{orderID: {
					ClientOrderID: orderID, Symbol: "1306", Side: domain.SideBuy,
					Quantity: dec(100), FilledQuantity: decimal.Zero, Status: domain.OrderStatusSubmitted,
				}}
			},
			wantBalances: 1, wantPreviews: 0, wantPlaced: 0,
		},
		{
			// 以前は計画が発注済み額を 0 と読んで進み、余力の照会まで行っていた（A3）
			name: "台帳が読めなければ発注に進まない", live: false,
			closeLedger:  true,
			wantErr:      "台帳を読めない",
			wantBalances: 0, wantPreviews: 0, wantPlaced: 0,
		},
		{
			name: "買付余力が足りなければ送らず、失敗として返す", live: true,
			setup: func(t *testing.T, e env) {
				e.b.buyingPower = dec(1000)
			},
			wantErr:      "買付余力不足",
			wantBalances: 1, wantPreviews: 1, wantPlaced: 0,
		},
		{
			name: "買付余力を照会できなければ発注せず、失敗として返す", live: true,
			setup: func(t *testing.T, e env) {
				e.b.balanceErr = errors.New("接続できません")
			},
			wantErr:      "買付余力を照会できない",
			wantBalances: 1, wantPreviews: 0, wantPlaced: 0,
		},
		{
			// 届いたかどうか分からない注文が残る銘柄には出さない（A1）。
			// 送った直後（猶予の内側）の PENDING でも同じ——当日の一覧で決まるまで待つ
			name: "送信結果不明の注文が残る銘柄は発注しない", live: true,
			setup: func(t *testing.T, e env) {
				// 額 0 の行にして、差額（200000）が今日の注文として立つようにする
				recordOrder(t, e.led, "不明", string(domain.OrderStatusPending), &thisMonth, 0)
			},
			wantErr:      "送信結果不明の注文 不明",
			wantBalances: 1, wantPreviews: 0, wantPlaced: 0,
		},
		{
			// 受理されたのに台帳を書けなければ、次の銘柄へ進まず止める（A2）
			name: "受理後に台帳を書けなければ止める", live: true,
			setup: func(t *testing.T, e env) {
				e.b.onPlace = func() { _ = e.led.Close() }
			},
			wantErr:      "台帳を更新できません",
			wantBalances: 1, wantPreviews: 1, wantPlaced: 1,
			then: func(t *testing.T, e env) {
				led, err := ledger.OpenLedger(e.led.Path())
				if err != nil {
					t.Fatal(err)
				}
				defer led.Close()
				recent, err := led.Recent(1)
				if err != nil || len(recent) != 1 || recent[0].Status != string(domain.OrderStatusPending) {
					t.Errorf("送った事実が PENDING で残っていない（次の run の照合に回らない）: %+v (err: %v)", recent, err)
				}
			},
		},
		{
			name: "台帳に無い当月の約定があれば止める", live: true,
			setup: func(t *testing.T, e env) {
				price := dec(1000)
				e.b.history = []domain.Order{{
					ClientOrderID: "手で出した注文", Symbol: "1306", Side: domain.SideBuy,
					Quantity: dec(100), FilledQuantity: dec(100), AvgFillPrice: &price,
					Status: domain.OrderStatusFilled,
				}}
			},
			wantErr:      "台帳に無い当月の約定",
			wantBalances: 0, wantPreviews: 0, wantPlaced: 0,
		},
		{
			name: "発注時間帯の外なら何もしない", live: true, window: &closedWindow,
			wantBalances: 0, wantPreviews: 0, wantPlaced: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := data.NewBarStore(t.TempDir())
			writeBars(t, store, "1306.T", monthStart.AddDate(0, 0, -3).Format("2006-01-02"),
				yesterday.Format("2006-01-02"), 1000)
			w := window.Unrestricted()
			if tc.window != nil {
				w = *tc.window
			}
			cfg := planConfig(200_000, w)
			led := newLedger(t)
			// 前年から積み立てている銘柄（開始月の日割りを掛けない）
			if err := led.MarkStarted("1306", "2025-01-01"); err != nil {
				t.Fatal(err)
			}
			stubAlerts(t)
			b := &runBroker{buyingPower: dec(1_000_000), cost: dec(101_000)}
			e := env{b: b, led: led, run: func() error {
				return RunAccumulation(cfg, b, store, led, &logging.Logger{}, nil, tc.live, false)
			}}
			if tc.setup != nil {
				tc.setup(t, e)
			}
			if tc.closeLedger {
				if err := led.Close(); err != nil {
					t.Fatal(err)
				}
			}

			err := e.run()
			switch {
			case tc.wantUnconfirmed:
				var unconfirmed *ErrUnconfirmedOrder
				if !errors.As(err, &unconfirmed) {
					t.Fatalf("ErrUnconfirmedOrder を返すべき: %v", err)
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("エラー = %v, want に %q を含む", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("エラーにならないはず: %v", err)
			}
			if b.balances != tc.wantBalances {
				t.Errorf("余力の照会 = %d 回, want %d", b.balances, tc.wantBalances)
			}
			if b.previews != tc.wantPreviews {
				t.Errorf("見積り = %d 回, want %d", b.previews, tc.wantPreviews)
			}
			if len(b.placed) != tc.wantPlaced {
				t.Fatalf("発注 = %d 件, want %d", len(b.placed), tc.wantPlaced)
			}
			if tc.wantPlaced > 0 && b.placed[0].ClientOrderID != orderID {
				t.Errorf("client_order_id = %s, want %s", b.placed[0].ClientOrderID, orderID)
			}
			if tc.then != nil {
				tc.then(t, e)
			}
		})
	}
}

// stubAlerts は運用通知を控えるだけにする（Discord に送らず、state/notify にも書かない）。
func stubAlerts(t *testing.T) *[]string {
	t.Helper()
	var got []string
	saved := alert
	alert = func(title, body string, _ *logging.Logger) bool {
		got = append(got, title+"\n"+body)
		return true
	}
	t.Cleanup(func() { alert = saved })
	return &got
}
