package main

// runDaily の流れのテスト（characterization test）。
//
// 設定（settings.toml・strategies.toml）・カレンダー・日足・台帳を一時ディレクトリに作り、
// 偽のブローカー（runBroker の差し替え）で runDaily を最後まで走らせる。ブローカーへの
// 呼び出しの順・出した注文・台帳（runs・orders・stops・risk_events・建玉の記録・目標・
// シグナル）・ログのイベント・ダイジェスト・通知・端末の出力を 1 本の文書に書き出し、
// testdata/run_flow/*.golden と突き合わせる。**今の挙動をそのまま固定する**もので、
// 正しさの判定ではない。runDaily を分けるときに、この文書が 1 文字も変わらないことを確かめる。
//
// 時計は 1 回読むごとに 1 ms 進む（runFlowClock）。台帳の時刻（秒まで）が clock.Now の
// 呼び回数を映す。戦略は判断を固定するための試験用（flow_fixed: buy / sell に書いた銘柄に
// +1 / -1 を出すだけ）。足の更新（sync）は外へ繋ぐので全ケース --no-sync で回す。
//
// 期待値を作り直すとき: go test ./cmd/wbjp -run TestRunFlow -update-run-flow

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
)

var updateRunFlow = flag.Bool("update-run-flow", false, "runDaily の流れのテストの期待値（testdata/run_flow）を書き直す")

func init() {
	// 判断を固定するための試験用の戦略。buy に書いた銘柄に +1、sell に書いた銘柄に -1 を出す
	strategy.Registry.MustRegister("flow_fixed", "流れのテスト用（判断を固定する）",
		func(raw map[string]any) (strategy.Strategy, error) {
			s := &flowStrategy{}
			for _, key := range []string{"buy", "sell"} {
				list, _ := raw[key].([]any)
				for _, v := range list {
					sym, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("flow_fixed.%s は文字列の配列: %v", key, v)
					}
					if key == "buy" {
						s.buy = append(s.buy, sym)
					} else {
						s.sell = append(s.sell, sym)
					}
				}
			}
			return s, nil
		})
}

type flowStrategy struct{ buy, sell []string }

func (s *flowStrategy) Name() string     { return "flow_fixed" }
func (s *flowStrategy) Describe() string { return "flow_fixed" }
func (s *flowStrategy) WarmupBars() int  { return 1 }
func (s *flowStrategy) OnBars(ctx *strategy.Context) ([]domain.Signal, error) {
	var out []domain.Signal
	for _, pair := range []struct {
		syms []string
		dir  float64
	}{{s.buy, 1}, {s.sell, -1}} {
		for _, sym := range pair.syms {
			if !ctx.HasBars(sym, 1) {
				continue
			}
			sig, err := domain.NewSignal("flow_fixed", sym, pair.dir, 1, "固定", nil)
			if err != nil {
				return nil, err
			}
			out = append(out, sig)
		}
	}
	return out, nil
}

// runFlowDay は判定日（木曜）。前日 2026-09-23 は祝日（秋分の日）で、前営業日は 09-22。
var runFlowDay = time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

func runFlowAt(day, hour, minute int) time.Time {
	return time.Date(2026, 9, day, hour, minute, 0, 0, clock.Tokyo)
}

// runFlowClock は読むたびに 1 ms 進む時計。
type runFlowClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *runFlowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	c.now = c.now.Add(time.Millisecond)
	return t
}

func (c *runFlowClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// runFlowBroker は日次実行の流れを見るための偽のブローカー。呼ばれた順を calls に残す。
type runFlowBroker struct {
	balance      domain.Balance
	positions    []domain.Position
	positionsErr error
	openOrders   []domain.Order
	openErr      error
	history      []domain.Order
	historyErr   error
	// orders は GetOrder の応答（client_order_id → 注文）。無ければ nil を返す
	orders    map[string]domain.Order
	orderErrs map[string]error
	// placeErr は Place の失敗（銘柄 → エラー）
	placeErr map[string]error

	calls  []string
	placed []domain.OrderRequest
}

func (s *runFlowBroker) call(format string, args ...any) {
	s.calls = append(s.calls, fmt.Sprintf(format, args...))
}

func (s *runFlowBroker) Name() string      { return "flow" }
func (s *runFlowBroker) AccountID() string { return "flow" }
func (s *runFlowBroker) GetBalance() (*domain.Balance, error) {
	s.call("GetBalance")
	b := s.balance
	return &b, nil
}
func (s *runFlowBroker) GetPositions() ([]domain.Position, error) {
	s.call("GetPositions")
	return s.positions, s.positionsErr
}
func (s *runFlowBroker) PositionsBySymbol() (map[string]domain.Position, error) {
	s.call("PositionsBySymbol")
	if s.positionsErr != nil {
		return nil, s.positionsErr
	}
	return broker.PositionsBySymbolHelper(s.positions), nil
}
func (s *runFlowBroker) GetOpenOrders() ([]domain.Order, error) {
	s.call("GetOpenOrders")
	return s.openOrders, s.openErr
}
func (s *runFlowBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	s.call("GetOrder %s %s", clientOrderID, stringOf(brokerOrderID))
	if err := s.orderErrs[clientOrderID]; err != nil {
		return nil, err
	}
	o, ok := s.orders[clientOrderID]
	if !ok {
		return nil, nil
	}
	return &o, nil
}
func (s *runFlowBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	s.call("GetOrderHistory %s %s", start.Format(time.RFC3339), end.Format(time.RFC3339))
	return s.history, s.historyErr
}
func (s *runFlowBroker) Preview(domain.OrderRequest) (*domain.OrderPreview, error) {
	s.call("Preview")
	return &domain.OrderPreview{}, nil
}
func (s *runFlowBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	s.call("Place %s %s %s", req.Symbol, req.Side, req.Quantity)
	s.placed = append(s.placed, req)
	if err := s.placeErr[req.Symbol]; err != nil {
		return nil, err
	}
	id := "B/" + req.Symbol
	return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
}
func (s *runFlowBroker) Cancel(clientOrderID string, _ *string) error {
	s.call("Cancel %s", clientOrderID)
	return nil
}
func (s *runFlowBroker) LotSizes([]string) map[string]decimal.Decimal {
	s.call("LotSizes")
	return map[string]decimal.Decimal{}
}

func stringOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// runFlowKnobs は設定の中でケースごとに変える項目。
type runFlowKnobs struct {
	buy, sell       []string
	maxOrdersPerDay int  // 0 なら 20
	killSwitch      bool // risk.kill_switch
	marketOrder     bool // execution.order_type = "market"
}

var runFlowUniverse = []string{"7203", "6758", "9984", "8306"}

func runFlowSettings(k runFlowKnobs) string {
	maxOrders := k.maxOrdersPerDay
	if maxOrders == 0 {
		maxOrders = 20
	}
	orderType := "limit"
	if k.marketOrder {
		orderType = "market"
	}
	return fmt.Sprintf(`[universe]
market = "JP"
symbols = %s
topix500_symbols = %s

[risk]
kill_switch = %v
max_order_value = "3000000"
max_orders_per_day = %d
max_daily_loss = "100000"
max_position_weight = "0.25"
max_gross_exposure = "0.90"
max_preview_deviation = "0.02"

[sizing]
method = "atr_risk"
risk_per_trade = "0.01"
atr_stop_multiple = "2.0"
fixed_notional = "300000"
max_positions = 5

[stops]
trailing = true
breakeven_after_r = "1.0"

[execution]
broker = "tachibana"
tax_account_type = "SPECIFIC"
order_type = %q
limit_offset = "0.005"
`, tomlList(runFlowUniverse), tomlList(runFlowUniverse), k.killSwitch, maxOrders, orderType)
}

func runFlowStrategies(k runFlowKnobs) string {
	return fmt.Sprintf(`combiner = "weighted_vote"
entry_threshold = 0.3
exit_threshold = 0.1

[[strategies]]
name = "flow_fixed"
weight = 1.0
buy = %s
sell = %s
`, tomlList(k.buy), tomlList(k.sell))
}

func tomlList(syms []string) string {
	q := make([]string, len(syms))
	for i, s := range syms {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// runFlowRun は 1 回の実行。
type runFlowRun struct {
	at                     time.Time
	live, acceptFlat, sync bool
	// prep は実行の前にブローカーの応答を変える（nil なら変えない）
	prep func(fb *runFlowBroker)
}

type runFlowCase struct {
	name     string
	knobs    runFlowKnobs
	env      settings.Environment
	calendar [][2]string // nil ならカレンダーを置かない
	// lastBar は銘柄ごとの最後の足の日付（無ければ 2026-09-22）
	lastBar map[string]string
	// seed は実行の前に台帳を作る（clock は runs[0].at の 1 時間前）
	seed   func(t *testing.T, rep *repo.Repo)
	broker func() *runFlowBroker
	runs   []runFlowRun
}

func runFlowCalendar() [][2]string {
	return [][2]string{{"2026-09-18", "1"}, {"2026-09-21", "0"}, {"2026-09-22", "1"}, {"2026-09-23", "0"},
		{"2026-09-24", "1"}, {"2026-09-25", "1"}}
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// runFlowBalance は余力 1,000 万円の口座。
func runFlowBalance() domain.Balance {
	return domain.Balance{Currency: "JPY", CashBalance: dec("10000000"), BuyingPower: dec("10000000"), MarketValue: decimal.Zero}
}

func heldPosition(sym, qty, cost, last string) domain.Position {
	return domain.Position{Symbol: sym, Quantity: dec(qty), AvailableQuantity: dec(qty), CostPrice: dec(cost),
		LastPrice: dec(last), Currency: "JPY", TaxType: domain.TaxAccountSpecific, Trade: domain.TradeTypeCash}
}

func seedOrder(t *testing.T, rep *repo.Repo, runID, cid, sym string, side domain.Side, qty, limit, status string, brokerID *string) {
	t.Helper()
	lp := dec(limit)
	req, err := domain.NewOrderRequest(cid, sym, side, domain.OrderTypeLimit, dec(qty), &lp,
		domain.TaxAccountSpecific, "seed", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.RecordOrder(runID, req, status, brokerID); err != nil {
		t.Fatal(err)
	}
}

func seedRun(t *testing.T, rep *repo.Repo, runID, asOf, env, mode string) {
	t.Helper()
	if err := rep.StartRun(runID, asOf, env, mode); err != nil {
		t.Fatal(err)
	}
}

func ptr(s string) *string { return &s }

func TestRunFlow(t *testing.T) {
	prod := settings.EnvProd
	buys := runFlowKnobs{buy: []string{"7203", "6758"}}
	at0931 := runFlowAt(24, 9, 31)
	live := func() []runFlowRun { return []runFlowRun{{at: at0931, live: true}} }
	cases := []runFlowCase{
		{
			// dry-run（--live なし）。メモリ上の模型で判断し、台帳には DRY_RUN で残す
			name: "dry_run", knobs: buys, env: prod, calendar: runFlowCalendar(),
			runs: []runFlowRun{{at: at0931}},
		},
		{
			// uat の --live は発注しない（dry-run と同じ）
			name: "uat_live", knobs: buys, env: settings.Environment("uat"), calendar: runFlowCalendar(),
			runs: live(),
		},
		{
			// 緊急停止（kill_switch）は --live でも発注しない
			name: "kill_switch", knobs: runFlowKnobs{buy: []string{"7203"}, killSwitch: true}, env: prod,
			calendar: runFlowCalendar(), runs: live(),
		},
		{
			// 発注する回の基本形: 建玉なし・台帳空で新規の買いを出す。2 回目は約定待ちのまま
			// （ユニバース外の保有が見えている）で、同じ注文を発注済みとして飛ばす。
			// 3 回目は建玉 0 件の照会で止まる（買いの約定が分からないので持っているはず）
			name: "live_buys", knobs: buys, env: prod, calendar: runFlowCalendar(),
			broker: func() *runFlowBroker { return &runFlowBroker{balance: runFlowBalance()} },
			runs: []runFlowRun{
				{at: at0931, live: true},
				{at: runFlowAt(24, 13, 31), live: true, prep: func(fb *runFlowBroker) {
					fb.positions = []domain.Position{heldPosition("1301", "100", "3000", "3100")}
					fb.orders = map[string]domain.Order{}
					for _, req := range fb.placed {
						fb.orders[req.ClientOrderID] = domain.Order{ClientOrderID: req.ClientOrderID,
							BrokerOrderID: ptr("B/" + req.Symbol), Symbol: req.Symbol, Side: req.Side,
							Quantity: req.Quantity, Status: domain.OrderStatusSubmitted}
					}
				}},
				{at: runFlowAt(24, 14, 31), live: true, prep: func(fb *runFlowBroker) { fb.positions = nil }},
			},
		},
		{
			// 成行の設定
			name: "live_market", knobs: runFlowKnobs{buy: []string{"7203"}, marketOrder: true}, env: prod,
			calendar: runFlowCalendar(),
			broker:   func() *runFlowBroker { return &runFlowBroker{balance: runFlowBalance()} },
			runs:     live(),
		},
		{
			// 売りを先に並べる・max_orders_per_day で買いを見送る。ユニバース外の保有には手を出さない
			name: "sells_first_max_orders", env: prod, calendar: runFlowCalendar(),
			knobs: runFlowKnobs{buy: []string{"7203", "6758"}, sell: []string{"9984"}, maxOrdersPerDay: 3},
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-1", "2026-09-22", "prod", "live")
				for _, sym := range []string{"9984", "8306"} {
					if err := rep.SaveStop(repo.StopRecord{Symbol: sym, StopPrice: dec("500"), EntryPrice: dec("900"),
						CreatedOn: "2026-09-01", ATRMultiple: dec("2")}); err != nil {
						t.Fatal(err)
					}
				}
			},
			broker: func() *runFlowBroker {
				return &runFlowBroker{balance: runFlowBalance(), positions: []domain.Position{
					heldPosition("9984", "200", "900", "1000"), heldPosition("8306", "300", "900", "1000"),
					heldPosition("1301", "100", "3000", "3100"),
				}}
			},
			runs: live(),
		},
		{
			// ストップの同期: 保有していない銘柄のストップを外す・無い銘柄に作る・割った銘柄を売る
			name: "stop_sync", env: prod, calendar: runFlowCalendar(),
			knobs: runFlowKnobs{buy: []string{"7203", "6758", "8306"}},
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-1", "2026-09-22", "prod", "live")
				// 8306 は終値（約 700 円）がストップ 800 円を割っている
				if err := rep.SaveStop(repo.StopRecord{Symbol: "8306", StopPrice: dec("800"), EntryPrice: dec("900"),
					CreatedOn: "2026-09-01", ATRMultiple: dec("2")}); err != nil {
					t.Fatal(err)
				}
				// 9984 は手仕舞い済み（建玉に無い）
				if err := rep.SaveStop(repo.StopRecord{Symbol: "9984", StopPrice: dec("900"), EntryPrice: dec("1000"),
					CreatedOn: "2026-09-01", ATRMultiple: dec("2")}); err != nil {
					t.Fatal(err)
				}
			},
			broker: func() *runFlowBroker {
				return &runFlowBroker{balance: runFlowBalance(), positions: []domain.Position{
					heldPosition("8306", "300", "900", "700"), heldPosition("7203", "100", "1000", "1200"),
				}}
			},
			runs: live(),
		},
		{
			// 建玉の照会が 0 件なのに台帳ではストップがある: 発注する回は止める
			name: "positions_empty", knobs: buys, env: prod, calendar: runFlowCalendar(),
			seed: func(t *testing.T, rep *repo.Repo) {
				if err := rep.SaveStop(repo.StopRecord{Symbol: "7203", StopPrice: dec("900"), EntryPrice: dec("1000"),
					CreatedOn: "2026-09-01", ATRMultiple: dec("2")}); err != nil {
					t.Fatal(err)
				}
			},
			broker: func() *runFlowBroker { return &runFlowBroker{balance: runFlowBalance()} },
			runs: []runFlowRun{
				{at: at0931, live: true},
				// 同じ台帳の dry-run は警告だけで続ける
				{at: runFlowAt(24, 9, 40)},
				// --accept-flat で 1 回だけ素通りする
				{at: runFlowAt(24, 9, 50), live: true, acceptFlat: true},
			},
		},
		{
			// --accept-flat は 1 回で失効する（同じ日に素通りして成功した回がある）
			name: "accept_flat_spent", knobs: buys, env: prod, calendar: runFlowCalendar(),
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-flat", "2026-09-24", "prod", "live")
				if err := rep.MarkAcceptFlat("seed-flat"); err != nil {
					t.Fatal(err)
				}
				if err := rep.FinishRun("seed-flat", "success", nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := rep.SaveStop(repo.StopRecord{Symbol: "7203", StopPrice: dec("900"), EntryPrice: dec("1000"),
					CreatedOn: "2026-09-01", ATRMultiple: dec("2")}); err != nil {
					t.Fatal(err)
				}
			},
			broker: func() *runFlowBroker { return &runFlowBroker{balance: runFlowBalance()} },
			runs:   []runFlowRun{{at: at0931, live: true, acceptFlat: true}},
		},
		{
			// 送信結果不明（PENDING）を送った直後: 判定できず発注を中止する（too_recent）
			name: "pending_too_recent", knobs: buys, env: prod, calendar: runFlowCalendar(),
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-1", "2026-09-24", "prod", "live")
			},
			broker: func() *runFlowBroker { return &runFlowBroker{balance: runFlowBalance()} },
			runs:   live(),
		},
		{
			// PENDING の判定: 一覧に無い（送っていない）→ UNSENT。前日以前の PENDING は判定しない
			name: "pending_not_sent", knobs: buys, env: prod, calendar: runFlowCalendar(),
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-1", "2026-09-24", "prod", "live")
			},
			broker: func() *runFlowBroker {
				return &runFlowBroker{balance: runFlowBalance(),
					positions: []domain.Position{heldPosition("1301", "100", "3000", "3100")}}
			},
			runs: live(),
		},
		{
			// 当日の損益を確かめられない（約定した売りの取得単価が分からない）: 新規の買いを止める
			name: "daily_pnl_unknown", knobs: runFlowKnobs{buy: []string{"7203"}}, env: prod,
			calendar: runFlowCalendar(),
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-1", "2026-09-24", "prod", "live")
				seedOrder(t, rep, "seed-1", "cid-sell", "9984", domain.SideSell, "100", "1000", "SUBMITTED", ptr("B3"))
			},
			broker: func() *runFlowBroker {
				fill := dec("990")
				return &runFlowBroker{balance: runFlowBalance(),
					positions: []domain.Position{heldPosition("1301", "100", "3000", "3100")},
					orders: map[string]domain.Order{"cid-sell": {ClientOrderID: "cid-sell", BrokerOrderID: ptr("B3"),
						Symbol: "9984", Side: domain.SideSell, Quantity: dec("100"), FilledQuantity: dec("100"),
						Status: domain.OrderStatusFilled, AvgFillPrice: &fill}},
				}
			},
			runs: live(),
		},
		{
			// PENDING の判定: 一覧を照会できない → 発注しない
			name: "pending_history_error", knobs: buys, env: prod, calendar: runFlowCalendar(),
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-1", "2026-09-24", "prod", "live")
			},
			broker: func() *runFlowBroker {
				return &runFlowBroker{balance: runFlowBalance(), historyErr: errors.New("一覧の照会に失敗")}
			},
			runs: live(),
		},
		{
			// 約定の取り込み: 約定を台帳へ、照会できない注文は保留。当日買付は売らない
			name: "fills", knobs: runFlowKnobs{buy: []string{"6758"}}, env: prod, calendar: runFlowCalendar(),
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-1", "2026-09-24", "prod", "live")
				seedOrder(t, rep, "seed-1", "cid-7203", "7203", domain.SideBuy, "100", "1000", "SUBMITTED", ptr("B1"))
				seedOrder(t, rep, "seed-1", "cid-9984", "9984", domain.SideBuy, "100", "1000", "SUBMITTED", ptr("B2"))
			},
			broker: func() *runFlowBroker {
				fill := dec("1000")
				return &runFlowBroker{balance: runFlowBalance(),
					positions: []domain.Position{heldPosition("7203", "100", "1000", "1010")},
					orders: map[string]domain.Order{"cid-7203": {ClientOrderID: "cid-7203", BrokerOrderID: ptr("B1"),
						Symbol: "7203", Side: domain.SideBuy, Quantity: dec("100"), FilledQuantity: dec("100"),
						Status: domain.OrderStatusFilled, AvgFillPrice: &fill}},
					orderErrs: map[string]error{"cid-9984": errors.New("照会に失敗")},
				}
			},
			runs: live(),
		},
		{
			// 休場日は判断も発注もしない
			name: "holiday", knobs: buys, env: prod, calendar: runFlowCalendar(),
			broker: func() *runFlowBroker { return &runFlowBroker{balance: runFlowBalance()} },
			runs:   []runFlowRun{{at: runFlowAt(23, 9, 31), live: true}},
		},
		{
			// カレンダーが読めない: 発注する回は止め、dry-run は平日として続ける（足は古いとみなす）
			name: "calendar_empty", knobs: buys, env: prod,
			broker: func() *runFlowBroker { return &runFlowBroker{balance: runFlowBalance()} },
			runs:   []runFlowRun{{at: at0931, live: true}, {at: runFlowAt(24, 9, 40)}},
		},
		{
			// 足が古い銘柄は判断しない（保有中なら通知）
			name: "bars_stale", knobs: runFlowKnobs{buy: []string{"6758"}}, env: prod, calendar: runFlowCalendar(),
			lastBar: map[string]string{"7203": "2026-09-18", "9984": "2026-09-18"},
			seed: func(t *testing.T, rep *repo.Repo) {
				seedRun(t, rep, "seed-1", "2026-09-22", "prod", "live")
			},
			broker: func() *runFlowBroker {
				return &runFlowBroker{balance: runFlowBalance(),
					positions: []domain.Position{heldPosition("7203", "100", "1000", "1010")}}
			},
			runs: live(),
		},
		{
			// 建玉を照会できない: 発注する回は止める
			name: "positions_error", knobs: buys, env: prod, calendar: runFlowCalendar(),
			broker: func() *runFlowBroker {
				return &runFlowBroker{balance: runFlowBalance(), positionsErr: errors.New("建玉の照会に失敗")}
			},
			runs: live(),
		},
		{
			// 板の注文を照会できない: 発注する回は止める
			name: "open_orders_error", knobs: buys, env: prod, calendar: runFlowCalendar(),
			broker: func() *runFlowBroker {
				return &runFlowBroker{balance: runFlowBalance(), openErr: errors.New("注文の照会に失敗")}
			},
			runs: live(),
		},
		{
			// 板に残る注文は二重に出さない
			name: "open_orders", knobs: buys, env: prod, calendar: runFlowCalendar(),
			broker: func() *runFlowBroker {
				lp := dec("1000")
				return &runFlowBroker{balance: runFlowBalance(), openOrders: []domain.Order{{
					ClientOrderID: "cid-open", BrokerOrderID: ptr("B9"), Symbol: "7203", Side: domain.SideBuy,
					OrderType: domain.OrderTypeLimit, Quantity: dec("100000"), Status: domain.OrderStatusSubmitted,
					LimitPrice: &lp, Trade: domain.TradeTypeCash,
				}}}
			},
			runs: live(),
		},
		{
			// 拒否は記録して次へ、結果の分からない注文で止める
			name: "order_failures", knobs: runFlowKnobs{buy: []string{"7203", "6758", "9984"}}, env: prod,
			calendar: runFlowCalendar(),
			broker: func() *runFlowBroker {
				return &runFlowBroker{balance: runFlowBalance(), placeErr: map[string]error{
					"6758": &broker.OrderRejectedError{Message: "余力不足"},
					"7203": errors.New("通信が切れた"),
				}}
			},
			runs: live(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runRunFlowCase(t, c)
			path := filepath.Join("testdata", "run_flow", c.name+".golden")
			if *updateRunFlow {
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
				t.Fatalf("期待値がありません（-update-run-flow で作る）: %v", err)
			}
			if got != string(want) {
				t.Errorf("runDaily の流れが変わった（%s）:\n%s", path, runFlowFirstDiff(string(want), got))
			}
		})
	}
}

// runFlowFirstDiff は最初に食い違った行の前後を返す。
func runFlowFirstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < max(len(w), len(g)); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("行 %d\n want: %s\n  got: %s", i+1, wl, gl)
		}
	}
	return "（行は同じ）"
}

// runRunFlowCase は 1 ケースを走らせ、観測したものを文書にして返す。
func runRunFlowCase(t *testing.T, c runFlowCase) string {
	root := t.TempDir()
	s := &settings.AppSettings{
		Env: c.env, StateDir: filepath.Join(root, "state"), DataDir: filepath.Join(root, "data"),
		LogDir: filepath.Join(root, "logs"), LogLevel: "INFO", LogJSON: true, Timezone: "Asia/Tokyo",
		DotenvMap: map[string]string{},
	}
	for _, dir := range []string{s.StateDir, s.DataDir, s.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.toml"), []byte(runFlowSettings(c.knobs)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "strategies.toml"), []byte(runFlowStrategies(c.knobs)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRunFlowCalendar(t, s, c.calendar)
	writeRunFlowBars(t, s, c.lastBar)

	savedSettings, savedConfigDir, savedRun, savedNow, savedBroker := appSettings, configDirFlag, run, clock.Now, runBroker
	t.Cleanup(func() {
		appSettings, configDirFlag, run, clock.Now, runBroker = savedSettings, savedConfigDir, savedRun, savedNow, savedBroker
		digest.Reset()
	})
	appSettings, configDirFlag = s, configDir
	fake := &runFlowClock{}
	clock.Now = fake.Now

	// 台帳の下ごしらえ（1 時間前に作った形）
	if c.seed != nil || strings.HasPrefix(c.name, "pending_") {
		fake.set(c.runs[0].at.Add(-time.Hour))
		rep, err := repo.OpenRepo(s.DBPath())
		if err != nil {
			t.Fatal(err)
		}
		if c.seed != nil {
			c.seed(t, rep)
		}
		seedPending(t, rep, c.name, fake, c.runs[0].at)
		rep.Close()
	}

	fb := &runFlowBroker{}
	if c.broker != nil {
		fb = c.broker()
	}
	var doc strings.Builder
	var runIDs []string
	for i, r := range c.runs {
		fake.set(r.at)
		if r.prep != nil {
			r.prep(fb)
		}
		connects := 0
		runBroker = func(name string, _ *settings.AppSettings) (broker.Broker, error) {
			connects++
			if name != "tachibana" {
				return nil, fmt.Errorf("未知の broker: %q", name)
			}
			return fb, nil
		}
		callsBefore, placedBefore := len(fb.calls), len(fb.placed)
		logsBefore := len(readRunFlowLogs(t, s))

		var alerts []string
		stdout, err := captureRunFlowStdout(t, func() error {
			run = cli.StartRun("wbjp", s, "run")
			runIDs = append(runIDs, run.RunID)
			run.Alerter = func(title, body string, _ *logging.Logger) bool {
				alerts = append(alerts, title+" | "+body)
				return true
			}
			err := run.Crash("日次実行", "wbjp.crash", runDaily(r.live, true, !r.sync, false, r.acceptFlat))
			run.Finish(err)
			return err
		})

		norm := func(text string) string { return normalizeRunFlow(root, runIDs, text) }
		fmt.Fprintf(&doc, "=== 実行 %d: %s JST live=%v accept_flat=%v env=%s\n", i+1,
			r.at.In(clock.Tokyo).Format("2006-01-02 15:04"), r.live, r.acceptFlat, c.env)
		fmt.Fprintf(&doc, "--- 戻り値\n%v\n", norm(runFlowErrText(err)))
		fmt.Fprintf(&doc, "--- ブローカーへの接続: %d 回\n", connects)
		doc.WriteString("--- ブローカーの呼び出し\n")
		for _, call := range fb.calls[callsBefore:] {
			fmt.Fprintf(&doc, "%s\n", norm(call))
		}
		doc.WriteString("--- 出した注文\n")
		for _, req := range fb.placed[placedBefore:] {
			fmt.Fprintf(&doc, "%s\n", norm(runFlowJSON(runFlowRequestView(req))))
		}
		doc.WriteString("--- 通知\n")
		for _, a := range alerts {
			fmt.Fprintf(&doc, "%s\n", norm(a))
		}
		doc.WriteString("--- 端末\n")
		doc.WriteString(norm(stdout))
		doc.WriteString("--- ログ\n")
		for _, line := range sortUnordered(readRunFlowLogs(t, s)[logsBefore:]) {
			fmt.Fprintf(&doc, "%s\n", norm(line))
		}
		doc.WriteString("--- ダイジェスト\n")
		for _, line := range readRunFlowDigest(t, s, i) {
			fmt.Fprintf(&doc, "%s\n", norm(line))
		}
	}
	doc.WriteString("=== 最後の状態\n")
	for _, q := range runFlowLedgerQueries {
		fmt.Fprintf(&doc, "--- 台帳 %s\n", q.name)
		for _, line := range readRunFlowLedger(t, s, q.query, func(text string) string {
			return normalizeRunFlow(root, runIDs, text)
		}) {
			fmt.Fprintf(&doc, "%s\n", line)
		}
	}
	return doc.String()
}

// seedPending はケースの名前で決まる PENDING を台帳に置く。
func seedPending(t *testing.T, rep *repo.Repo, name string, fake *runFlowClock, at time.Time) {
	t.Helper()
	switch name {
	case "pending_too_recent":
		// 送ったのは 2 秒前（Grace の 5 秒未満）
		fake.set(at.Add(-2 * time.Second))
		seedOrder(t, rep, "seed-1", "cid-pend", "7203", domain.SideBuy, "100", "1000", "PENDING", nil)
	case "pending_not_sent", "pending_history_error":
		// 今日 10 分前に送った PENDING（一覧に無ければ届いていない）と、前日の PENDING（判定しない）
		fake.set(at.Add(-24 * time.Hour))
		seedOrder(t, rep, "seed-1", "cid-old", "9984", domain.SideBuy, "100", "1000", "PENDING", nil)
		fake.set(at.Add(-10 * time.Minute))
		seedOrder(t, rep, "seed-1", "cid-pend", "7203", domain.SideBuy, "100", "1000", "PENDING", nil)
	}
}

// runFlowLedgerQueries は最後に読む台帳の表（順が決まるように並べる）。
var runFlowLedgerQueries = []struct{ name, query string }{
	{"runs", "SELECT * FROM runs ORDER BY started_at, run_id"},
	{"orders", "SELECT * FROM orders ORDER BY placed_at, client_order_id"},
	{"stops", "SELECT * FROM stops ORDER BY symbol"},
	{"risk_events", "SELECT run_id, symbol, reason, created_at FROM risk_events ORDER BY created_at, symbol"},
	{"position_snapshots", "SELECT run_id, as_of, symbol, quantity, cost_price, last_price FROM position_snapshots ORDER BY id"},
	{"targets", "SELECT run_id, symbol, quantity, reason FROM targets ORDER BY id"},
	{"combined_signals", "SELECT run_id, symbol, direction, contributions_json, reason FROM combined_signals ORDER BY id"},
	{"signals", "SELECT run_id, strategy, symbol, direction, confidence, reason FROM signals ORDER BY id"},
}

// readRunFlowLedger は台帳の表を 1 行 1 JSON で返す（norm で run_id などを置き換えてから並べる）。targets・risk_events は map から書くので
// 行の順が実行ごとに変わる（並べ替えて比べる）。
func readRunFlowLedger(t *testing.T, s *settings.AppSettings, query string, norm func(string) string) []string {
	t.Helper()
	if _, err := os.Stat(s.DBPath()); err != nil {
		return []string{"<台帳なし>"}
	}
	db, err := storage.OpenSQLite(s.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		rec := make(map[string]any, len(cols))
		for i, col := range cols {
			if vals[i].Valid {
				rec[col] = vals[i].String
			} else {
				rec[col] = nil
			}
		}
		out = append(out, norm(runFlowJSON(rec)))
	}
	if strings.Contains(query, "FROM targets") || strings.Contains(query, "FROM risk_events") {
		sort.Strings(out)
	}
	return out
}

// runFlowUnordered は map の順に出るログ（同じコードが続く間は並べ替えて比べる）。
var runFlowUnordered = map[string]bool{"wbjp.reconcile_skip": true, "wbjp.risk_rejected": true}

func sortUnordered(lines []string) []string {
	out := append([]string(nil), lines...)
	code := func(line string) string {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			return ""
		}
		c, _ := rec["event"].(string)
		return c
	}
	for i := 0; i < len(out); {
		c := code(out[i])
		j := i + 1
		for j < len(out) && code(out[j]) == c {
			j++
		}
		if runFlowUnordered[c] {
			sort.Strings(out[i:j])
		}
		i = j
	}
	return out
}

func runFlowErrText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// captureRunFlowStdout は f の間の標準出力を集める。
func captureRunFlowStdout(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	ferr := f()
	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, ferr
}

func writeRunFlowCalendar(t *testing.T, s *settings.AppSettings, rows [][2]string) {
	t.Helper()
	if rows == nil {
		return
	}
	str := func(v string) *string { return &v }
	arch := archive.NewArchive(s.JQuantsArchiveDir())
	frame := &archive.Frame{Columns: []string{"Date", "HolDiv"}}
	for _, row := range rows {
		frame.AppendRow(map[string]*string{"Date": str(row[0]), "HolDiv": str(row[1])})
	}
	if _, err := arch.Upsert(archive.CalendarEndpoint(), frame); err != nil {
		t.Fatal(err)
	}
}

// writeRunFlowBars は銘柄ごとに 120 本の日足を置く（平日。最後は lastBar か 2026-09-22）。
// 値は銘柄ごとの傾きと波で決まる（8306 は下げて 700 円前後で終わる）。
func writeRunFlowBars(t *testing.T, s *settings.AppSettings, lastBar map[string]string) {
	t.Helper()
	store := data.NewBarStore(s.BarsDir())
	shape := map[string][2]float64{"7203": {1000, 0.2}, "6758": {2000, 0.1}, "9984": {1000, -0.05}, "8306": {1000, -0.3}}
	for _, sym := range runFlowUniverse {
		last := "2026-09-22"
		if v, ok := lastBar[sym]; ok {
			last = v
		}
		end, err := time.Parse("2006-01-02", last)
		if err != nil {
			t.Fatal(err)
		}
		var dates []string
		for d := end; len(dates) < 120; d = d.AddDate(0, 0, -1) {
			if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
				continue
			}
			dates = append(dates, d.Format("2006-01-02"))
		}
		sort.Strings(dates)
		base, slope := shape[sym][0], shape[sym][1]
		bars := make([]domain.Bar, 0, len(dates))
		for i, date := range dates {
			close := base * (1 + slope*float64(i)/120) * (1 + 0.01*math.Sin(float64(i)/3))
			c := decimal.NewFromFloat(close).Round(1)
			bars = append(bars, domain.Bar{Symbol: sym, Date: date, Open: c,
				High: c.Mul(dec("1.01")).Round(1), Low: c.Mul(dec("0.99")).Round(1), Close: c,
				Volume: decimal.NewFromInt(1_000_000)})
		}
		if err := store.Write(sym, bars); err != nil {
			t.Fatal(err)
		}
	}
}

func runFlowRequestView(req domain.OrderRequest) map[string]any {
	view := map[string]any{
		"client_order_id": req.ClientOrderID, "symbol": req.Symbol, "side": req.Side,
		"quantity": req.Quantity.String(), "type": req.OrderType, "trade": req.Trade,
		"tax": req.TaxType, "reason": req.Reason,
	}
	if req.LimitPrice != nil {
		view["limit_price"] = req.LimitPrice.String()
	}
	return view
}

// readRunFlowLogs はこれまでの全実行のログ（JSONL）。時刻と run_id は落とす。
func readRunFlowLogs(t *testing.T, s *settings.AppSettings) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(s.ResolvedLogDir(), "*"))
	sort.Strings(files)
	var out []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("ログを読めません: %v: %s", err, line)
			}
			delete(rec, "ts_utc")
			delete(rec, "run_id")
			out = append(out, runFlowJSON(rec))
		}
	}
	return out
}

func readRunFlowDigest(t *testing.T, s *settings.AppSettings, i int) []string {
	t.Helper()
	var lines []string
	for _, day := range []string{"2026-09-23", "2026-09-24"} {
		raw, err := os.ReadFile(digest.Path(s.StateDir, string(s.Env), day))
		if err != nil {
			continue
		}
		lines = append(lines, strings.Split(strings.TrimSpace(string(raw)), "\n")...)
	}
	if i >= len(lines) {
		return []string{"<なし>"}
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
		t.Fatal(err)
	}
	delete(rec, "ts_utc")
	delete(rec, "run_id")
	delete(rec, "dur_ms")
	return []string{runFlowJSON(rec)}
}

func runFlowJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<JSON にできない: %v>", err)
	}
	return string(raw)
}

// normalizeRunFlow は一時ディレクトリを <root> に、実行ごとの run_id を <run N> に置き換える。
func normalizeRunFlow(root string, runIDs []string, text string) string {
	text = strings.ReplaceAll(text, root, "<root>")
	for i, id := range runIDs {
		if id != "" {
			text = strings.ReplaceAll(text, id, fmt.Sprintf("<run %d>", i+1))
		}
	}
	return text
}
