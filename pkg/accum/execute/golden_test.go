package execute

// PlanOrders・RunAccumulation・VerifyStop の特性テスト（入力→出力の golden）。
//
// 分割のリファクタリングで振る舞いが変わらないことを確かめるためのもので、**今の挙動をそのまま
// 固定する**。値が正しいかどうかは見ていない（それは run_test.go などの役目）。条件ごとに
// 戻り値・ログ・通知・ブローカーへの呼び出し・台帳の中身を書き出し、testdata/golden/*.golden と
// 突き合わせる。時刻は clock.Now を差し替えて 2026-09-14（月）14:30 JST に固定する。
//
// 期待値を作り直すとき: go test ./pkg/accum/execute -run 'Golden' -update-accum-golden

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	wbhistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
)

var updateAccumGolden = flag.Bool("update-accum-golden", false, "accum の特性テストの期待値（testdata/golden）を書き直す")

// goldenNow は固定する時刻（2026-09-14 月曜 14:30 JST）。
var goldenNow = time.Date(2026, 9, 14, 14, 30, 0, 0, clock.Tokyo)

// fixClock は clock.Now を *at に向ける（途中で動かせるようにポインタで持つ）。
func fixClock(t *testing.T, at *time.Time) {
	t.Helper()
	saved := clock.Now
	clock.Now = func() time.Time { return *at }
	t.Cleanup(func() { clock.Now = saved })
}

// tempPath は t.TempDir() の場所（実行ごとに変わる）。期待値では <tmp>/ に置き換える。
var tempPath = regexp.MustCompile(regexp.QuoteMeta(os.TempDir()) + `/[^/\s]+/\d+/`)

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	got = tempPath.ReplaceAllString(got, "<tmp>/")
	path := filepath.Join("testdata", "golden", name+".golden")
	if *updateAccumGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("期待値がありません（-update-accum-golden で作る）: %v", err)
	}
	if got != string(want) {
		t.Errorf("出力が期待値と違います\n--- got\n%s--- want\n%s", got, want)
	}
}

func decText(d *decimal.Decimal) string {
	if d == nil {
		return "nil"
	}
	return d.String()
}

func strText(s *string) string {
	if s == nil {
		return "nil"
	}
	return *s
}

func reqText(r *domain.OrderRequest) string {
	if r == nil {
		return "nil"
	}
	stop := "nil"
	if r.Stop != nil {
		stop = fmt.Sprintf("{Trigger:%s Price:%s}", r.Stop.Trigger, decText(r.Stop.Price))
	}
	return fmt.Sprintf("{ID:%s Sym:%s Side:%s Type:%s Qty:%s Limit:%s Tax:%s Trade:%s Cond:%s Stop:%s Reason:%s}",
		r.ClientOrderID, r.Symbol, r.Side, r.OrderType, r.Quantity, decText(r.LimitPrice),
		r.TaxType, r.Trade, r.Condition, stop, r.Reason)
}

func plannedText(po PlannedOrder) string {
	return fmt.Sprintf("Symbol=%s Amount=%s Quantity=%s Limit=%s Failed=%v LotUnknown=%v Market=%s JudgedOn=%s Month=%s "+
		"Close=%s Target=%s Placed=%s Multiplier=%v Tactic=%s\n    Reason=%s\n    Note=%s\n    Request=%s\n",
		po.Symbol, po.Amount, po.Quantity, decText(po.LimitPrice), po.Failed, po.LotUnknown, po.Market,
		po.JudgedOn, po.Month, po.Close, po.Target, po.Placed, po.Multiplier, po.Tactic,
		po.Reason, po.Note, reqText(po.Request))
}

// logText はロガーの端末出力から時刻を落とす（行頭の時刻の後の " [" から残す）。
func logText(buf *bytes.Buffer) string {
	var sb strings.Builder
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		if i := strings.Index(line, " ["); i >= 0 {
			line = line[i+1:]
		}
		sb.WriteString("  " + line + "\n")
	}
	return sb.String()
}

func ledgerText(t *testing.T, led *ledger.Ledger) string {
	t.Helper()
	rows, err := led.Recent(1000)
	if err != nil {
		return "  (台帳を読めない: " + err.Error() + ")\n"
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ClientOrderID < rows[j].ClientOrderID })
	var sb strings.Builder
	for _, r := range rows {
		mkt := "nil"
		if r.Market != nil {
			mkt = string(*r.Market)
		}
		fmt.Fprintf(&sb, "  %s sym=%s mkt=%s qty=%s filled=%s status=%s amount=%s month=%s placed=%s updated=%s broker=%s avg=%s\n",
			r.ClientOrderID, r.Symbol, mkt, r.Quantity, r.FilledQuantity, r.Status, decText(r.Amount),
			strText(r.PlanMonth), r.PlacedAt, strText(r.UpdatedAt), strText(r.BrokerOrderID), decText(r.AvgFillPrice))
	}
	return sb.String()
}

// writeWave は from〜to の毎日に、波打つ終値の足を書く（倍率が動くように）。
func writeWave(t *testing.T, store *data.BarStore, symbol, from, to string, base float64) {
	t.Helper()
	start, _ := time.Parse("2006-01-02", from)
	end, _ := time.Parse("2006-01-02", to)
	var bars []domain.Bar
	i := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		// 280 日周期で ±25%、ほかに細かい揺れ
		phase := float64(i%280) / 280
		var level float64
		if i%280 < 140 {
			level = 1.0 - 0.5*phase
		} else {
			level = 0.5 + 0.5*phase
		}
		c := decimal.NewFromFloat(base * level * (1 + 0.01*float64(i%5-2))).Round(1)
		bar, err := domain.NewBar(symbol, d.Format("2006-01-02"), c, c, c, c, dec(1000))
		if err != nil {
			t.Fatal(err)
		}
		bars = append(bars, bar)
		i++
	}
	if err := store.Write(symbol, bars); err != nil {
		t.Fatal(err)
	}
}

// writeBroken は読めない足のファイル（権限 000）を置く。中身の壊れた parquet は
// ライブラリが panic するので、開けない形で読み出しの失敗を作る。
func writeBroken(t *testing.T, store *data.BarStore, symbol string) {
	t.Helper()
	path, err := store.PathFor(symbol)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(path); err == nil {
		_ = f.Close()
		t.Skip("権限 000 のファイルを開けてしまう（root で動いている）")
	}
}

// --- PlanOrders --------------------------------------------------------

type planEnv struct {
	cfg          *accumcfg.AccumConfig
	store        *data.BarStore
	led          *ledger.Ledger
	now          time.Time
	ignoreWindow bool
	markStart    bool
	lots         func() map[string]decimal.Decimal
	lotCalls     int
}

func TestPlanOrdersGolden(t *testing.T) {
	now := goldenNow
	fixClock(t, &now)

	cases := []struct {
		name  string
		setup func(t *testing.T, e *planEnv)
	}{
		{"normal", func(t *testing.T, e *planEnv) {}},
		{"disabled_tactic", func(t *testing.T, e *planEnv) { e.cfg.Tactics[0].Enabled = boolPtr(false) }},
		{"unknown_tactic", func(t *testing.T, e *planEnv) { e.cfg.Tactics[0].Tactic = "magic" }},
		{"signal_missing", func(t *testing.T, e *planEnv) { e.cfg.Tactics[0].SignalSymbol = "^NONE" }},
		{"signal_broken", func(t *testing.T, e *planEnv) {
			writeBroken(t, e.store, "^BRK")
			e.cfg.Tactics[0].SignalSymbol = "^BRK"
		}},
		{"signal_only_today", func(t *testing.T, e *planEnv) {
			writeBars(t, e.store, "^TDY", "2026-09-14", "2026-09-15", 500)
			e.cfg.Tactics[0].SignalSymbol = "^TDY"
		}},
		{"signal_stale", func(t *testing.T, e *planEnv) {
			writeWave(t, e.store, "^OLD", "2025-06-01", "2026-09-01", 20000)
			e.cfg.Tactics[0].SignalSymbol = "^OLD"
			e.cfg.Execution.MaxStaleDays = 5
		}},
		{"bars_missing", func(t *testing.T, e *planEnv) { e.cfg.Tactics[0].Symbols = []string{"9999.T"} }},
		{"bars_broken", func(t *testing.T, e *planEnv) {
			writeBroken(t, e.store, "8888.T")
			e.cfg.Tactics[0].Symbols = []string{"8888.T"}
		}},
		{"bars_only_today", func(t *testing.T, e *planEnv) {
			writeBars(t, e.store, "7777.T", "2026-09-14", "2026-09-15", 1000)
			e.cfg.Tactics[0].Symbols = []string{"7777.T"}
		}},
		{"bars_stale", func(t *testing.T, e *planEnv) {
			writeBars(t, e.store, "1306.T", "2026-08-01", "2026-09-05", 1000)
			e.cfg.Execution.MaxStaleDays = 3
		}},
		{"no_rows_this_month", func(t *testing.T, e *planEnv) {
			writeBars(t, e.store, "1306.T", "2026-07-01", "2026-08-31", 1000)
		}},
		{"outside_window", func(t *testing.T, e *planEnv) {
			e.cfg.Tactics[0].Window = window.TradingWindow{Start: window.DefaultStart, End: window.DefaultStart, Enabled: true}
		}},
		{"outside_window_ignored", func(t *testing.T, e *planEnv) {
			e.cfg.Tactics[0].Window = window.TradingWindow{Start: window.DefaultStart, End: window.DefaultStart, Enabled: true}
			e.ignoreWindow = true
		}},
		{"ledger_closed", func(t *testing.T, e *planEnv) { _ = e.led.Close() }},
		{"unmarked_dry", func(t *testing.T, e *planEnv) { e.led = newLedger(t) }},
		{"unmarked_mark", func(t *testing.T, e *planEnv) {
			e.led = newLedger(t)
			e.markStart = true
		}},
		{"unmarked_first_order", func(t *testing.T, e *planEnv) {
			e.led = newLedger(t)
			e.markStart = true
			aug := "2026-08-01"
			recordOrderFor(t, e.led, "8月", "1306", &aug, 150_000)
		}},
		{"carry_over", func(t *testing.T, e *planEnv) {
			aug := "2026-08-01"
			recordOrderFor(t, e.led, "8月", "1306", &aug, 120_000)
		}},
		{"already_placed", func(t *testing.T, e *planEnv) {
			sep := "2026-09-01"
			recordOrderFor(t, e.led, "9月", "1306", &sep, 200_000)
		}},
		{"partial_placed_hold", func(t *testing.T, e *planEnv) {
			sep := "2026-09-01"
			recordOrderFor(t, e.led, "9月", "1306", &sep, 150_000)
		}},
		{"lot_unknown", func(t *testing.T, e *planEnv) { e.cfg.Execution.LotSizeOverrides = nil }},
		{"lot_from_broker", func(t *testing.T, e *planEnv) {
			e.cfg.Execution.LotSizeOverrides = nil
			e.lots = func() map[string]decimal.Decimal { return map[string]decimal.Decimal{"1306": dec(10)} }
		}},
		{"lot_mismatch", func(t *testing.T, e *planEnv) {
			e.lots = func() map[string]decimal.Decimal { return map[string]decimal.Decimal{"1306": dec(10)} }
		}},
		{"limit_offset_custom", func(t *testing.T, e *planEnv) { e.cfg.Execution.LimitOffset = "0.037" }},
		{"limit_offset_bad", func(t *testing.T, e *planEnv) { e.cfg.Execution.LimitOffset = "abc" }},
		{"below_lot", func(t *testing.T, e *planEnv) { e.cfg.Tactics[0].MonthlyBudget = dec(50_000) }},
		{"market_order_tax", func(t *testing.T, e *planEnv) {
			e.cfg.Execution.OrderType = "market"
			e.cfg.Execution.TaxAccountType = "NISA"
		}},
		{"multi_tactics", func(t *testing.T, e *planEnv) {
			writeWave(t, e.store, "^IXIC", "2025-01-01", "2026-09-13", 18000)
			writeWave(t, e.store, "563A.T", "2025-06-01", "2026-09-13", 1500)
			writeWave(t, e.store, "2559.T", "2024-06-01", "2026-09-13", 20000)
			if err := e.led.MarkStarted("563A", "2026-09-08"); err != nil {
				t.Fatal(err)
			}
			if err := e.led.MarkStarted("2559", "2025-01-01"); err != nil {
				t.Fatal(err)
			}
			e.cfg.Execution.LotSizeOverrides["563A.T"] = 1
			e.cfg.Execution.MaxStaleDays = 7
			e.lots = func() map[string]decimal.Decimal { return map[string]decimal.Decimal{"2559": dec(1)} }
			e.cfg.Tactics = append(e.cfg.Tactics,
				accumcfg.TacticEntry{ID: "B", Tactic: "bear_stack", Symbols: []string{"563A.T"}, SignalSymbol: "^IXIC",
					SignalMarket: "US", Multiplier: 4, MonthlyBudget: dec(15_000), Window: window.Unrestricted()},
				accumcfg.TacticEntry{ID: "C", Tactic: "bear_stack", Symbols: []string{"2559.T", "9999.T"},
					Multiplier: 4, MonthlyBudget: dec(25_000), Window: window.Unrestricted()},
			)
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := data.NewBarStore(t.TempDir())
			writeBars(t, store, "1306.T", "2026-08-25", "2026-09-13", 1000)
			led := newLedger(t)
			if err := led.MarkStarted("1306", "2025-01-01"); err != nil {
				t.Fatal(err)
			}
			e := &planEnv{cfg: planConfig(200_000, window.Unrestricted()), store: store, led: led, now: goldenNow}
			c.setup(t, e)
			var lots func() map[string]decimal.Decimal
			if e.lots != nil {
				lots = func() map[string]decimal.Decimal { e.lotCalls++; return e.lots() }
			}
			orders, stale, err := PlanOrders(e.cfg, e.store, e.led, e.now, e.ignoreWindow, e.markStart, lots)

			var sb strings.Builder
			fmt.Fprintf(&sb, "err: %v\n", err)
			fmt.Fprintf(&sb, "lotCalls: %d\n", e.lotCalls)
			fmt.Fprintf(&sb, "stale: %q\n", stale)
			for i, po := range orders {
				fmt.Fprintf(&sb, "[%d] %s", i, plannedText(po))
			}
			if err == nil || c.name != "ledger_closed" {
				for _, sym := range []string{"1306", "563A", "2559"} {
					on, serr := e.led.StartedOn(sym)
					fmt.Fprintf(&sb, "started %s: %s (%v)\n", sym, strText(on), serr)
				}
			}
			checkGolden(t, "plan_"+c.name, sb.String())
		})
	}
}

// --- RunAccumulation ---------------------------------------------------

// goldenBroker は RunAccumulation が使うメソッドを持ち、呼び出しを控えるブローカー。
type goldenBroker struct {
	broker.Broker
	orders      map[string]*domain.Order
	history     []domain.Order
	historyErr  error
	orderErr    error
	buyingPower decimal.Decimal
	cost        decimal.Decimal
	fee         decimal.Decimal
	balanceErr  error
	balanceNil  bool
	previewErr  error
	placeErrs   map[string]error // 銘柄ごとの発注エラー
	onPlace     func()
	lots        map[string]decimal.Decimal

	calls []string
}

func (g *goldenBroker) Name() string { return "golden" }

func (g *goldenBroker) GetOrder(clientOrderID string, _ *string) (*domain.Order, error) {
	g.calls = append(g.calls, "GetOrder "+clientOrderID)
	if g.orderErr != nil {
		return nil, g.orderErr
	}
	return g.orders[clientOrderID], nil
}

func (g *goldenBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	g.calls = append(g.calls, fmt.Sprintf("GetOrderHistory %s..%s", start.Format(time.RFC3339), end.Format(time.RFC3339)))
	return g.history, g.historyErr
}

func (g *goldenBroker) LotSizes(symbols []string) map[string]decimal.Decimal {
	g.calls = append(g.calls, fmt.Sprintf("LotSizes %v", symbols))
	out := map[string]decimal.Decimal{}
	for _, s := range symbols {
		if lot, ok := g.lots[s]; ok {
			out[s] = lot
		}
	}
	return out
}

func (g *goldenBroker) GetBalance() (*domain.Balance, error) {
	g.calls = append(g.calls, "GetBalance")
	if g.balanceErr != nil {
		return nil, g.balanceErr
	}
	if g.balanceNil {
		return nil, nil
	}
	return &domain.Balance{BuyingPower: g.buyingPower}, nil
}

func (g *goldenBroker) Preview(req domain.OrderRequest) (*domain.OrderPreview, error) {
	g.calls = append(g.calls, "Preview "+req.Symbol)
	if g.previewErr != nil {
		return nil, g.previewErr
	}
	return &domain.OrderPreview{EstimatedCost: g.cost, EstimatedFee: g.fee}, nil
}

func (g *goldenBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	g.calls = append(g.calls, "Place "+reqText(&req))
	if err := g.placeErrs[req.Symbol]; err != nil {
		if g.onPlace != nil {
			g.onPlace()
		}
		return nil, err
	}
	if g.orders == nil {
		g.orders = map[string]*domain.Order{}
	}
	id := "B-" + req.Symbol
	g.orders[req.ClientOrderID] = &domain.Order{
		ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Symbol: req.Symbol, Side: req.Side,
		Quantity: req.Quantity, FilledQuantity: decimal.Zero, Status: domain.OrderStatusSubmitted,
	}
	if g.onPlace != nil {
		g.onPlace()
	}
	return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
}

type runEnv struct {
	cfg          *accumcfg.AccumConfig
	store        *data.BarStore
	led          *ledger.Ledger
	b            *goldenBroker
	hist         *wbhistory.Store
	histRoot     string
	live         bool
	ignoreWindow bool
	// at は setup 中の時刻（台帳の placed_at に入る）。run の前に goldenNow に戻す
	at *time.Time
}

func recordStatus(t *testing.T, led *ledger.Ledger, id, symbol, status string, month *string, amount int64, qty int64) domain.OrderRequest {
	t.Helper()
	req := newRequest(t, id, symbol, qty)
	amt := dec(amount)
	mkt := domain.MarketJP
	if err := led.Record(req, status, nil, month, &amt, &mkt); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRunAccumulationGolden(t *testing.T) {
	now := goldenNow
	fixClock(t, &now)
	sep := "2026-09-01"
	todayID := domain.MakeClientOrderID("2026-09-14", "1306", domain.SideBuy, dec(100))
	closed := window.TradingWindow{Start: window.DefaultStart, End: window.DefaultStart, Enabled: true}
	filled := func(sym string, qty int64, id string) domain.Order {
		avg := dec(1000)
		created := goldenNow.Add(-2 * time.Hour).UTC()
		return domain.Order{ClientOrderID: id, BrokerOrderID: &id, Symbol: sym, Side: domain.SideBuy,
			Trade: domain.TradeTypeCash, Quantity: dec(qty), FilledQuantity: dec(qty),
			Status: domain.OrderStatusFilled, AvgFillPrice: &avg, CreatedAt: &created}
	}

	cases := []struct {
		name  string
		setup func(t *testing.T, e *runEnv)
	}{
		{"live_normal", func(t *testing.T, e *runEnv) {}},
		{"dry_normal", func(t *testing.T, e *runEnv) { e.live = false }},
		{"live_outside_window", func(t *testing.T, e *runEnv) { e.cfg.Tactics[0].Window = closed }},
		{"live_outside_window_ignored", func(t *testing.T, e *runEnv) {
			e.cfg.Tactics[0].Window = closed
			e.ignoreWindow = true
		}},
		{"dry_outside_window", func(t *testing.T, e *runEnv) {
			e.live = false
			e.cfg.Tactics[0].Window = closed
		}},
		{"dry_lot_unknown", func(t *testing.T, e *runEnv) {
			e.live = false
			e.cfg.Execution.LotSizeOverrides = nil
		}},
		{"live_lot_unknown", func(t *testing.T, e *runEnv) { e.cfg.Execution.LotSizeOverrides = nil }},
		{"live_lot_from_broker", func(t *testing.T, e *runEnv) {
			e.cfg.Execution.LotSizeOverrides = nil
			e.b.lots = map[string]decimal.Decimal{"1306": dec(10)}
		}},
		{"ledger_closed", func(t *testing.T, e *runEnv) { _ = e.led.Close() }},
		{"stale_signal", func(t *testing.T, e *runEnv) {
			writeBars(t, e.store, "^OLD", "2026-08-01", "2026-09-01", 500)
			e.cfg.Tactics[0].SignalSymbol = "^OLD"
			e.cfg.Execution.MaxStaleDays = 5
		}},
		{"history_unwritable", func(t *testing.T, e *runEnv) {
			// 履歴の置き場をファイルにして書けなくする
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			e.hist = wbhistory.NewStore(path)
			e.histRoot = ""
		}},
		{"no_history", func(t *testing.T, e *runEnv) { e.hist = nil; e.histRoot = "" }},
		{"unrecorded_fill", func(t *testing.T, e *runEnv) {
			e.b.history = []domain.Order{filled("1306", 100, "999/20260914")}
		}},
		{"unrecorded_multi", func(t *testing.T, e *runEnv) {
			writeBars(t, e.store, "1305.T", "2026-08-25", "2026-09-13", 2000)
			e.cfg.Execution.LotSizeOverrides["1305.T"] = 10
			e.cfg.Tactics[0].Symbols = []string{"1306.T", "1305.T"}
			e.b.history = []domain.Order{filled("1306", 100, "999/20260914"), filled("1306", 50, "998/20260914"),
				filled("1305", 30, "997/20260914")}
		}},
		{"unrecorded_check_failed", func(t *testing.T, e *runEnv) {
			e.b.historyErr = errors.New("履歴の照会がタイムアウト")
		}},
		{"dry_skips_unrecorded_check", func(t *testing.T, e *runEnv) {
			e.live = false
			e.b.historyErr = errors.New("履歴の照会がタイムアウト")
		}},
		{"balance_failed", func(t *testing.T, e *runEnv) {
			e.b.balanceErr = errors.New("余力照会がタイムアウト")
			e.cfg.Tactics[0].Symbols = append(e.cfg.Tactics[0].Symbols, "9999.T")
		}},
		{"balance_nil", func(t *testing.T, e *runEnv) { e.b.balanceNil = true }},
		{"balance_failed_pending", func(t *testing.T, e *runEnv) {
			e.b.balanceErr = errors.New("余力照会がタイムアウト")
			recordStatus(t, e.led, "前日の不明", "1306", string(domain.OrderStatusPending), &sep, 0, 100)
			backdate(t, e.led, "前日の不明")
		}},
		{"pending_earlier_blocks", func(t *testing.T, e *runEnv) {
			recordStatus(t, e.led, "前日の不明", "1306", string(domain.OrderStatusPending), &sep, 0, 100)
			backdate(t, e.led, "前日の不明")
		}},
		{"pending_earlier_counted", func(t *testing.T, e *runEnv) {
			recordStatus(t, e.led, "前日の不明", "1306", string(domain.OrderStatusPending), &sep, 200_000, 100)
			backdate(t, e.led, "前日の不明")
		}},
		{"pending_today_attributed", func(t *testing.T, e *runEnv) {
			*e.at = goldenNow.Add(-3 * time.Hour)
			recordStatus(t, e.led, "今日の不明", "1306", string(domain.OrderStatusPending), &sep, 200_000, 100)
			e.b.history = []domain.Order{filled("1306", 100, "777/20260914")}
		}},
		{"pending_today_not_sent", func(t *testing.T, e *runEnv) {
			*e.at = goldenNow.Add(-3 * time.Hour)
			recordStatus(t, e.led, "今日の不明", "1306", string(domain.OrderStatusPending), &sep, 200_000, 100)
			e.b.history = []domain.Order{filled("2559", 1, "555/20260914")}
		}},
		{"pending_today_ambiguous", func(t *testing.T, e *runEnv) {
			*e.at = goldenNow.Add(-3 * time.Hour)
			recordStatus(t, e.led, "今日の不明", "1306", string(domain.OrderStatusPending), &sep, 200_000, 100)
			e.b.history = []domain.Order{filled("1306", 70, "777/20260914")}
		}},
		{"pending_today_too_recent", func(t *testing.T, e *runEnv) {
			*e.at = goldenNow.Add(-1 * time.Minute)
			recordStatus(t, e.led, "今日の不明", "1306", string(domain.OrderStatusPending), &sep, 0, 100)
		}},
		{"submitted_filled_since", func(t *testing.T, e *runEnv) {
			*e.at = goldenNow.Add(-24 * time.Hour)
			req := recordStatus(t, e.led, "前回", "1306", string(domain.OrderStatusSubmitted), &sep, 150_000, 100)
			avg := dec(990)
			e.b.orders = map[string]*domain.Order{req.ClientOrderID: {
				ClientOrderID: req.ClientOrderID, Symbol: "1306", Side: domain.SideBuy, Quantity: dec(100),
				FilledQuantity: dec(100), Status: domain.OrderStatusFilled, AvgFillPrice: &avg,
			}}
		}},
		{"submitted_query_failed", func(t *testing.T, e *runEnv) {
			*e.at = goldenNow.Add(-24 * time.Hour)
			recordStatus(t, e.led, "前回", "1306", string(domain.OrderStatusSubmitted), &sep, 200_000, 100)
			e.b.orderErr = errors.New("照会できない")
		}},
		{"already_placed_id", func(t *testing.T, e *runEnv) {
			recordStatus(t, e.led, todayID, "1306", string(domain.OrderStatusSubmitted), nil, 0, 100)
			e.b.orders = map[string]*domain.Order{todayID: {ClientOrderID: todayID, Symbol: "1306",
				Side: domain.SideBuy, Quantity: dec(100), Status: domain.OrderStatusSubmitted}}
		}},
		{"preview_failed", func(t *testing.T, e *runEnv) { e.b.previewErr = errors.New("見積りできない") }},
		{"buying_power_short", func(t *testing.T, e *runEnv) {
			e.b.buyingPower = dec(100_000)
			e.b.fee = dec(55)
		}},
		{"rejected", func(t *testing.T, e *runEnv) {
			e.b.placeErrs = map[string]error{"1306": &broker.OrderRejectedError{Message: "値幅制限"}}
		}},
		{"dry_rejected_not_sent", func(t *testing.T, e *runEnv) {
			e.live = false
			e.b.placeErrs = map[string]error{"1306": &broker.OrderRejectedError{Message: "値幅制限"}}
		}},
		{"unconfirmed", func(t *testing.T, e *runEnv) {
			e.b.placeErrs = map[string]error{"1306": errors.New("接続が切れた")}
		}},
		{"unconfirmed_after_failure", func(t *testing.T, e *runEnv) {
			e.cfg.Tactics[0].Symbols = []string{"0000.T", "1306.T"}
			e.b.placeErrs = map[string]error{"1306": errors.New("接続が切れた")}
		}},
		{"not_recorded", func(t *testing.T, e *runEnv) {
			e.b.onPlace = func() { _ = e.led.Close() }
		}},
		{"rejection_not_recorded", func(t *testing.T, e *runEnv) {
			e.b.placeErrs = map[string]error{"1306": &broker.OrderRejectedError{Message: "値幅制限"}}
			e.b.onPlace = func() { _ = e.led.Close() }
		}},
		{"two_symbols_budget", func(t *testing.T, e *runEnv) {
			writeBars(t, e.store, "1305.T", "2026-08-25", "2026-09-13", 2000)
			if err := e.led.MarkStarted("1305", "2025-01-01"); err != nil {
				t.Fatal(err)
			}
			e.cfg.Execution.LotSizeOverrides["1305.T"] = 10
			e.cfg.Tactics[0].Symbols = []string{"1305.T", "1306.T"}
			e.b.buyingPower = dec(150_000)
		}},
		{"pending_today_empty_list", func(t *testing.T, e *runEnv) {
			*e.at = goldenNow.Add(-3 * time.Hour)
			recordStatus(t, e.led, "今日の不明", "1306", string(domain.OrderStatusPending), &sep, 200_000, 100)
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now = goldenNow.Add(-10 * time.Minute)
			alerts := stubAlerts(t)
			store := data.NewBarStore(t.TempDir())
			writeBars(t, store, "1306.T", "2026-08-25", "2026-09-13", 1000)
			led := newLedger(t)
			if err := led.MarkStarted("1306", "2025-01-01"); err != nil {
				t.Fatal(err)
			}
			histRoot := t.TempDir()
			e := &runEnv{
				cfg: planConfig(200_000, window.Unrestricted()), store: store, led: led,
				b:    &goldenBroker{buyingPower: dec(1_000_000), cost: dec(101_000)},
				hist: wbhistory.NewStore(histRoot), histRoot: histRoot, live: true, at: &now,
			}
			c.setup(t, e)
			now = goldenNow

			var buf bytes.Buffer
			logger := &logging.Logger{}
			logger.SetOutput(&buf)
			err := RunAccumulation(e.cfg, e.b, e.store, e.led, logger, e.hist, e.live, e.ignoreWindow)

			var sb strings.Builder
			fmt.Fprintf(&sb, "err: %v\n", err)
			var failed *OrdersFailedError
			fmt.Fprintf(&sb, "OrdersFailedError: %v\n", errors.As(err, &failed))
			sb.WriteString("log:\n" + logText(&buf))
			sb.WriteString("alerts:\n")
			for _, a := range *alerts {
				sb.WriteString("  " + strings.ReplaceAll(a, "\n", "\n  ") + "\n")
			}
			sb.WriteString("calls:\n")
			for _, call := range e.b.calls {
				sb.WriteString("  " + call + "\n")
			}
			sb.WriteString("ledger:\n" + ledgerText(t, e.led))
			sb.WriteString("history:\n")
			if e.histRoot != "" {
				_ = filepath.Walk(e.histRoot, func(path string, info os.FileInfo, err error) error {
					if err == nil && !info.IsDir() {
						rel, _ := filepath.Rel(e.histRoot, path)
						fmt.Fprintf(&sb, "  %s\n", rel)
					}
					return nil
				})
			}
			checkGolden(t, "run_"+c.name, sb.String())
		})
	}
}

// --- VerifyStop --------------------------------------------------------

// stopBroker は逆指値の検証に要るメソッドを持ち、呼び出しを控えるブローカー。
type stopBroker struct {
	broker.Broker
	lot        decimal.Decimal
	noLot      bool
	last       decimal.Decimal
	quoteErr   error
	noQuote    bool
	held       decimal.Decimal
	posErr     error
	placeErr   error
	cancelErr  error
	correctErr error
	// getOrder は n 回目（0 始まり）の照会の応答
	getOrder func(n int, clientOrderID string) (*domain.Order, error)
	gets     int

	calls []string
}

func (s *stopBroker) Name() string { return "stopgolden" }

func (s *stopBroker) LotSizes(symbols []string) map[string]decimal.Decimal {
	s.calls = append(s.calls, fmt.Sprintf("LotSizes %v", symbols))
	if s.noLot {
		return map[string]decimal.Decimal{}
	}
	return map[string]decimal.Decimal{"563A": s.lot}
}

func (s *stopBroker) PositionsBySymbol() (map[string]domain.Position, error) {
	s.calls = append(s.calls, "PositionsBySymbol")
	if s.posErr != nil {
		return nil, s.posErr
	}
	if !s.held.IsPositive() {
		return map[string]domain.Position{}, nil
	}
	return map[string]domain.Position{"563A": {Symbol: "563A", Quantity: s.held}}, nil
}

func (s *stopBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	s.calls = append(s.calls, "Place "+reqText(&req))
	if s.placeErr != nil {
		return nil, s.placeErr
	}
	id := "B-1"
	return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
}

func (s *stopBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	s.calls = append(s.calls, fmt.Sprintf("GetOrder %s %s", clientOrderID, strText(brokerOrderID)))
	n := s.gets
	s.gets++
	return s.getOrder(n, clientOrderID)
}

func (s *stopBroker) Cancel(clientOrderID string, brokerOrderID *string) error {
	s.calls = append(s.calls, fmt.Sprintf("Cancel %s %s", clientOrderID, strText(brokerOrderID)))
	return s.cancelErr
}

// pricedStopBroker は時価を返せる（priceSource を満たす）。
type pricedStopBroker struct{ *stopBroker }

func (p pricedStopBroker) MarketPrices(symbols []string) (map[string]broker.MarketPrice, error) {
	p.calls = append(p.calls, fmt.Sprintf("MarketPrices %v", symbols))
	if p.quoteErr != nil {
		return nil, p.quoteErr
	}
	if p.noQuote {
		return map[string]broker.MarketPrice{}, nil
	}
	return map[string]broker.MarketPrice{"563A": {Symbol: "563A", Last: p.last}}, nil
}

// correctingStopBroker は CorrectStop も持つ。
type correctingStopBroker struct{ pricedStopBroker }

func (c correctingStopBroker) CorrectStop(clientOrderID string, brokerOrderID *string, stop domain.StopSpec) error {
	c.calls = append(c.calls, fmt.Sprintf("CorrectStop %s %s trigger=%s price=%s",
		clientOrderID, strText(brokerOrderID), stop.Trigger, decText(stop.Price)))
	return c.correctErr
}

func TestVerifyStopGolden(t *testing.T) {
	now := goldenNow
	fixClock(t, &now)

	stopOrder := func(status domain.OrderStatus, trigger int64, triggered bool) *domain.Order {
		tr := dec(trigger)
		return &domain.Order{Symbol: "563A", Status: status, Quantity: dec(1),
			Stop: &domain.StopSpec{Trigger: tr}, StopTriggered: triggered}
	}
	// seq は照会の応答を順に返す（尽きたら最後のものを返し続ける）
	seq := func(orders ...*domain.Order) func(int, string) (*domain.Order, error) {
		return func(n int, _ string) (*domain.Order, error) {
			if n >= len(orders) {
				n = len(orders) - 1
			}
			o := orders[n]
			if o == nil {
				return nil, errors.New("照会できない")
			}
			return o, nil
		}
	}
	avg := dec(1031)
	filledOrder := &domain.Order{Symbol: "563A", Status: domain.OrderStatusFilled, Quantity: dec(1),
		FilledQuantity: dec(1), AvgFillPrice: &avg, StopTriggered: true, Stop: &domain.StopSpec{Trigger: dec(1030)}}
	partialOrder := &domain.Order{Symbol: "563A", Status: domain.OrderStatusPartiallyFilled, Quantity: dec(1),
		FilledQuantity: dec(0), StopTriggered: true, Stop: &domain.StopSpec{Trigger: dec(1030)}}
	canceled := &domain.Order{Symbol: "563A", Status: domain.OrderStatusCancelled, Quantity: dec(1)}

	type kind int
	const (
		plain kind = iota
		priced
		correcting
	)
	cases := []struct {
		name string
		kind kind
		sb   stopBroker
		opts VerifyStopOptions
	}{
		// 送る前に止まる・dry-run
		{name: "no_symbol", kind: correcting, opts: VerifyStopOptions{}},
		{name: "no_lot", kind: correcting, sb: stopBroker{noLot: true}, opts: VerifyStopOptions{Symbol: "563A"}},
		{name: "no_price_source", kind: plain, sb: stopBroker{lot: dec(1)}, opts: VerifyStopOptions{Symbol: "563A"}},
		{name: "quote_error", kind: correcting, sb: stopBroker{lot: dec(1), quoteErr: errors.New("時価が取れない")},
			opts: VerifyStopOptions{Symbol: "563A"}},
		{name: "no_quote", kind: correcting, sb: stopBroker{lot: dec(1), noQuote: true}, opts: VerifyStopOptions{Symbol: "563A"}},
		{name: "positions_error", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), posErr: errors.New("建玉照会できない")},
			opts: VerifyStopOptions{Symbol: "563A"}},
		{name: "no_position", kind: correcting, sb: stopBroker{lot: dec(10), last: dec(1000), held: dec(0)},
			opts: VerifyStopOptions{Symbol: "563A", Units: 2}},
		{name: "partial_position", kind: correcting, sb: stopBroker{lot: dec(10), last: dec(1000), held: dec(15)},
			opts: VerifyStopOptions{Symbol: "563A", Units: 2}},
		{name: "dry_default", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(2777), held: dec(5)},
			opts: VerifyStopOptions{Symbol: "563A"}},
		{name: "dry_fire", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(2777), held: dec(5)},
			opts: VerifyStopOptions{Symbol: "563A", Fire: true, DropPct: decimal.RequireFromString("2.5")}},
		{name: "dry_also_limit", kind: correcting, sb: stopBroker{lot: dec(10), last: dec(31234), held: dec(30)},
			opts: VerifyStopOptions{Symbol: "563A", Units: 3, AlsoLimit: true, DropPct: dec(5)}},
		{name: "live_place_error", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			placeErr: errors.New("拒否")}, opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		// 送った後（2 秒ずつ待つので並列にする）
		{name: "live_ok", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(stopOrder(domain.OrderStatusSubmitted, 970, false), stopOrder(domain.OrderStatusSubmitted, 951, false),
				stopOrder(domain.OrderStatusSubmitted, 951, false), canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_query_error", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(nil)}, opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_stop_nil", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(&domain.Order{Status: domain.OrderStatusSubmitted}, &domain.Order{Status: domain.OrderStatusSubmitted}, canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_also_limit_ok", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(stopOrder(domain.OrderStatusSubmitted, 970, false), stopOrder(domain.OrderStatusSubmitted, 951, false),
				stopOrder(domain.OrderStatusSubmitted, 951, false), canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true, AlsoLimit: true}},
		{name: "live_no_corrector", kind: priced, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(stopOrder(domain.OrderStatusSubmitted, 970, false), stopOrder(domain.OrderStatusSubmitted, 970, false), canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_correct_error", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			correctErr: errors.New("訂正できない"),
			getOrder:   seq(stopOrder(domain.OrderStatusSubmitted, 970, false), stopOrder(domain.OrderStatusSubmitted, 970, false), canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_correct_query_error", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(stopOrder(domain.OrderStatusSubmitted, 970, false), nil, stopOrder(domain.OrderStatusSubmitted, 951, false), canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_correct_stop_nil", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(stopOrder(domain.OrderStatusSubmitted, 970, false), &domain.Order{Status: domain.OrderStatusSubmitted},
				stopOrder(domain.OrderStatusSubmitted, 951, false), canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_correct_not_reflected", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(stopOrder(domain.OrderStatusSubmitted, 970, false), stopOrder(domain.OrderStatusSubmitted, 970, false),
				stopOrder(domain.OrderStatusSubmitted, 970, false), canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_cancel_error", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			cancelErr: errors.New("取消できない"),
			getOrder:  seq(stopOrder(domain.OrderStatusSubmitted, 970, false), stopOrder(domain.OrderStatusSubmitted, 951, false))},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_cancel_query_error", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(stopOrder(domain.OrderStatusSubmitted, 970, false), stopOrder(domain.OrderStatusSubmitted, 951, false),
				stopOrder(domain.OrderStatusSubmitted, 951, false), nil)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true}},
		{name: "live_fire_rejected", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			correctErr: errors.New("発火後は訂正できません"),
			getOrder:   seq(filledOrder)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true, Fire: true}},
		{name: "live_fire_correct_passed", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(filledOrder)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true, Fire: true}},
		{name: "live_fire_not_triggered", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			correctErr: errors.New("訂正できない"),
			getOrder:   seq(stopOrder(domain.OrderStatusSubmitted, 1030, false), partialOrder, nil, canceled)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true, Fire: true}},
		{name: "live_fire_stop_nil", kind: correcting, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			correctErr: errors.New("訂正できない"),
			getOrder: seq(&domain.Order{Status: domain.OrderStatusFilled}, nil,
				&domain.Order{Status: domain.OrderStatusFilled, FilledQuantity: dec(1)})},
			opts: VerifyStopOptions{Symbol: "563A", Live: true, Fire: true}},
		{name: "live_fire_no_corrector", kind: priced, sb: stopBroker{lot: dec(1), last: dec(1000), held: dec(1),
			getOrder: seq(filledOrder)},
			opts: VerifyStopOptions{Symbol: "563A", Live: true, Fire: true}},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if c.opts.Live && c.sb.placeErr == nil && c.sb.getOrder != nil {
				t.Parallel()
			}
			sb := c.sb
			var b broker.Broker
			switch c.kind {
			case plain:
				b = &sb
			case priced:
				b = pricedStopBroker{&sb}
			case correcting:
				b = correctingStopBroker{pricedStopBroker{&sb}}
			}
			var buf bytes.Buffer
			logger := &logging.Logger{}
			logger.SetOutput(&buf)
			res, err := VerifyStop(b, logger, c.opts)

			var out strings.Builder
			fmt.Fprintf(&out, "err: %v\n", err)
			var noPos *ErrNoPosition
			fmt.Fprintf(&out, "ErrNoPosition: %v\n", errors.As(err, &noPos))
			if res == nil {
				out.WriteString("result: nil\n")
			} else {
				fmt.Fprintf(&out, "request: %s\n", reqText(&res.Request))
				fmt.Fprintf(&out, "trigger=%s corrected=%s limit=%s\n", res.Trigger, res.Corrected, res.Limit)
				for _, s := range res.Steps {
					fmt.Fprintf(&out, "step %s ok=%v %s\n", s.Name, s.OK, s.Detail)
				}
			}
			out.WriteString("log:\n" + logText(&buf))
			out.WriteString("calls:\n")
			for _, call := range sb.calls {
				out.WriteString("  " + call + "\n")
			}
			checkGolden(t, "stop_"+c.name, out.String())
		})
	}
}
