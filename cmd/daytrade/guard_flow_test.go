package main

// runGuard の流れのテスト（characterization test）。open_flow_test.go と同じ形。
//
// 設定・カレンダー・ニュースの記録簿・台帳（今日の建玉）を一時ディレクトリに作り、偽のブローカー
// （openBroker の差し替え）で runGuard を最後まで走らせる。出した注文・取消・照会・台帳・ログ・
// ダイジェスト・通知・端末の出力を 1 本の文書にして testdata/guard_flow/*.golden と突き合わせる。
// **今の挙動をそのまま固定する**もので、正しさの判定ではない。runGuard を分けるときに、この文書が
// 1 文字も変わらないことを確かめる。
//
// 時計は open と同じく読むたびに 1 ms 進む（flowClock）。elapsed_ms とログの並びが clock.Now の
// 呼び回数を映す。
//
// 期待値を作り直すとき: go test ./cmd/daytrade -run TestGuardFlow -update-guard-flow

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/news"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

var updateGuardFlow = flag.Bool("update-guard-flow", false, "runGuard の流れのテストの期待値（testdata/guard_flow）を書き直す")

// guardBroker は flowBroker に注文の照会・取消・当日の一覧の応答を足したもの。
type guardBroker struct {
	*flowBroker
	clock *flowClock
	// orders は照会（GetOrder）の応答。無い注文は nil（応答に該当なし）。
	orders map[string]*domain.Order
	// onGetOrder は照会のたびに呼ぶ（時計を進めて遅い応答を模す）。
	onGetOrder func(clientOrderID string)
	// onCancel は取消を受けた注文の、次の照会の応答を決める。nil なら取消完了（約定はそのまま）。
	onCancel func(o *domain.Order)
	// historyErr が非 nil なら当日の一覧の照会が失敗する。
	historyErr error
	queried    []string
}

func (g *guardBroker) GetOrder(clientOrderID string, _ *string) (*domain.Order, error) {
	g.queried = append(g.queried, clientOrderID)
	if g.onGetOrder != nil {
		g.onGetOrder(clientOrderID)
	}
	o, ok := g.orders[clientOrderID]
	if !ok || o == nil {
		return nil, nil
	}
	cp := *o
	return &cp, nil
}

func (g *guardBroker) Cancel(clientOrderID string, id *string) error {
	_ = g.flowBroker.Cancel(clientOrderID, id)
	if o, ok := g.orders[clientOrderID]; ok && o != nil {
		if g.onCancel != nil {
			g.onCancel(o)
		} else {
			o.Status = domain.OrderStatusCancelled
		}
	}
	return nil
}

func (g *guardBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	if g.historyErr != nil {
		return nil, g.historyErr
	}
	return g.flowBroker.GetOrderHistory(start, end)
}

// guardKnobs は設定の中でケースごとに変える項目。
type guardKnobs struct {
	marginDisabled bool // margin.enabled = false
	noCancel       bool // margin.cancel_on_corp_event = false
}

func guardConfig(k guardKnobs) string {
	return fmt.Sprintf(`[capital]
enabled = true
max_capital = 2000000
order_budget = 670000
max_positions = 10
weighting = "inverse_vol"
max_order = 1000000

[execution]
broker = "tachibana"
quote_source = "tachibana"
entry_window = ["08:59", "09:15"]
exit_window = ["15:20", "15:30"]
guard_window = ["09:00", "15:19"]
max_quote_age = 90
max_run_seconds = 150

[margin]
enabled = %v
max_capital = 2000000
order_budget = 670000
max_positions = 10
weighting = "inverse_vol"
exclude_corp_events = true
cancel_on_corp_event = %v
corp_event_lookback_days = 120
corp_event_max_staleness_minutes = 90
min_gap = 0.05
max_gap = 1.0
multiplier_normal = 1.0
multiplier_long_weak = 1.0
max_order = 1000000
long_via_margin = true
`, !k.marginDisabled, !k.noCancel)
}

// guardSeed は台帳に置く今日の注文 1 件（9:00:01 に送った形）。
type guardSeed struct {
	id, symbol string
	side       domain.Side
	trade      domain.TradeType
	quantity   int64
	status     domain.OrderStatus
	filled     int64
	price      float64 // 約定の平均値（filled > 0 のとき）
}

func shortEntry(symbol string, quantity int64, status domain.OrderStatus, filled int64) guardSeed {
	return guardSeed{id: "E-" + symbol, symbol: symbol, side: domain.SideSell, trade: domain.TradeTypeMarginOpen,
		quantity: quantity, status: status, filled: filled, price: 1300}
}

func longEntry(symbol string, quantity int64) guardSeed {
	return guardSeed{id: "E-" + symbol, symbol: symbol, side: domain.SideBuy, trade: domain.TradeTypeMarginOpen,
		quantity: quantity, status: domain.OrderStatusFilled, filled: quantity, price: 980}
}

// guardNews はニュースの記録簿の状態。
type guardNews struct {
	missing bool // 記録簿のファイルが無い
	// fetchedAt は今日の最後の取り込み（JST）。ゼロなら 9:50。TOB の記事もこの時刻に入る。
	fetchedAt time.Time
	// tob は TOB（賛同の意見表明）の出た銘柄。
	tob []string
}

type guardCase struct {
	name     string
	knobs    guardKnobs
	calendar [][2]string
	env      settings.Environment
	news     guardNews
	seeds    []guardSeed
	runs     []guardRun
}

type guardRun struct {
	at                 time.Time
	live, yes, ignoreW bool
	date               string
	// orders は照会の応答（client_order_id → 注文）。台帳の既定から作ったものを上書きする。
	orders     func(led map[string]*domain.Order)
	onGetOrder func(c *flowClock, clientOrderID string)
	onCancel   func(o *domain.Order)
	place      func(domain.OrderRequest) (*domain.OrderAck, error)
	historyErr error
	connectErr error
}

func guardLive(at time.Time) guardRun { return guardRun{at: at, live: true, yes: true} }

func TestGuardFlow(t *testing.T) {
	tob := guardNews{tob: []string{"2001"}}
	shorts := []guardSeed{
		longEntry("1001", 600),
		shortEntry("2001", 500, domain.OrderStatusFilled, 500),
		shortEntry("2002", 700, domain.OrderStatusFilled, 700),
	}
	cases := []guardCase{
		{
			// 全部約定した売建に TOB: 成行で返済買いし、通知する
			name: "filled_return", calendar: tradingCalendar(), env: settings.EnvProd, news: tob, seeds: shorts,
			runs: []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			// 再実行: 1 回目で返済 → 2 回目は台帳だけで「処置済み」と判定し、接続しない
			name: "rerun", calendar: tradingCalendar(), env: settings.EnvProd, news: tob, seeds: shorts,
			runs: []guardRun{guardLive(jstAt(10, 0, 0, 0)), guardLive(jstAt(10, 10, 0, 0))},
		},
		{
			// 未確定の売建（一部約定）: 取消を送り、確定した約定分を返済買い。
			// 取消の後の照会は締め切り（15:19）までしか待たないので、締め切りの直前に回す
			name: "open_cancel", calendar: tradingCalendar(), env: settings.EnvProd,
			news: guardNews{tob: []string{"2001"}, fetchedAt: jstAt(15, 10, 0, 0)},
			seeds: []guardSeed{
				longEntry("1001", 600),
				shortEntry("2001", 500, domain.OrderStatusPartiallyFilled, 200),
			},
			runs: []guardRun{{
				at: jstAt(15, 18, 59, 700), live: true, yes: true,
				onCancel: func(o *domain.Order) {
					o.Status = domain.OrderStatusCancelled
					o.FilledQuantity = decimal.NewFromInt(300)
				},
			}},
		},
		{
			// 未約定の売建: 取消だけ（約定なし）。取消の後も照会で終わりを確かめられないと失敗
			name: "open_cancel_unconfirmed", calendar: tradingCalendar(), env: settings.EnvProd,
			news: guardNews{tob: []string{"2001", "2002"}, fetchedAt: jstAt(15, 10, 0, 0)},
			seeds: []guardSeed{
				shortEntry("2001", 500, domain.OrderStatusSubmitted, 0),
				shortEntry("2002", 700, domain.OrderStatusSubmitted, 0),
			},
			runs: []guardRun{{
				at: jstAt(15, 18, 59, 700), live: true, yes: true,
				onCancel: func(o *domain.Order) {
					if o.Symbol == "2002" {
						return // 取消中のまま
					}
					o.Status = domain.OrderStatusCancelled
				},
			}},
		},
		{
			// 売建を照会できない（応答に該当なし）: 取消も返済もせず失敗として知らせる
			name: "query_unknown", calendar: tradingCalendar(), env: settings.EnvProd, news: tob,
			seeds: []guardSeed{shortEntry("2001", 500, domain.OrderStatusSubmitted, 0)},
			runs: []guardRun{{
				at: jstAt(10, 0, 0, 0), live: true, yes: true,
				orders: func(m map[string]*domain.Order) { delete(m, "E-2001") },
			}},
		},
		{
			// 照会の間に締め切りを過ぎた: 取消を送らない
			name: "deadline", calendar: tradingCalendar(), env: settings.EnvProd, news: tob,
			seeds: []guardSeed{shortEntry("2001", 500, domain.OrderStatusSubmitted, 0)},
			runs: []guardRun{{
				at: jstAt(10, 0, 0, 0), live: true, yes: true,
				onGetOrder: func(c *flowClock, _ string) { c.set(c.Now().Add(200 * time.Second)) },
			}},
		},
		{
			// 返済の買いが拒否された: 失敗として知らせ、エラーで終わる
			name: "return_rejected", calendar: tradingCalendar(), env: settings.EnvProd, news: tob, seeds: shorts,
			runs: []guardRun{{
				at: jstAt(10, 0, 0, 0), live: true, yes: true,
				place: func(domain.OrderRequest) (*domain.OrderAck, error) {
					return nil, &broker.OrderRejectedError{Message: "建玉不足"}
				},
			}},
		},
		{
			// 送信結果不明の注文を判定できない（当日の一覧を照会できない）: ログに残して処置は続ける
			name: "pending_unresolved", calendar: tradingCalendar(), env: settings.EnvProd, news: tob,
			seeds: []guardSeed{
				{id: "E-1002", symbol: "1002", side: domain.SideBuy, trade: domain.TradeTypeMarginOpen,
					quantity: 300, status: domain.OrderStatusPending},
				shortEntry("2001", 500, domain.OrderStatusFilled, 500),
			},
			runs: []guardRun{{at: jstAt(10, 0, 0, 0), live: true, yes: true, historyErr: errors.New("read tcp: i/o timeout")}},
		},
		{
			// 証券会社に接続できない
			name: "connect_error", calendar: tradingCalendar(), env: settings.EnvProd, news: tob, seeds: shorts,
			runs: []guardRun{{at: jstAt(10, 0, 0, 0), live: true, yes: true, connectErr: errors.New("ログインに失敗")}},
		},
		{
			// dry-run: 何を送るかを示すだけ（台帳もブローカーも触らない）
			name: "dry_run", calendar: tradingCalendar(), env: settings.EnvProd,
			news: guardNews{tob: []string{"2001", "2002"}},
			seeds: []guardSeed{
				shortEntry("2001", 500, domain.OrderStatusFilled, 500),
				shortEntry("2002", 700, domain.OrderStatusSubmitted, 0),
			},
			runs: []guardRun{{at: jstAt(10, 0, 0, 0)}},
		},
		{
			// テスト口座（uat）の --live は送らない（dry-run と同じ経路）
			name: "uat_live", calendar: tradingCalendar(), env: settings.Environment("uat"), news: tob, seeds: shorts,
			runs: []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			// 材料の出た売建が無い
			name: "no_marks", calendar: tradingCalendar(), env: settings.EnvProd, news: guardNews{tob: []string{"1001"}},
			seeds: shorts,
			runs:  []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			// 記録簿が古い（最後の取り込みが 2 時間前）: 知らせたうえで手元の分で点検する
			name: "news_stale", calendar: tradingCalendar(), env: settings.EnvProd,
			news: guardNews{tob: []string{"2001"}, fetchedAt: jstAt(8, 0, 0, 0)}, seeds: shorts,
			runs: []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			// 記録簿が無い: 点検できない（エラー）
			name: "news_missing", calendar: tradingCalendar(), env: settings.EnvProd,
			news: guardNews{missing: true}, seeds: shorts,
			runs: []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			// 今日の売建が無い（ロングと、約定なしで終わった売建だけ）: 記録簿にもブローカーにも触らない
			name: "no_shorts", calendar: tradingCalendar(), env: settings.EnvProd, news: tob,
			seeds: []guardSeed{longEntry("1001", 600), shortEntry("2001", 500, domain.OrderStatusRejected, 0)},
			runs:  []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			// 時間帯の外（15:19 以降）は送らない。--ignore-window なら時間帯の外でも回す
			name: "window", calendar: tradingCalendar(), env: settings.EnvProd,
			news: guardNews{tob: []string{"2001"}, fetchedAt: jstAt(15, 10, 0, 0)}, seeds: shorts,
			runs: []guardRun{
				guardLive(jstAt(15, 19, 0, 0)),
				{at: jstAt(15, 19, 30, 0), live: true, yes: true, ignoreW: true},
			},
		},
		{
			// 信用売りの脚が無効・cancel_on_corp_event が無効
			name: "disabled", knobs: guardKnobs{noCancel: true}, calendar: tradingCalendar(), env: settings.EnvProd,
			news: tob, seeds: shorts, runs: []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			name: "margin_disabled", knobs: guardKnobs{marginDisabled: true}, calendar: tradingCalendar(), env: settings.EnvProd,
			news: tob, seeds: shorts, runs: []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			name: "holiday", calendar: [][2]string{{"2026-09-22", "1"}, {"2026-09-24", "0"}, {"2026-09-25", "1"}},
			env: settings.EnvProd, news: tob, seeds: shorts, runs: []guardRun{guardLive(jstAt(10, 0, 0, 0))},
		},
		{
			// カレンダーが空: live は見送り、dry-run は平日で代用して続ける
			name: "calendar_empty", calendar: nil, env: settings.EnvProd, news: tob, seeds: shorts,
			runs: []guardRun{guardLive(jstAt(10, 0, 0, 0)), {at: jstAt(10, 1, 0, 0)}},
		},
		{
			// --date の書き間違い
			name: "bad_date", calendar: tradingCalendar(), env: settings.EnvProd, news: tob, seeds: shorts,
			runs: []guardRun{{at: jstAt(10, 0, 0, 0), live: true, yes: true, date: "2026/09/24"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runGuardCase(t, c)
			path := filepath.Join("testdata", "guard_flow", c.name+".golden")
			if *updateGuardFlow {
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
				t.Fatalf("期待値がありません（-update-guard-flow で作る）: %v", err)
			}
			if got != string(want) {
				t.Errorf("runGuard の流れが変わった（%s）:\n%s", path, firstDiff(string(want), got))
			}
		})
	}
}

// runGuardCase は 1 ケースを走らせ、観測したものを文書にして返す。
func runGuardCase(t *testing.T, c guardCase) string {
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
	if err := os.WriteFile(filepath.Join(configDir, dtconfig.Filename), []byte(guardConfig(c.knobs)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFlowCalendar(t, s, c.calendar)
	writeGuardNews(t, s, c.news)

	savedSettings, savedConfigDir, savedRun, savedNow, savedBroker := appSettings, configDirFlag, run, clock.Now, openBroker
	t.Cleanup(func() {
		appSettings, configDirFlag, run, clock.Now, openBroker = savedSettings, savedConfigDir, savedRun, savedNow, savedBroker
		digest.Reset()
	})
	appSettings, configDirFlag = s, configDir
	fake := &flowClock{}
	clock.Now = fake.Now
	fake.set(jstAt(9, 0, 1, 0))
	orders := writeGuardLedger(t, s, c.seeds)

	gb := &guardBroker{flowBroker: &flowBroker{balance: domain.Balance{
		BuyingPower: decimal.NewFromInt(50_000_000), MarginBuyingPower: ptrDecimal(decimal.NewFromInt(50_000_000)),
	}}, clock: fake, orders: orders}
	var doc logDoc
	for i, r := range c.runs {
		fake.set(r.at)
		gb.place, gb.onCancel, gb.historyErr = r.place, r.onCancel, r.historyErr
		gb.onGetOrder = nil
		if r.onGetOrder != nil {
			hook := r.onGetOrder
			gb.onGetOrder = func(id string) { hook(fake, id) }
		}
		if r.orders != nil {
			r.orders(gb.orders)
		}
		gb.queried = nil
		connects := 0
		openBroker = func(cfg dtconfig.Config) (broker.Broker, error) {
			connects++
			if r.connectErr != nil {
				return nil, r.connectErr
			}
			if cfg.Execution.Broker != "tachibana" {
				return nil, fmt.Errorf("未知の broker: %q", cfg.Execution.Broker)
			}
			return gb, nil
		}
		placedBefore := len(gb.placed)
		logsBefore := len(readLogs(t, s))

		var alerts []string
		stdout, err := captureStdout(t, func() error {
			run = cli.StartRun("daytrade", s, "guard")
			run.Alerter = func(title, body string, _ *logging.Logger) bool {
				alerts = append(alerts, title+" | "+body)
				return true
			}
			err := crash("材料の出た売建の処置", "daytrade.crash", runGuard(r.live, r.yes, r.ignoreW, r.date))
			run.Finish(err)
			return err
		})

		doc.printf("=== 実行 %d: %s JST live=%v yes=%v ignore_window=%v date=%q env=%s\n", i+1,
			r.at.In(clock.Tokyo).Format("15:04:05.000"), r.live, r.yes, r.ignoreW, r.date, c.env)
		doc.printf("--- 戻り値\n%v\n", normalize(root, errText(err)))
		doc.printf("--- ブローカーへの接続: %d 回\n", connects)
		doc.printf("--- 照会\n")
		for _, id := range gb.queried {
			doc.printf("%s\n", id)
		}
		doc.printf("--- 出した注文\n")
		for _, req := range gb.placed[placedBefore:] {
			doc.printf("%s\n", normalizeJSON(root, orderRequestView(req)))
		}
		doc.printf("--- 取消\n")
		for _, id := range gb.cancelled {
			doc.printf("%s\n", id)
		}
		gb.cancelled = nil
		doc.printf("--- 通知\n")
		for _, a := range alerts {
			doc.printf("%s\n", normalize(root, a))
		}
		doc.printf("--- 端末\n%s", normalize(root, stdout))
		doc.printf("--- ログ\n")
		for _, line := range readLogs(t, s)[logsBefore:] {
			doc.printf("%s\n", normalize(root, line))
		}
		doc.printf("--- ダイジェスト\n")
		for _, line := range readDigest(t, s, i) {
			doc.printf("%s\n", normalize(root, line))
		}
	}
	doc.printf("=== 最後の状態\n--- 台帳\n")
	for _, line := range readLedger(t, s) {
		doc.printf("%s\n", normalize(root, line))
	}
	return doc.String()
}

// logDoc は観測の文書。
type logDoc struct{ b []byte }

func (d *logDoc) printf(format string, a ...any) { d.b = fmt.Appendf(d.b, format, a...) }
func (d *logDoc) String() string                 { return string(d.b) }

// writeGuardLedger は今日の注文を台帳に置き、照会の既定の応答（台帳と同じ状態）を返す。
func writeGuardLedger(t *testing.T, s *settings.AppSettings, seeds []guardSeed) map[string]*domain.Order {
	t.Helper()
	orders := map[string]*domain.Order{}
	if len(seeds) == 0 {
		return orders
	}
	led, err := dtledger.Open(s.DaytradeDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	for _, seed := range seeds {
		brokerID := "N/" + seed.symbol
		req := domain.OrderRequest{
			ClientOrderID: seed.id, Symbol: seed.symbol, Side: seed.side, Quantity: decimal.NewFromInt(seed.quantity),
			OrderType: domain.OrderTypeMarket, Trade: seed.trade, Reason: "seed",
		}
		var bid *string
		if seed.status != domain.OrderStatusPending {
			bid = &brokerID
		}
		if err := led.Record(req, flowDay, string(domain.OrderStatusSubmitted), nil, bid); err != nil {
			t.Fatal(err)
		}
		filled := decimal.NewFromInt(seed.filled)
		var avg *decimal.Decimal
		if seed.filled > 0 {
			avg = ptrDecimal(decimal.NewFromFloat(seed.price))
		}
		if err := led.UpdateStatus(seed.id, seed.status, filled, avg, bid); err != nil {
			t.Fatal(err)
		}
		orders[seed.id] = &domain.Order{
			ClientOrderID: seed.id, BrokerOrderID: bid, Symbol: seed.symbol, Side: seed.side,
			Quantity: req.Quantity, FilledQuantity: filled, AvgFillPrice: avg, Status: seed.status, Trade: seed.trade,
		}
	}
	return orders
}

// writeGuardNews はニュースの記録簿。前の 4 日は日が明けてから取り込み済み、今日は fetchedAt に取り込んだ形。
func writeGuardNews(t *testing.T, s *settings.AppSettings, n guardNews) {
	t.Helper()
	if n.missing {
		return
	}
	path := s.NewsDBPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := news.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	for d := 4; d >= 1; d-- {
		day := flowDay.AddDate(0, 0, -d)
		at := time.Date(day.Year(), day.Month(), day.Day()+1, 6, 20, 0, 0, clock.Tokyo)
		if _, err := store.Save(ctx, day.Format(DateLayout), nil, at); err != nil {
			t.Fatal(err)
		}
	}
	fetched := n.fetchedAt
	if fetched.IsZero() {
		fetched = jstAt(9, 50, 0, 0)
	}
	var items []broker.NewsItem
	for i, symbol := range n.tob {
		items = append(items, broker.NewsItem{
			ID: fmt.Sprintf("T%d", i+1), Time: "0930", Genres: []string{news.GenreTDnet}, Codes: []string{symbol},
			Headline: "株式会社K891による当社株券等に対する公開買付けに関する賛同の意見表明",
		})
	}
	if _, err := store.Save(ctx, flowDay.Format(DateLayout), items, fetched); err != nil {
		t.Fatal(err)
	}
}
