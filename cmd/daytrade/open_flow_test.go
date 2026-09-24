package main

// runOpen の流れのテスト（characterization test）。
//
// 設定・カレンダー・plan・米国市場のキャッシュ・気配（csv）・台帳を一時ディレクトリに作り、
// 偽のブローカー（openBroker の差し替え）で runOpen を最後まで走らせる。出した注文・台帳・
// ログのイベント・ダイジェスト・通知・履歴（open_run・順位表）・端末の出力を 1 本の文書に
// 書き出し、testdata/open_flow/*.golden と突き合わせる。**今の挙動をそのまま固定する**もので、
// 正しさの判定ではない。runOpen を分けるときに、この文書が 1 文字も変わらないことを確かめる。
//
// 時計は 1 回読むごとに 1 ms 進む（flowClock）。open_run の elapsed_ms とログの並びが
// clock.Now の呼び回数を映すので、途中に時刻の読み取りや I/O を足すと差分に出る。
//
// 期待値を作り直すとき: go test ./cmd/daytrade -run TestOpenFlow -update-open-flow

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

var updateOpenFlow = flag.Bool("update-open-flow", false, "runOpen の流れのテストの期待値（testdata/open_flow）を書き直す")

// TestMain は外への接続を塞ぐ。米国市場のキャッシュが古い経路は Cboe・Yahoo・FRED に取りに行くので、
// 届かない proxy に向けて即座に失敗させる（テストからネットワークに繋がない）。
func TestMain(m *testing.M) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		os.Setenv(key, "http://127.0.0.1:9")
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

// flowDay は判定日（木曜）。前夜の米国セッションは 2026-09-23（水）。
var flowDay = time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

// jstAt は判定日の JST の時刻。
func jstAt(hour, minute, sec, ms int) time.Time {
	return time.Date(2026, 9, 24, hour, minute, sec, ms*int(time.Millisecond), clock.Tokyo)
}

// flowClock は読むたびに 1 ms 進む時計。
type flowClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *flowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	c.now = c.now.Add(time.Millisecond)
	return t
}

func (c *flowClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// flowBroker は発注の流れを見るための偽のブローカー（execute の stubBroker と同じ形）。
type flowBroker struct {
	place     func(domain.OrderRequest) (*domain.OrderAck, error)
	positions []domain.Position
	// history は当日の注文一覧（送った注文から作る。nil なら空）
	history   func(placed []domain.OrderRequest) []domain.Order
	balance   domain.Balance
	placed    []domain.OrderRequest
	cancelled []string
}

func (s *flowBroker) Name() string      { return "flow" }
func (s *flowBroker) AccountID() string { return "flow" }
func (s *flowBroker) GetBalance() (*domain.Balance, error) {
	b := s.balance
	return &b, nil
}
func (s *flowBroker) GetPositions() ([]domain.Position, error) { return s.positionsOfKind(false), nil }
func (s *flowBroker) MarginPositions() ([]domain.Position, error) {
	return s.positionsOfKind(true), nil
}
func (s *flowBroker) positionsOfKind(margin bool) []domain.Position {
	var out []domain.Position
	for _, p := range s.positions {
		if broker.LegOf(p.Symbol, p.Trade, p.Quantity.IsNegative()).Margin == margin {
			out = append(out, p)
		}
	}
	return out
}
func (s *flowBroker) PositionsBySymbol() (map[string]domain.Position, error) {
	return broker.PositionsBySymbolHelper(s.positions), nil
}
func (s *flowBroker) GetOpenOrders() ([]domain.Order, error) { return nil, nil }
func (s *flowBroker) GetOrder(string, *string) (*domain.Order, error) {
	return nil, nil
}
func (s *flowBroker) GetOrderHistory(_, _ time.Time) ([]domain.Order, error) {
	if s.history == nil {
		return nil, nil
	}
	return s.history(s.placed), nil
}
func (s *flowBroker) Preview(domain.OrderRequest) (*domain.OrderPreview, error) {
	return &domain.OrderPreview{}, nil
}
func (s *flowBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	s.placed = append(s.placed, req)
	if s.place != nil {
		return s.place(req)
	}
	id := "N/" + req.Symbol
	return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
}
func (s *flowBroker) Cancel(clientOrderID string, _ *string) error {
	s.cancelled = append(s.cancelled, clientOrderID)
	return nil
}
func (s *flowBroker) LotSizes([]string) map[string]decimal.Decimal {
	return map[string]decimal.Decimal{}
}

// flowKnobs は設定の中でケースごとに変える項目。
type flowKnobs struct {
	marginPaused      bool
	excludeCorpEvents bool
	rankByUsLow       string // 空なら書かない（gap_vol のまま）
	preopenLegs       string
	noSpill           bool // margin.spill_to_long = false（件数で「発注済み」と言える形）
	watchOnly         bool // capital.max_capital = 0（候補を見せるだけ）
}

func flowConfig(k flowKnobs) string {
	legs := k.preopenLegs
	if legs == "" {
		legs = "long"
	}
	usLow := ""
	if k.rankByUsLow != "" {
		usLow = fmt.Sprintf("rank_by_us_low = %q\nmodel = \"models/missing.txt\"\n", k.rankByUsLow)
	}
	maxCapital := 2000000
	if k.watchOnly {
		maxCapital = 0
	}
	return fmt.Sprintf(`[capital]
enabled = true
max_capital = %d
order_budget = 670000
max_positions = 10
weighting = "inverse_vol"
max_order = 1000000

[signal]
max_gap = 0.0
min_gap = -1.0
skip_limit_down = true
skip_opened = false
rank_by = "gap_vol"
%smax_per_sector = 1

[regime]
iv_gate = 0.0
skip_months = [12]
equity_curve_days = 0
us_skip_low = 0.0
us_skip_high = 0.01
us_vix_override = 24
us_skip_legs = "short"
us_stale_wait_until = "09:12"
shock_market_gap = -0.02
shock_us_ret = -0.02
shock_long_scale = 1.5
shock_short_scale = 0.0

[execution]
broker = "tachibana"
quote_source = "tachibana"
entry_window = ["08:59", "09:15"]
preopen_legs = %q
preopen_limit_pct_us_low = 1.5
exit_window = ["15:20", "15:30"]
max_quote_age = 90
max_run_seconds = 150

[margin]
enabled = true
max_capital = 2000000
order_budget = 670000
max_positions = 10
weighting = "inverse_vol"
exclude_corp_events = %v
corp_event_max_staleness_minutes = 90
min_gap = 0.05
max_gap = 1.0
skip_limit_up = true
multiplier_normal = 1.0
multiplier_long_weak = 1.0
max_order = 1000000
spill_to_long = %v
paused = %v
long_via_margin = true
`, maxCapital, usLow, legs, k.excludeCorpEvents, !k.noSpill, k.marginPaused)
}

// flowCandidate は plan の 1 銘柄と、その朝の気配。
type flowCandidate struct {
	symbol    string
	prevClose float64
	vol       float64
	sector    string
	long      bool
	short     bool
	quote     float64 // 0 なら気配の CSV に載せない
}

// flowUniverse はロング 6 銘柄（ギャップ下げ）とショート 3 銘柄（ギャップ上げ）。業種は 2 銘柄だけ重ねる。
// ロングの中央値が −1.1% で、市場ギャップのショック（−2%）には掛からない。
func flowUniverse() []flowCandidate {
	return []flowCandidate{
		{"1001", 1000, 0.020, "3050", true, false, 980},
		{"1002", 2000, 0.025, "3100", true, false, 1970},
		{"1003", 1500, 0.030, "3100", true, false, 1482},
		{"1004", 800, 0.015, "3200", true, false, 792},
		{"1005", 3000, 0.022, "3250", true, false, 2985},
		{"1006", 500, 0.018, "3300", true, false, 501},
		{"2001", 1200, 0.020, "3350", false, true, 1320},
		{"2002", 900, 0.025, "3400", false, true, 972},
		{"2003", 2500, 0.030, "3450", false, true, 2550},
	}
}

// flowCase は 1 回ぶんの runOpen の状況。
type flowCase struct {
	name     string
	knobs    flowKnobs
	calendar [][2]string // Date・HolDiv。nil ならカレンダーを作らない（空）
	usRet    *float64    // 前夜の S&P500 の騰落。nil なら前夜のセッションをキャッシュに入れない
	env      settings.Environment
	// runs は同じ状態（台帳・履歴）の上で続けて回す実行。
	runs []flowRun
}

type flowRun struct {
	at   time.Time
	opts openOptions
	// place は偽のブローカーの応答（nil なら受理）。history は当日の注文一覧。
	place   func(domain.OrderRequest) (*domain.OrderAck, error)
	history func(placed []domain.OrderRequest) []domain.Order
}

func ret(v float64) *float64 { return &v }

func tradingCalendar() [][2]string {
	return [][2]string{{"2026-09-22", "1"}, {"2026-09-23", "0"}, {"2026-09-24", "1"}, {"2026-09-25", "1"}}
}

func liveOpts() openOptions { return openOptions{live: true, yes: true} }

func TestOpenFlow(t *testing.T) {
	rejectSymbol := func(symbol string) func(domain.OrderRequest) (*domain.OrderAck, error) {
		return func(req domain.OrderRequest) (*domain.OrderAck, error) {
			if req.Symbol == symbol {
				return nil, &broker.OrderRejectedError{Message: "残高不足"}
			}
			id := "N/" + req.Symbol
			return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
		}
	}
	cases := []flowCase{
		{
			// 寄る前の回（8:59:50）: ロングを寄成で。ショートは paused
			name: "preopen_long", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(8, 59, 50, 0), opts: liveOpts()}},
		},
		{
			// 9:00:01.2 の回: ザラ場の成行。ショートは paused
			name: "open_0900_long", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}},
		},
		{
			// ショートを開けた日（材料の除外なし）: ロングと売建の両方
			name: "short_open", knobs: flowKnobs{marginPaused: false, excludeCorpEvents: false},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}},
		},
		{
			// 材料の記録簿が読めない: ショートを見送り、余りもロングへ回さない
			name: "corp_stale", knobs: flowKnobs{marginPaused: false, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}},
		},
		{
			name: "holiday", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: [][2]string{{"2026-09-22", "1"}, {"2026-09-24", "0"}, {"2026-09-25", "1"}},
			usRet:    ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}},
		},
		{
			name: "half_day", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: [][2]string{{"2026-09-22", "1"}, {"2026-09-24", "2"}, {"2026-09-25", "1"}},
			usRet:    ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}, {at: jstAt(9, 3, 1, 200), opts: liveOpts()}},
		},
		{
			// カレンダーが空: live は見送り、dry-run は平日で代用して続ける
			name: "calendar_empty", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: nil, usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}, {at: jstAt(9, 0, 5, 0), opts: openOptions{}}},
		},
		{
			name: "window_closed", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 20, 0, 0), opts: liveOpts()}},
		},
		{
			// 米国小幅高（+0.5%）の寄る前の回: ロングは寄指（前日終値 −1.5%）、ショートは休み
			name: "us_low_preopen", knobs: flowKnobs{marginPaused: false, excludeCorpEvents: false},
			calendar: tradingCalendar(), usRet: ret(0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(8, 59, 50, 0), opts: liveOpts()}},
		},
		{
			// 米国小幅高の 9:00 以降の回: 見送り（見送りの順位表を積む）
			name: "us_low_0900", knobs: flowKnobs{marginPaused: false, excludeCorpEvents: false},
			calendar: tradingCalendar(), usRet: ret(0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}},
		},
		{
			// LightGBM で並べる設定なのにモデルが無い: gap_vol に戻す（小幅高の日は両脚とも休む）
			name: "us_low_lgbm_fallback", knobs: flowKnobs{marginPaused: false, excludeCorpEvents: false, rankByUsLow: "lgbm"},
			calendar: tradingCalendar(), usRet: ret(0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(8, 59, 50, 0), opts: liveOpts()}},
		},
		{
			// 米国のショック日（−3%）: ショートの倍率 0・ロングの倍率 1.5
			name: "us_shock", knobs: flowKnobs{marginPaused: false, excludeCorpEvents: false},
			calendar: tradingCalendar(), usRet: ret(-0.03), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}},
		},
		{
			// 前夜の米国市場がまだ無い: 9:12 までは待って見送り、過ぎたら取れないまま判定する
			name: "us_stale", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: nil, env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}, {at: jstAt(9, 13, 0, 0), opts: liveOpts()}},
		},
		{
			// 再実行: 1 回目で 1 銘柄が拒否 → 2 回目は残りだけ建てる → 3 回目は発注済みで何もしない
			name: "rerun", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true, noSpill: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{
				{at: jstAt(9, 0, 1, 200), opts: liveOpts(), place: rejectSymbol("1001")},
				{at: jstAt(9, 3, 1, 200), opts: liveOpts()},
				{at: jstAt(9, 6, 1, 200), opts: liveOpts()},
			},
		},
		{
			// 再実行（余りをロングに回す設定）: 件数だけでは「済み」と言わず、金額で数え直す
			name: "rerun_spill", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}, {at: jstAt(9, 3, 1, 200), opts: liveOpts()}},
		},
		{
			// 資金 0: 候補を見せるだけで建てない
			name: "watch_only", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true, watchOnly: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}},
		},
		{
			// 発注の失敗（拒否）と、送ったが結果の分からない注文（一覧で届いていたと判定）
			name: "order_failures", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{
				at: jstAt(9, 0, 1, 200), opts: liveOpts(),
				place: func(req domain.OrderRequest) (*domain.OrderAck, error) {
					switch req.Symbol {
					case "1001":
						return nil, &broker.OrderRejectedError{Message: "規制銘柄"}
					case "1004":
						return nil, errors.New("read tcp: i/o timeout")
					}
					id := "N/" + req.Symbol
					return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
				},
				history: func(placed []domain.OrderRequest) []domain.Order {
					var out []domain.Order
					for _, req := range placed {
						if req.Symbol != "1004" {
							continue
						}
						id := "N/1004"
						out = append(out, domain.Order{
							ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Symbol: req.Symbol,
							Side: req.Side, Quantity: req.Quantity, Status: domain.OrderStatusSubmitted,
							Trade: req.Trade,
						})
					}
					return out
				},
			}},
		},
		{
			// dry-run を 2 回: その日の古い dry-run は消して最新だけ残す
			name: "dry_run", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.EnvProd,
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: openOptions{}}, {at: jstAt(9, 1, 0, 0), opts: openOptions{}}},
		},
		{
			// テスト口座（uat）の --live は発注しない（dry-run と同じ経路）
			name: "uat_live", knobs: flowKnobs{marginPaused: true, excludeCorpEvents: true},
			calendar: tradingCalendar(), usRet: ret(-0.005), env: settings.Environment("uat"),
			runs: []flowRun{{at: jstAt(9, 0, 1, 200), opts: liveOpts()}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runFlowCase(t, c)
			path := filepath.Join("testdata", "open_flow", c.name+".golden")
			if *updateOpenFlow {
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
				t.Fatalf("期待値がありません（-update-open-flow で作る）: %v", err)
			}
			if got != string(want) {
				t.Errorf("runOpen の流れが変わった（%s）:\n%s", path, firstDiff(string(want), got))
			}
		})
	}
}

// firstDiff は最初に食い違った行の前後を返す。
func firstDiff(want, got string) string {
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

// runFlowCase は 1 ケースを走らせ、観測したものを文書にして返す。
func runFlowCase(t *testing.T, c flowCase) string {
	root := t.TempDir()
	s := &settings.AppSettings{
		Env: c.env, StateDir: filepath.Join(root, "state"), DataDir: filepath.Join(root, "data"),
		LogDir: filepath.Join(root, "logs"), LogLevel: "INFO", LogJSON: true, Timezone: "Asia/Tokyo",
		DotenvMap: map[string]string{},
	}
	for _, dir := range []string{s.StateDir, s.DataDir, s.LogDir, s.DaytradeDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, dtconfig.Filename), []byte(flowConfig(c.knobs)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFlowCalendar(t, s, c.calendar)
	writeFlowPlan(t, s)
	writeFlowUs(t, s, c.usRet)
	quoteFile := writeFlowQuotes(t, root)

	savedSettings, savedConfigDir, savedRun, savedNow, savedBroker := appSettings, configDirFlag, run, clock.Now, openBroker
	t.Cleanup(func() {
		appSettings, configDirFlag, run, clock.Now, openBroker = savedSettings, savedConfigDir, savedRun, savedNow, savedBroker
		digest.Reset()
	})
	appSettings, configDirFlag = s, configDir
	fake := &flowClock{}
	clock.Now = fake.Now

	fb := &flowBroker{balance: domain.Balance{
		BuyingPower: decimal.NewFromInt(50_000_000), MarginBuyingPower: ptrDecimal(decimal.NewFromInt(50_000_000)),
	}}
	var doc strings.Builder
	for i, r := range c.runs {
		fake.set(r.at)
		fb.place, fb.history = r.place, r.history
		connects := 0
		openBroker = func(cfg dtconfig.Config) (broker.Broker, error) {
			connects++
			if cfg.Execution.Broker != "tachibana" {
				return nil, fmt.Errorf("未知の broker: %q", cfg.Execution.Broker)
			}
			return fb, nil
		}
		placedBefore := len(fb.placed)
		logsBefore := len(readLogs(t, s))

		opts := r.opts
		opts.quoteSource, opts.quoteFile = "csv", quoteFile
		var alerts []string
		stdout, err := captureStdout(t, func() error {
			run = cli.StartRun("daytrade", s, "open")
			run.Alerter = func(title, body string, _ *logging.Logger) bool {
				alerts = append(alerts, title+" | "+body)
				return true
			}
			err := crash("寄付の買い", "daytrade.crash", runOpen(opts))
			run.Finish(err)
			return err
		})

		fmt.Fprintf(&doc, "=== 実行 %d: %s JST live=%v env=%s\n", i+1,
			r.at.In(clock.Tokyo).Format("15:04:05.000"), r.opts.live, c.env)
		fmt.Fprintf(&doc, "--- 戻り値\n%v\n", errText(err))
		fmt.Fprintf(&doc, "--- ブローカーへの接続: %d 回\n", connects)
		doc.WriteString("--- 出した注文\n")
		for _, req := range fb.placed[placedBefore:] {
			fmt.Fprintf(&doc, "%s\n", normalizeJSON(root, orderRequestView(req)))
		}
		doc.WriteString("--- 取消\n")
		for _, id := range fb.cancelled {
			fmt.Fprintf(&doc, "%s\n", id)
		}
		fb.cancelled = nil
		doc.WriteString("--- 通知\n")
		for _, a := range alerts {
			fmt.Fprintf(&doc, "%s\n", normalize(root, a))
		}
		doc.WriteString("--- 端末\n")
		doc.WriteString(normalize(root, stdout))
		doc.WriteString("--- ログ\n")
		for _, line := range readLogs(t, s)[logsBefore:] {
			fmt.Fprintf(&doc, "%s\n", normalize(root, line))
		}
		doc.WriteString("--- ダイジェスト\n")
		for _, line := range readDigest(t, s, i) {
			fmt.Fprintf(&doc, "%s\n", normalize(root, line))
		}
	}
	doc.WriteString("=== 最後の状態\n--- 台帳\n")
	for _, line := range readLedger(t, s) {
		fmt.Fprintf(&doc, "%s\n", normalize(root, line))
	}
	for _, kind := range []string{dthistory.KindOpenRun, dthistory.KindRanking, dthistory.KindQuotes} {
		fmt.Fprintf(&doc, "--- 履歴 %s\n", kind)
		for _, line := range readHistory(t, s, kind) {
			fmt.Fprintf(&doc, "%s\n", normalize(root, line))
		}
	}
	return doc.String()
}

func ptrDecimal(d decimal.Decimal) *decimal.Decimal { return &d }

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// captureStdout は f の間の標準出力を集める（runOpen は fmt.Print と env.Out に os.Stdout を使う）。
func captureStdout(t *testing.T, f func() error) (string, error) {
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

func writeFlowCalendar(t *testing.T, s *settings.AppSettings, rows [][2]string) {
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

func writeFlowPlan(t *testing.T, s *settings.AppSettings) {
	t.Helper()
	p := dtplan.Plan{Meta: dtplan.Meta{
		Day: flowDay.Format(DateLayout), PrevDay: "2026-09-22", Positions: 3, BudgetPerOrder: "670000",
		CreatedAt: "2026-09-22T11:30:00Z",
	}}
	for _, c := range flowUniverse() {
		vol := c.vol
		p.Candidates = append(p.Candidates, universe.Candidate{
			Code: c.symbol + "0", Symbol: c.symbol, Name: "銘柄" + c.symbol, Segment: "prime",
			PrevClose: c.prevClose, TurnoverMed: 5e8, MktCap: 1e11, Vol20: &vol, CapTercile: 2,
			Shortable: c.short, Sector: c.sector, Eligible: c.long, ShortEligible: c.short,
		})
	}
	for _, c := range p.Candidates {
		if c.Eligible {
			p.Meta.Eligible++
		}
		if c.ShortEligible {
			p.Meta.ShortEligible++
		}
	}
	p.Meta.Candidates = len(p.Candidates)
	if _, _, err := dtplan.Save(p, s.DaytradeDir()); err != nil {
		t.Fatal(err)
	}
}

// writeFlowUs は米国市場のキャッシュ。usRet が nil なら前夜（9/23）の行を入れない（古いキャッシュ）。
func writeFlowUs(t *testing.T, s *settings.AppSettings, usRet *float64) {
	t.Helper()
	rows := []map[string]any{{"date": "2026-09-21", "spx": 6000.0, "vix": 15.0}, {"date": "2026-09-22", "spx": 6000.0, "vix": 15.0}}
	if usRet != nil {
		rows = append(rows, map[string]any{"date": "2026-09-23", "spx": 6000.0 * (1 + *usRet), "vix": 16.0})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.DataDir, "daytrade", "us.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFlowQuotes は気配の CSV（時刻の列は無し＝読んだ時刻）。
func writeFlowQuotes(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("symbol,price\n")
	for _, c := range flowUniverse() {
		if c.quote > 0 {
			fmt.Fprintf(&b, "%s,%g\n", c.symbol, c.quote)
		}
	}
	path := filepath.Join(root, "quotes.csv")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func orderRequestView(req domain.OrderRequest) map[string]any {
	view := map[string]any{
		"client_order_id": req.ClientOrderID, "symbol": req.Symbol, "side": req.Side,
		"quantity": req.Quantity.String(), "type": req.OrderType, "trade": req.Trade,
		"condition": req.Condition,
	}
	if req.LimitPrice != nil {
		view["limit_price"] = req.LimitPrice.String()
	}
	return view
}

// readLogs はこれまでの全実行のログ（JSONL）。時刻と run_id は落とす。
func readLogs(t *testing.T, s *settings.AppSettings) []string {
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
			out = append(out, dropKeys(rec, "ts_utc", "run_id"))
		}
	}
	return out
}

func readDigest(t *testing.T, s *settings.AppSettings, i int) []string {
	t.Helper()
	path := digest.Path(s.StateDir, string(s.Env), digest.DayOf(flowDay.Add(time.Hour)))
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{"<なし>"}
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if i >= len(lines) {
		return []string{"<なし>"}
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
		t.Fatal(err)
	}
	return []string{dropKeys(rec, "ts_utc", "run_id", "dur_ms")}
}

func readLedger(t *testing.T, s *settings.AppSettings) []string {
	t.Helper()
	if _, err := os.Stat(s.DaytradeDBPath()); err != nil {
		return []string{"<台帳なし>"}
	}
	led, err := dtledger.Open(s.DaytradeDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	orders, err := led.OrdersOn(flowDay, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range orders {
		out = append(out, normalizeJSON("", o))
	}
	return out
}

func readHistory(t *testing.T, s *settings.AppSettings, kind string) []string {
	t.Helper()
	store := dthistory.StoreFor(s)
	frame, err := store.Read(kind, history.Range{Start: flowDay, End: flowDay})
	if err != nil {
		return []string{"<なし> " + err.Error()}
	}
	var out []string
	for _, row := range frame.Rows {
		out = append(out, dropKeys(row, "run_id"))
	}
	return out
}

// dropKeys は map を鍵の順に JSON にする（落とす鍵を除く）。
func dropKeys(rec map[string]any, keys ...string) string {
	clean := make(map[string]any, len(rec))
	for k, v := range rec {
		clean[k] = v
	}
	for _, k := range keys {
		delete(clean, k)
	}
	return normalizeJSON("", clean)
}

func normalizeJSON(root string, v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<JSON にできない: %v>", err)
	}
	return normalize(root, string(raw))
}

// historyFileID は履歴のファイル名の末尾（実行ごとの乱数）。
var historyFileID = regexp.MustCompile(`-[0-9a-f]{12}\.parquet`)

// normalize は一時ディレクトリを <root> に、履歴のファイル名の乱数を <id> に置き換える。
func normalize(root, text string) string {
	if root != "" {
		text = strings.ReplaceAll(text, root, "<root>")
	}
	return historyFileID.ReplaceAllString(text, "-<id>.parquet")
}
