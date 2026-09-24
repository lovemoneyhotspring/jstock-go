package execute

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	accumhist "github.com/lovemoneyhotspring/jstock-go/pkg/accum/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbhistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/reconcile"
	"github.com/shopspring/decimal"
)

// alert は運用通知（テストで差し替える）。
var alert = notify.Alert

type PlannedOrder struct {
	Symbol     string
	Amount     decimal.Decimal
	Quantity   decimal.Decimal
	LimitPrice *decimal.Decimal
	Request    *domain.OrderRequest
	Reason     string
	Note       string
	// Failed は見送りのうち「発注すべきなのに出せなかった」もの（足が無い・古い・判定用の足が
	// 読めない・注文を組み立てられない）。時間帯の外や単元未満のような正常な見送りとは分け、
	// run の最後に通知・ダイジェストの失敗・非 0 終了にする（A6）。
	Failed bool
	// LotUnknown は売買単位が分からない（lot_size_overrides にも銘柄情報にも無い）失敗。
	// dry-run（PaperBroker）は銘柄マスタを持たないので、RunAccumulation は dry-run に限り
	// これを失敗から外して見送りにする。
	LotUnknown bool

	// 以下は判断履歴に残すための材料。発注に至らなかった日
	// （時間帯の外・単元未満）も残さないと、後から「倍率の付け方は
	// 効いていたのか」を確かめられない。
	Market     domain.Market
	JudgedOn   string // 判断に使った足の日付
	Month      string // どの月の積立か（月初）
	Close      decimal.Decimal
	Target     decimal.Decimal
	Placed     decimal.Decimal
	Multiplier float64
	Tactic     string
}

// BrokerSymbol は足の表記を発注・台帳の表記にする（accumcfg.BrokerSymbol と同じ）。
func BrokerSymbol(symbol string) string { return accumcfg.BrokerSymbol(symbol) }

// PlanOrders は最新の足データと台帳情報から本日の発注計画を立てる。
//
// markStart が真（本発注の実行）なら、開始日の記録が無い銘柄に今日を開始日として
// 台帳へ残す（開始月の日割りの起点。dry-run は残さず、今日を開始日とみなして計算だけする）。
//
// 台帳が読めない・書けないときはエラーを返す。発注済み額を 0 と読むと同じ月の予算を
// もう一度買うので、計画そのものを立てない。
//
// brokerLots はブローカーの銘柄情報から売買単位を引く関数（発注の表記がキー。nil 可）。
// 注文を作る段（今日出す差額が立った銘柄）に初めて来たときに 1 回だけ呼ぶ。今月分を
// 出し終えた日・時間帯の外・持ち越しの日は銘柄マスタを引かない。
//
// 売買単位は lot_size_overrides と brokerLots から決める（lotSizeFor）。どちらにも無い、
// または両方あって値が違う銘柄は既定の 100 株に倒さず、発注できなかった銘柄（Failed）にする。
func PlanOrders(
	cfg *accumcfg.AccumConfig,
	barStore *data.BarStore,
	led *ledger.Ledger,
	now time.Time,
	ignoreWindow bool,
	markStart bool,
	brokerLots func() map[string]decimal.Decimal,
) (orders []PlannedOrder, staleSignals []string, err error) {
	pc := newPlanContext(cfg, barStore, led, now, ignoreWindow, markStart, brokerLots)

	for _, entry := range cfg.Tactics {
		if !entry.IsEnabled() {
			continue
		}

		// 設定に書かれたパラメータ（倍率・段表・発注時間帯）をそのまま反映する。
		tactic, err := entry.Build()
		if err != nil {
			return nil, nil, err
		}

		signalBars, signalProblem := loadSignalBars(barStore, &entry)

		for _, sym := range entry.Symbols {
			if signalProblem != "" {
				orders = append(orders, PlannedOrder{Symbol: BrokerSymbol(sym), Note: signalProblem, Failed: true})
				continue
			}
			po, staleSignal, err := pc.planSymbol(&entry, tactic, sym, signalBars)
			if err != nil {
				return nil, nil, err
			}
			if staleSignal != "" {
				staleSignals = append(staleSignals, staleSignal)
			}
			if po != nil {
				orders = append(orders, *po)
			}
		}
	}

	return orders, staleSignals, nil
}

// planContext は PlanOrders の 1 回の呼び出しで銘柄をまたいで共通の材料。
type planContext struct {
	cfg          *accumcfg.AccumConfig
	barStore     *data.BarStore
	led          *ledger.Ledger
	now          time.Time
	ignoreWindow bool
	markStart    bool
	todayJST     string    // 今日（東京）。これより前の足だけを確定足とみなす
	monthStart   string    // 今月の月初（YYYY-MM-DD）
	monthTime    time.Time // monthStart を日付にしたもの
	// lots は銘柄マスタの売買単位。注文を作る段に初めて来たときに 1 回だけ引く
	lots func() map[string]decimal.Decimal
}

func newPlanContext(
	cfg *accumcfg.AccumConfig,
	barStore *data.BarStore,
	led *ledger.Ledger,
	now time.Time,
	ignoreWindow bool,
	markStart bool,
	brokerLots func() map[string]decimal.Decimal,
) *planContext {
	var lots map[string]decimal.Decimal
	lotsLoaded := false
	lotsOnce := func() map[string]decimal.Decimal {
		if !lotsLoaded {
			lotsLoaded = true
			if brokerLots != nil {
				lots = brokerLots()
			}
		}
		return lots
	}

	todayJST := clock.ToZone(now, clock.Tokyo).Format("2006-01-02")
	monthStart := todayJST[:7] + "-01"
	monthTime, _ := time.Parse("2006-01-02", monthStart)
	return &planContext{
		cfg: cfg, barStore: barStore, led: led, now: now,
		ignoreWindow: ignoreWindow, markStart: markStart,
		todayJST: todayJST, monthStart: monthStart, monthTime: monthTime,
		lots: lotsOnce,
	}
}

// loadSignalBars は戦略の判定用の足を読む。読めない・無いときは見送りの理由を返す。
//
// 判定用の足（^IXIC など）が読めないとき、黙って買う銘柄自身の足で判定しない
// （BuildPlanWithSignal は判定用が空なら自身の足に倒れる）。上場の浅い ETF は自身の足に
// 200 日線が揃わず、別物の判定で買うことになる。その戦略の銘柄は見送って知らせる（A7）。
func loadSignalBars(barStore *data.BarStore, entry *accumcfg.TacticEntry) (signalBars []domain.Bar, problem string) {
	if entry.SignalSymbol == "" {
		return nil, ""
	}
	bars, rerr := barStore.Read(entry.SignalSymbol, "", "")
	switch {
	case rerr != nil:
		return nil, fmt.Sprintf("判定用の足（%s）を読めないため見送り: %v", entry.SignalSymbol, rerr)
	case len(bars) == 0:
		return nil, fmt.Sprintf("判定用の足（%s）が無いため見送り", entry.SignalSymbol)
	}
	return bars, ""
}

// barsBefore は day より前の足（確定足）。1 本も無ければ nil。
func barsBefore(bars []domain.Bar, day string) []domain.Bar {
	var out []domain.Bar
	for _, b := range bars {
		if b.Date < day {
			out = append(out, b)
		}
	}
	return out
}

// planSymbol は 1 銘柄の今日の計画を立てる。
//
// 計画の行を立てない日（確定足が無い・今月の確定足が無い・今月分を発注済み）は nil を返す。
// staleSignal は判定用の足が古いときの警告（空なら無し）。
func (pc *planContext) planSymbol(
	entry *accumcfg.TacticEntry,
	tactic tactics.Tactic,
	sym string,
	signalBars []domain.Bar,
) (po *PlannedOrder, staleSignal string, err error) {
	bars, err := pc.barStore.Read(sym, "", "")
	if err != nil || len(bars) == 0 {
		note := "足データなし"
		if err != nil {
			note = fmt.Sprintf("足データを読めない: %v", err)
		}
		return &PlannedOrder{
			Symbol: BrokerSymbol(sym),
			Note:   note,
			Failed: true,
		}, "", nil
	}

	// 確定足（前日以前）で計画を計算
	completed := barsBefore(bars, pc.todayJST)
	if len(completed) == 0 {
		return nil, "", nil
	}

	completedSignal := barsBefore(signalBars, pc.todayJST)
	if entry.SignalSymbol != "" && len(completedSignal) == 0 {
		return &PlannedOrder{
			Symbol: BrokerSymbol(sym),
			Note:   fmt.Sprintf("判定用の足（%s）に確定足が無いため見送り", entry.SignalSymbol),
			Failed: true,
		}, "", nil
	}

	// 最終足が古すぎるなら判定しない。
	//
	// 取得元が止まっているのに気付かず、古い配列のまま増額判定を
	// 続けるのを防ぐ。判定用の銘柄が古い場合は投下を止めず警告に留める
	// （買う銘柄の足は新しいので、判定だけが前日基準になる）。
	stale, age := isStale(completed, pc.todayJST, pc.cfg.Execution.MaxStaleDays)
	if stale {
		return &PlannedOrder{
			Symbol: BrokerSymbol(sym),
			Note: fmt.Sprintf("足が %d 日前（%s）で古いため見送り（max_stale_days=%d）",
				age, completed[len(completed)-1].Date, pc.cfg.Execution.MaxStaleDays),
			Failed: true,
		}, "", nil
	}
	if len(completedSignal) > 0 {
		if staleSig, sigAge := isStale(completedSignal, pc.todayJST, pc.cfg.Execution.MaxStaleDays); staleSig {
			staleSignal = fmt.Sprintf("%s（%s, %d日前）", entry.SignalSymbol,
				completedSignal[len(completedSignal)-1].Date, sigAge)
		}
	}

	// 発注時間帯の外なら注文は作らない。
	if !pc.ignoreWindow && !tactic.AllowsOrder(pc.now) {
		return &PlannedOrder{
			Symbol: BrokerSymbol(sym),
			Note:   fmt.Sprintf("発注時間帯の外（%s）", tactic.Window().Describe()),
		}, staleSignal, nil
	}

	p, err := plan.BuildPlanWithSignal(completed, completedSignal, entry.SignalLags(), tactic, entry.MonthlyBudget)
	if err != nil {
		return &PlannedOrder{
			Symbol: BrokerSymbol(sym),
			Note:   fmt.Sprintf("計画を立てられないため見送り: %v", err),
			Failed: true,
		}, staleSignal, nil
	}
	if len(p.Rows) == 0 {
		return nil, staleSignal, nil
	}

	po, err = pc.planMonth(entry, BrokerSymbol(sym), p.Rows, completed[len(completed)-1])
	return po, staleSignal, err
}

// planMonth は今月の計画行と台帳から差額を出し、今日の注文（または見送り）にする。
func (pc *planContext) planMonth(
	entry *accumcfg.TacticEntry,
	bSym string,
	rows []plan.PlanRow,
	lastBar domain.Bar,
) (*PlannedOrder, error) {
	// 今月ぶんの計画行だけを取り出す。
	var thisMonth []plan.PlanRow
	for _, r := range rows {
		if strings.HasPrefix(r.Date, pc.todayJST[:7]) {
			thisMonth = append(thisMonth, r)
		}
	}
	if len(thisMonth) == 0 {
		return nil, nil // 今月の確定足がまだ無い（月初の初日）
	}

	started, err := pc.startedOn(bSym)
	if err != nil {
		return nil, err
	}
	m, err := pc.monthAmounts(rows, thisMonth, bSym, entry.MonthlyBudget, started)
	if err != nil {
		return nil, err
	}
	if m.due.LessThanOrEqual(decimal.Zero) {
		return nil, nil // 今月分は発注済み
	}

	lastRow := thisMonth[len(thisMonth)-1]
	decided := m.decided(entry, bSym, pc.monthStart, lastRow)

	// 端数や小さな予算増は、注文が出る日にまとめる。
	if !ShouldPlaceToday(thisMonth, m.due, m.base) {
		decided.Note = fmt.Sprintf("差額 %s円は次のリリース日に持ち越し", m.due.Round(0))
		return &decided, nil
	}

	reason := m.reason()

	// 単元株数（設定の上書きとブローカーの銘柄情報。どちらも無ければ見送りの失敗）
	lotSize, lotErr := lotSizeFor(pc.cfg, bSym, pc.lots())
	if lotErr != nil {
		decided.Reason = reason
		decided.Note = lotErr.Error()
		decided.Failed = true
		decided.LotUnknown = errors.Is(lotErr, errLotUnknown)
		return &decided, nil
	}

	snappedPrice, qty := limitAndQuantity(pc.cfg.Execution, m.due, lastBar.Close, lotSize)
	if qty.LessThanOrEqual(decimal.Zero) {
		decided.Reason = reason
		decided.Note = fmt.Sprintf("単元株数（%s株）に満たないため見送り", lotSize)
		return &decided, nil
	}

	req, err := newBuyRequest(pc.cfg.Execution, pc.todayJST, bSym, qty, snappedPrice, reason)
	if err != nil {
		// 黙って飛ばすと、その銘柄の積立が止まったことにログでも履歴でも気付けない
		return &PlannedOrder{
			Symbol: bSym,
			Amount: m.due,
			Reason: reason,
			Note:   fmt.Sprintf("注文を組み立てられないため見送り: %v", err),
			Failed: true,
		}, nil
	}

	decided.Quantity = qty
	decided.LimitPrice = &snappedPrice
	decided.Request = &req
	decided.Reason = req.Reason
	return &decided, nil
}

// startedOn は銘柄の積立の開始日。月の途中から始めた月は日割りにする。
//
// 開始日は銘柄ごとに「最初に本発注の run が計画を立てた日」。記録が無ければ今日を
// 開始日とみなし、本発注の run なら台帳に残す（2 回目以降は INSERT OR IGNORE で
// 変わらない）。記録する経路が無いと日割りは一度も効かず、月の途中から始めた銘柄に
// その月の満額を投じる（2026-09-24 のレビュー A4。Python 版 c2ef6b4 の意図）。
//
// 記録が無くても注文が既にある銘柄（記録する経路が無かった間に発注・取り込みした
// もの）は、最初の注文の日を開始日とする。今日にすると積立中の月を日割りしてしまう。
func (pc *planContext) startedOn(bSym string) (*time.Time, error) {
	startedOn, err := pc.led.StartedOn(bSym)
	if err != nil {
		return nil, err
	}
	if startedOn == nil {
		startedOn, err = pc.led.FirstOrderDay(bSym, clock.Tokyo)
		if err != nil {
			return nil, err
		}
		if startedOn == nil {
			today := pc.todayJST
			startedOn = &today
		}
		if pc.markStart {
			if err := pc.led.MarkStarted(bSym, *startedOn); err != nil {
				return nil, fmt.Errorf("%s の積立の開始日を台帳に書けません: %w", bSym, err)
			}
		}
	}
	startedDay, err := time.Parse("2006-01-02", *startedOn)
	if err != nil {
		return nil, fmt.Errorf("%s の積立の開始日 %q を読めません: %w", bSym, *startedOn, err)
	}
	return &startedDay, nil
}

// monthAmounts は今月の目標と発注済みの額（差額 due の内訳）。
type monthAmounts struct {
	base     decimal.Decimal // 今月の基本目標（開始月は日割り）
	extras   decimal.Decimal // 今月の増額
	prorated string          // 日割りの説明（日割りしない月は空）
	carried  decimal.Decimal // 前月からの繰り越し
	target   decimal.Decimal // base + extras
	already  decimal.Decimal // 今月の発注済み
	due      decimal.Decimal // target + carried - already
}

// monthAmounts は今月の計画行と台帳から今月の差額を出す。
func (pc *planContext) monthAmounts(
	rows, thisMonth []plan.PlanRow,
	bSym string,
	budget decimal.Decimal,
	started *time.Time,
) (monthAmounts, error) {
	var m monthAmounts
	m.base, m.extras, m.prorated = MonthTarget(thisMonth, budget, pc.monthTime, started)
	carried, err := CarryOver(rows, bSym, pc.monthTime, budget, started, pc.led.HasOrders, pc.led.PlacedAmount)
	if err != nil {
		return m, err
	}
	m.carried = carried
	m.target = m.base.Add(m.extras)
	m.already, err = pc.led.PlacedAmount(bSym, pc.monthTime)
	if err != nil {
		return m, err
	}
	m.due = m.target.Add(m.carried).Sub(m.already)
	return m, nil
}

// reason は注文の理由（今月の目標の内訳）。
func (m monthAmounts) reason() string {
	reason := fmt.Sprintf("今月の目標 %s（基本 %s", m.target.Round(0), m.base.Round(0))
	if m.prorated != "" {
		reason += fmt.Sprintf("〔%s〕", m.prorated)
	}
	reason += fmt.Sprintf("＋増額 %s）", m.extras.Round(0))
	if m.carried.IsPositive() {
		reason += fmt.Sprintf("＋前月からの繰り越し %s", m.carried.Round(0))
	}
	reason += fmt.Sprintf("− 発注済み %s", m.already.Round(0))
	return reason
}

// decided は判断履歴に残す材料を埋めた計画の行（注文・見送りの中身は呼び出し側で足す）。
func (m monthAmounts) decided(entry *accumcfg.TacticEntry, bSym, monthStart string, lastRow plan.PlanRow) PlannedOrder {
	return PlannedOrder{
		Symbol:     bSym,
		Amount:     m.due,
		Market:     entry.MarketResolved(),
		JudgedOn:   lastRow.Date,
		Month:      monthStart,
		Close:      lastRow.Close,
		Target:     m.target.Add(m.carried),
		Placed:     m.already,
		Multiplier: lastRow.Multiplier,
		Tactic:     entry.Tactic,
	}
}

// limitAndQuantity は指値（終値 × (1 + limit_offset) を呼値に乗せたもの）と、
// 差額 due をその指値で割って売買単位で切り捨てた株数。
func limitAndQuantity(exec accumcfg.ExecutionConfig, due, lastPrice, lotSize decimal.Decimal) (snappedPrice, qty decimal.Decimal) {
	// 指値価格の計算 (終値 * (1 + offset))
	offset := decimal.RequireFromString("0.01")
	if exec.LimitOffset != "" {
		if d, err := decimal.NewFromString(exec.LimitOffset); err == nil {
			offset = d
		}
	}

	limitPrice := lastPrice.Mul(decimal.NewFromInt(1).Add(offset))
	snappedPrice, _ = marketrules.SnapToTick(limitPrice, domain.SideBuy, false, marketrules.RoundingConservative)

	// 株数 = floor(due / snappedPrice) を lotSize で切り捨て
	rawQty := due.Div(snappedPrice).Floor()
	qty, _ = marketrules.RoundToLot(rawQty, lotSize)
	return snappedPrice, qty
}

// newBuyRequest は積立の買い注文を組み立てる。
func newBuyRequest(exec accumcfg.ExecutionConfig, todayJST, bSym string, qty, snappedPrice decimal.Decimal, reason string) (domain.OrderRequest, error) {
	// 成行に指値を付けると NewOrderRequest が弾く。株数は指値で見積もるが、注文には載せない
	orderType := domain.OrderTypeLimit
	requestPrice := &snappedPrice
	if exec.OrderType == "market" {
		orderType = domain.OrderTypeMarket
		requestPrice = nil
	}

	taxType := domain.TaxAccountSpecific
	if exec.TaxAccountType != "" {
		taxType = domain.TaxAccountType(exec.TaxAccountType)
	}

	orderID := domain.MakeClientOrderID(todayJST, bSym, domain.SideBuy, qty)
	return domain.NewOrderRequest(
		orderID,
		bSym,
		domain.SideBuy,
		orderType,
		qty,
		requestPrice,
		taxType,
		reason,
		domain.TradeTypeCash,
	)
}

// lotSizeFor は発注に使う売買単位を決める。
//
//   - 設定の上書き（lot_size_overrides）とブローカーの値が両方あって違う → エラー。どちらが
//     正しいか分からないまま丸めると、10 倍・1/10 の株数で出しうる
//   - どちらか一方だけ → その値
//   - どちらも無い → エラー。以前は既定の 100 株で丸め、1 株単位の ETF（2559・1629）が
//     銘柄マスタを取れなかった回に「単元未満で見送り」になり、失敗として通知されなかった
//     （2026-09-24 のレビュー）。dry-run（PaperBroker）は与えていない銘柄の値を持たないので、
//     上書きの無い銘柄は dry-run でもここでエラーになる（RunAccumulation が dry-run に限り見送りに直す）
func lotSizeFor(cfg *accumcfg.AccumConfig, symbol string, brokerLots map[string]decimal.Decimal) (decimal.Decimal, error) {
	ov, hasOverride := cfg.Execution.LotSizeFor(symbol)
	lot, hasBroker := brokerLots[symbol]
	hasBroker = hasBroker && lot.IsPositive()
	switch {
	case hasOverride && hasBroker:
		if !lot.Equal(decimal.NewFromInt(int64(ov))) {
			return decimal.Zero, fmt.Errorf(
				"売買単位が設定（lot_size_overrides の %d 株）とブローカーの銘柄情報（%s 株）で違うため発注しません"+
					"（どちらが正しいか確かめて設定を直す）", ov, lot)
		}
		return lot, nil
	case hasOverride:
		return decimal.NewFromInt(int64(ov)), nil
	case hasBroker:
		return lot, nil
	}
	return decimal.Zero, errLotUnknown
}

// errLotUnknown は売買単位が設定にも銘柄情報にも無い（lotSizeFor）。
var errLotUnknown = errors.New(
	"売買単位が分からないため発注しません（ブローカーの銘柄情報に無い・取得できない。" +
		"lot_size_overrides にも無い。既定の 100 株では丸めない）")

// dryRunLotNote は dry-run で売買単位が分からない銘柄の見送りの理由。
const dryRunLotNote = "売買単位が分からないため見送り（dry-run は銘柄マスタを引かない。" +
	"本発注ではブローカーの銘柄マスタを使う。lot_size_overrides に書けば dry-run でも株数を出す）"

// isStale は最終足が maxStaleDays より古いかと、その日数を返す。
func isStale(bars []domain.Bar, todayJST string, maxStaleDays int) (bool, int) {
	if len(bars) == 0 || maxStaleDays <= 0 {
		return false, 0
	}
	last, err := time.Parse("2006-01-02", bars[len(bars)-1].Date)
	if err != nil {
		return false, 0
	}
	today, err := time.Parse("2006-01-02", todayJST)
	if err != nil {
		return false, 0
	}
	age := int(today.Sub(last).Hours() / 24)
	return age > maxStaleDays, age
}

// RunAccumulation は積立の実行サイクル（照会 → 計画 → 発注/記録）を行う。
func RunAccumulation(
	cfg *accumcfg.AccumConfig,
	b broker.Broker,
	barStore *data.BarStore,
	led *ledger.Ledger,
	logger *logging.Logger,
	hist *wbhistory.Store,
	isLive bool,
	ignoreWindow bool,
) error {
	now := clock.NowUTC()
	todayJST := clock.ToZone(now, clock.Tokyo).Format("2006-01-02")
	monthStart := todayJST[:7] + "-01"

	// 0. 発注時間帯の外なら何もしない。
	//
	// 日足で決まった投下額は変わらないので、時間帯の外でも計画は同じ。
	// ここで止めるのは「いつ発注してよいか」だけの制御。
	if isLive && !ignoreWindow {
		allowed, windows := WindowState(cfg, now)
		if !allowed {
			logger.Info("accum.skip", fmt.Sprintf("発注時間帯の外（%s）。何もしません", windows))
			return nil
		}
	}

	// 1. 前回までに送った注文がどうなったかを先に確かめる。
	//
	// 失効・拒否なら「発注済み」から外れ、この後の差額の計算で自動的に
	// 埋め直される。照会できないときは前回の状態のまま先へ進む——
	// ここで止めると、ブローカー側の一時的な不調で積立が丸ごと飛ぶ。
	synced, err := SyncOrderStatus(led, b, now)
	if err != nil {
		logger.Warn("accum.sync_failed",
			fmt.Sprintf("注文の照会に失敗（前回の状態のまま続けます）: %v", err))
	}
	for _, change := range synced.Changes {
		logger.Info("accum.fill", "前回の注文: "+change.Describe())
	}
	// 送信結果不明（PENDING）の注文を当日の注文一覧で判定した結果はダイジェストに載せる
	//（AI が最初に読む。届いていた／届いていなかった／決められない、の件数）
	if r := synced.Resolved; r.Attributed+r.NotSent+r.Ambiguous+r.TooRecent > 0 {
		digest.Note(r.Fields("pending"))
	}
	for _, r := range synced.Resolutions {
		fields := r.Fields()
		if r.Outcome == reconcile.Ambiguous {
			fields["fix"] = strings.Replace(fields["fix"].(string), "<app>", "accum", 1)
			logger.Error("accum.pending_ambiguous", "送信結果不明の注文を決められない（PENDING のまま）", fields)
			continue
		}
		logger.Info("accum.pending_resolved", "送信結果不明の注文を判定: "+string(r.Outcome), fields)
	}
	// 照会できなかった注文は「発注済み」に数えたまま保留してある。ふつうは次の run が
	// もう一度判定するが、前日以前の送信結果不明（NeedsResolve）は `accum pending resolve` まで
	// 残る。どちらかを書き分けてダイジェストの異常に残す（AI が読む）。
	for _, u := range synced.Unresolved {
		logger.Warn("accum.unresolved", "照会できず保留: "+u.Describe())
	}
	if pending, other := describeHeld(synced.Unresolved); pending != "" || other != "" {
		if pending != "" {
			digest.Anomaly("accum.pending_ambiguous", pending)
		}
		if other != "" {
			digest.Anomaly("accum.unresolved", other)
		}
	}
	// 通知は最後にまとめて 1 通。保留した PENDING の銘柄に今日の注文が立つと、発注できなかった
	// 銘柄（OrdersFailedError）として同じ注文を知らせる。以前はそれとは別に「照会できません」を
	// 送り、1 件の PENDING で 2 通になっていた。失敗の行で知らせた注文はここから外す
	coveredByFailure := map[string]bool{}
	defer func() { alertHeld(synced.Unresolved, coveredByFailure, !isLive, logger) }()

	// 送信結果不明（PENDING）が残る銘柄は発注しない。
	//
	// 照会の後でも PENDING のままの注文は、届いたかどうかを決められなかったもの（前日以前の
	// 注文は立花の当日の注文一覧に出ない・一覧が空・候補が曖昧）。届いていたなら同じ額を
	// もう一度買うことになる。台帳を読み直して決める——照会が途中で失敗しても漏らさない。
	pendingBySymbol, err := led.PendingSymbols()
	if err != nil {
		logger.Error("accum.ledger_read_failed", err.Error())
		return fmt.Errorf("台帳を読めないため発注を中止しました: %w", err)
	}

	// 2. 本日の発注計画（本発注の run なら、開始日の無い銘柄に今日を開始日として残す）
	// 銘柄マスタ（立花は全銘柄が一括で返る）は、今日出す注文が立ったときだけ引く
	planned, staleSignals, err := PlanOrders(cfg, barStore, led, now, ignoreWindow, isLive,
		func() map[string]decimal.Decimal { return b.LotSizes(orderableSymbols(cfg)) })
	if err != nil {
		logger.Error("accum.plan_failed", err.Error())
		return fmt.Errorf("発注計画を立てられないため発注を中止しました: %w", err)
	}
	// dry-run は銘柄マスタを持たない（PaperBroker）。売買単位の不明を失敗にすると、
	// lot_size_overrides に無い銘柄（1629・2559）で毎回、失敗の通知と非 0 終了になる。
	// 本発注ではブローカーの銘柄マスタを引くので、dry-run に限り見送りにする
	if !isLive {
		for i := range planned {
			if planned[i].LotUnknown {
				planned[i].Failed = false
				planned[i].Note = dryRunLotNote
			}
		}
	}
	for _, s := range staleSignals {
		logger.Warn("accum.stale_signal",
			fmt.Sprintf("判定用の足が古いままです（前日以前の値で判定します）: %s", s))
	}

	// 判断の履歴を残す。発注に至らなかった日も含めて残さないと、
	// 後から「倍率の付け方は効いていたのか」を確かめられない。
	if hist != nil {
		if err := recordDecisions(hist, planned, now); err != nil {
			logger.Warn("accum.history_failed", fmt.Sprintf("判断履歴を残せませんでした: %v", err))
		}
	}

	// 3. 台帳に無い当月の約定がないか確かめる。
	//
	// 台帳を失った状態で走ると当月の予算をもう一度買う。ブローカー側にだけ
	// 約定がある注文を見つけたら、額の計算が信用できないので発注を止める。
	if isLive {
		var symbols []string
		for _, po := range planned {
			if po.Request != nil {
				symbols = append(symbols, po.Symbol)
			}
		}
		if len(symbols) > 0 {
			unrecorded, err := UnrecordedFills(led, b, symbols, now)
			if err != nil {
				// 照会できないなら二重買付を否定できない。安全側に倒して止める。
				logger.Error("accum.unrecorded_check_failed", err.Error())
				return fmt.Errorf("台帳とブローカーの突き合わせができないため発注を中止しました: %w", err)
			}
			if len(unrecorded) > 0 {
				var detail []string
				for sym, amt := range unrecorded {
					detail = append(detail, fmt.Sprintf("%s %s円", sym, amt.Round(0)))
				}
				sort.Strings(detail)
				msg := strings.Join(detail, "、")
				logger.Error("accum.unrecorded_fills",
					fmt.Sprintf("台帳に無い当月の約定があります（二重買付の恐れ）: %s", msg))
				return fmt.Errorf(
					"台帳に無い当月の約定があります（二重買付になります）: %s\n"+
						"台帳（%s）が失われているか、別の環境で発注した可能性があります。"+
						"状態を確かめてから実行してください", msg, led.Path())
			}
		}
	}

	// 4. 発注処理
	//
	// 出すべきなのに出せなかった銘柄（足が無い・判定用の足が読めない・見積り失敗・余力不足・
	// 拒否・送信結果不明が残る）は failures に集め、最後に OrdersFailedError で返す。
	// 以前はログの warn に留まり、通知にもダイジェストにも出ず終了コード 0 だった（A6）。
	var failures []string
	fail := func(symbol, detail string) {
		line := fmt.Sprintf("%s: %s", symbol, detail)
		failures = append(failures, line)
		logger.Error("accum.order_failed", line)
	}

	// 余力が分からないまま「余力不足」と記録すると、照会の失敗が資金の不足に化けて
	// 切り分けられない。照会できない回は発注せず、理由をそのまま残す
	bal, err := b.GetBalance()
	if err != nil || bal == nil {
		if err == nil {
			err = errors.New("応答が空")
		}
		logger.Warn("accum.balance_failed",
			fmt.Sprintf("買付余力を照会できないため、この回は発注しません: %v", err))
		for _, po := range planned {
			switch {
			case po.Failed:
				fail(po.Symbol, po.Note)
			case po.Request != nil && len(pendingBySymbol[po.Symbol]) > 0:
				// 送信結果不明の注文はこの失敗の行で知らせる（下の本流と同じ）。印を付けないと
				// alertHeld が「照会できません」を別に送り、1 件の PENDING で 2 通になる
				ids := pendingBySymbol[po.Symbol]
				for _, id := range ids {
					coveredByFailure[id] = true
				}
				fail(po.Symbol, pendingFailureNote(ids))
			case po.Request != nil:
				fail(po.Symbol, fmt.Sprintf("買付余力を照会できないため発注しません: %v", err))
			}
		}
		return failedOrders(failures, !isLive)
	}
	buyingPower := bal.BuyingPower

	for _, po := range planned {
		if po.Request == nil {
			switch {
			case po.Failed:
				fail(po.Symbol, po.Note)
			case po.Note != "":
				logger.Info("accum.skip", fmt.Sprintf("%s: %s", po.Symbol, po.Note))
			}
			continue
		}

		req := *po.Request
		if ids := pendingBySymbol[po.Symbol]; len(ids) > 0 {
			for _, id := range ids {
				coveredByFailure[id] = true
			}
			fail(po.Symbol, pendingFailureNote(ids))
			continue
		}
		placed, err := led.WasPlaced(req.ClientOrderID)
		if err != nil {
			// 台帳が読めないなら二重発注を否定できない。安全側に倒して以降を止める。
			logger.Error("accum.ledger_read_failed", err.Error())
			return fmt.Errorf("台帳を読めないため発注を中止しました: %w", err)
		}
		if placed {
			logger.Info("accum.skip", fmt.Sprintf("%s: 既に発注済み (ID: %s)", po.Symbol, req.ClientOrderID))
			continue
		}

		// 見積りと買付余力チェック
		preview, err := b.Preview(req)
		if err != nil {
			fail(po.Symbol, fmt.Sprintf("見積り失敗: %v", err))
			continue
		}
		totalCost := preview.EstimatedCost.Add(preview.EstimatedFee)
		if totalCost.GreaterThan(buyingPower) {
			fail(po.Symbol, fmt.Sprintf("買付余力不足 (必要 %s / 余力 %s)", totalCost, buyingPower))
			continue
		}

		mkt := domain.MarketJP
		amt := po.Amount

		if !isLive {
			// dry-run
			if rerr := led.Record(req, ledger.DryRunStatus, nil, &monthStart, &amt, &mkt); rerr != nil {
				logger.Warn("accum.ledger", fmt.Sprintf("%s: dry-run の記録に失敗: %v", po.Symbol, rerr))
			}
			logger.Info("accum.dry_run", fmt.Sprintf("[dry-run] %s %s株 @ %s円 (%s)", po.Symbol, req.Quantity, po.LimitPrice, req.Reason))
			continue
		}

		// 実発注。台帳に送信中で先に記録してから送る。
		ack, err := placeRecorded(b, led, req, monthStart, amt, mkt)
		if err != nil {
			var rejected *broker.OrderRejectedError
			if errors.As(err, &rejected) {
				// 届いた上で拒否された。台帳は REJECTED で、次回の差額で埋め直す
				fail(po.Symbol, fmt.Sprintf("発注拒否: %v", err))
				continue
			}
			// それ以外は止める。送信結果不明（ErrUnconfirmedOrder）・送ったのに台帳を
			// 書けない（ErrOrderNotRecorded・拒否を REJECTED にできない ErrRejectionNotRecorded）・
			// 送る前の記録に失敗——どれも台帳が実態を
			// 表していないので、続けて出すと二重発注を否定できない
			code := "accum.order_aborted"
			var unconfirmed *ErrUnconfirmedOrder
			var unrecorded *ErrOrderNotRecorded
			var rejectionUnrecorded *ErrRejectionNotRecorded
			switch {
			case errors.As(err, &unconfirmed):
				code = "accum.unconfirmed"
			case errors.As(err, &unrecorded), errors.As(err, &rejectionUnrecorded):
				code = "accum.order_not_recorded"
			}
			logger.Error(code, fmt.Sprintf("%s: %v", po.Symbol, err))
			if len(failures) > 0 {
				return fmt.Errorf("%w（ほかに発注できなかった銘柄: %s）", err, strings.Join(failures, " / "))
			}
			return err
		}

		logger.Info("accum.order", fmt.Sprintf("発注成功: %s %s株 (ID: %s)", po.Symbol, req.Quantity, ack.ClientOrderID))
		buyingPower = buyingPower.Sub(totalCost)
	}

	return failedOrders(failures, !isLive)
}

// describeHeld は保留した注文をダイジェストの異常の文にする。
//
// pending は送信結果不明（PENDING）のまま残した注文、other はそれ以外（照会できない・
// ブローカーの応答に無い）。前日以前の PENDING は次の run でも判定しないので、
// 「次の run で再判定」とは書かない。
func describeHeld(held []UnresolvedOrder) (pending, other string) {
	var manual, retryPending, otherCount int
	for _, u := range held {
		switch {
		case u.NeedsResolve:
			manual++
		case u.Status == string(domain.OrderStatusPending):
			retryPending++
		default:
			otherCount++
		}
	}
	var parts []string
	if manual > 0 {
		parts = append(parts, fmt.Sprintf("%d 件は前日以前の注文で自動では決めない（`accum pending resolve` で確定するまで残り、その銘柄は発注しない）", manual))
	}
	if retryPending > 0 {
		parts = append(parts, fmt.Sprintf("%d 件は次の run で再判定", retryPending))
	}
	if len(parts) > 0 {
		pending = fmt.Sprintf("送信結果不明の注文 %d 件を保留: %s", manual+retryPending, strings.Join(parts, "、"))
	}
	if otherCount > 0 {
		other = fmt.Sprintf("%d 件の注文を照会できず保留（次の run で再照会）", otherCount)
	}
	return pending, other
}

// alertHeld は保留した注文を 1 通で知らせる。covered（発注できなかった銘柄の行で既に
// 知らせる注文）は外す。前日以前の送信結果不明が 1 件でもあれば、件名で
// `accum pending resolve` が要ることを言う（次の run を待っても消えない）。
func alertHeld(held []UnresolvedOrder, covered map[string]bool, dryRun bool, logger *logging.Logger) {
	var lines []string
	needsResolve := false
	for _, u := range held {
		if covered[u.ClientOrderID] {
			continue
		}
		lines = append(lines, u.Describe())
		needsResolve = needsResolve || u.NeedsResolve
	}
	if len(lines) == 0 {
		return
	}
	title := "積立: 前回の注文を照会できません（次の run で再判定。続くなら口座を確認してください）"
	if needsResolve {
		title = "積立: 送信結果不明の注文が残っています（口座の約定履歴で確かめて `accum pending resolve` で確定するまで、その銘柄は発注しません）"
	}
	if dryRun {
		title = DryRunTitlePrefix + title
	}
	alert(title, strings.Join(lines, "\n"), logger)
}

// orderableSymbols は有効な戦略の発注できる銘柄（発注の表記）。指数（^ で始まる）は除く。
func orderableSymbols(cfg *accumcfg.AccumConfig) []string {
	var out []string
	seen := map[string]bool{}
	for _, entry := range cfg.Tactics {
		if !entry.IsEnabled() {
			continue
		}
		for _, sym := range entry.Symbols {
			code := BrokerSymbol(sym)
			if strings.HasPrefix(strings.TrimSpace(sym), "^") || seen[code] {
				continue
			}
			seen[code] = true
			out = append(out, code)
		}
	}
	return out
}

// OrdersFailedError は「出すべきなのに出せなかった」銘柄があった回。
//
// 呼び出し側（cmd/accum）は通知・ダイジェストの失敗・非 0 終了にする。
// 出せた銘柄の注文はそのまま（止めるのは知らせることだけ）。
type OrdersFailedError struct {
	Lines []string
	// DryRun は dry-run の回か。通知の件名に DryRunTitlePrefix を付ける（本発注の失敗と見分ける）
	DryRun bool
}

// DryRunTitlePrefix は dry-run の回の運用通知の件名に付ける印。
const DryRunTitlePrefix = "[dry-run] "

func (e *OrdersFailedError) Error() string {
	return fmt.Sprintf("%d 件を発注できませんでした: %s", len(e.Lines), strings.Join(e.Lines, " / "))
}

// failedOrders は失敗が無ければ nil（型付きの nil をエラーとして返さない）。
func failedOrders(lines []string, dryRun bool) error {
	if len(lines) == 0 {
		return nil
	}
	return &OrdersFailedError{Lines: lines, DryRun: dryRun}
}

// WindowState は今が発注時間帯かと、有効な戦略の時間帯の説明を返す。
//
// 戦略ごとに時間帯を変えられるので、どれか1つでも許していれば発注に進む
// （どの銘柄を出せるかは PlanOrders 側で銘柄ごとに判定する）。
func WindowState(cfg *accumcfg.AccumConfig, now time.Time) (allowed bool, windows string) {
	seen := map[string]struct{}{}
	var descriptions []string
	for _, entry := range cfg.Active() {
		tactic, err := entry.Build()
		if err != nil {
			continue
		}
		w := tactic.Window()
		if _, ok := seen[w.Describe()]; !ok {
			seen[w.Describe()] = struct{}{}
			descriptions = append(descriptions, w.Describe())
		}
		if w.Allows(now) {
			allowed = true
		}
	}
	sort.Strings(descriptions)
	return allowed, strings.Join(descriptions, "、")
}

// ErrUnconfirmedOrder は送信したが結果を確認できなかった注文を表す。
//
// 通信断やタイムアウトでは「届いていない」と「届いたが応答が返らない」を
// 区別できない。台帳には送信中（PENDING）が残るので、次回の run は
// WasPlaced で弾かれて再送されない。人が状態を確かめるまで止める。
type ErrUnconfirmedOrder struct {
	ClientOrderID string
	Err           error
}

func (e *ErrUnconfirmedOrder) Error() string {
	return fmt.Sprintf("注文 %s の結果を確認できませんでした（送信済みの可能性があります）: %v", e.ClientOrderID, e.Err)
}

func (e *ErrUnconfirmedOrder) Unwrap() error { return e.Err }

// UnrecordedFills は台帳に無いのにブローカーには約定がある、当月の買い注文を探す。
//
// 台帳を失った（消した・別の環境で動かした）状態で走ると、当月の予算をもう一度
// 買う。ブローカーの当月の買い履歴に台帳が知らない注文 ID の約定があればそれで、
// 呼び出し側は発注を止めて人に知らせる。
//
// 返すのは 銘柄コード → 台帳に無い約定額（株数 × 約定単価）。
func UnrecordedFills(
	led *ledger.Ledger,
	b broker.Broker,
	symbols []string,
	now time.Time,
) (map[string]decimal.Decimal, error) {
	known, err := led.RecordedIDs()
	if err != nil {
		return nil, fmt.Errorf("台帳の注文 ID を読めません: %w", err)
	}
	// **注文番号でも突き合わせる。** 立花証券は client_order_id を持たないので、
	// 注文一覧から返る Order.ClientOrderID には「注文番号/営業日」が入る
	// （tachibana_orders.go の toOrder）。台帳が持つのは自分で作ったハッシュ ID なので、
	// RecordedIDs だけで比べると**自分が出した注文も「台帳に無い」**ことになり、
	// 当月に 1 件買ったら以降の run が毎回止まる。2026-09-11 の手動買付で踏んだ。
	knownBroker, err := led.BrokerOrderIDs()
	if err != nil {
		return nil, fmt.Errorf("台帳の注文番号を読めません: %w", err)
	}

	wanted := make(map[string]struct{}, len(symbols))
	for _, s := range symbols {
		wanted[s] = struct{}{}
	}

	jst := clock.ToZone(now, clock.Tokyo)
	monthStart := time.Date(jst.Year(), jst.Month(), 1, 0, 0, 0, 0, clock.Tokyo)

	history, err := b.GetOrderHistory(monthStart, jst)
	if err != nil {
		return nil, fmt.Errorf("ブローカーの注文履歴を照会できません: %w", err)
	}

	found := make(map[string]decimal.Decimal)
	for _, o := range history {
		if o.Side != domain.SideBuy || !o.FilledQuantity.IsPositive() {
			continue
		}
		if _, ok := known[o.ClientOrderID]; ok {
			continue
		}
		if _, ok := knownBroker[o.ClientOrderID]; ok {
			continue
		}
		if o.BrokerOrderID != nil {
			if _, ok := knownBroker[*o.BrokerOrderID]; ok {
				continue
			}
		}
		if _, ok := wanted[o.Symbol]; !ok {
			continue
		}
		price := decimal.Zero
		if o.AvgFillPrice != nil {
			price = *o.AvgFillPrice
		}
		found[o.Symbol] = found[o.Symbol].Add(o.FilledQuantity.Mul(price))
	}
	return found, nil
}

// pendingFailureNote は送信結果不明の注文が残る銘柄を発注しない理由。
func pendingFailureNote(ids []string) string {
	return fmt.Sprintf(
		"送信結果不明の注文 %s が残っているため発注しません（届いていれば二重買付になる。"+
			"口座の約定履歴で確かめて `accum pending resolve <client_order_id> --attribute … | --unsent` で確定する）",
		strings.Join(ids, ", "))
}

// placeRecorded は台帳に先に記録してから発注する。
//
// 送信後・記録前にプロセスが落ちる、あるいは注文は届いたのに応答が
// タイムアウトすると、次の run が同じ注文を送り直す。先に送信中（PENDING）で
// 記録しておけば WasPlaced が真になり再送されない。
//
//   - 受理された           → その状態で上書き
//   - 明確に拒否された     → REJECTED。次回の差額で埋め直す
//   - それ以外（通信断等） → 送信中のまま残し ErrUnconfirmedOrder
func placeRecorded(
	b broker.Broker,
	led *ledger.Ledger,
	req domain.OrderRequest,
	planMonth string,
	amount decimal.Decimal,
	market domain.Market,
) (*domain.OrderAck, error) {
	if err := led.Record(req, string(domain.OrderStatusPending), nil, &planMonth, &amount, &market); err != nil {
		return nil, fmt.Errorf("発注前の台帳記録に失敗しました（発注を中止します）: %w", err)
	}

	ack, err := b.Place(req)
	if err != nil {
		var rejected *broker.OrderRejectedError
		if errors.As(err, &rejected) {
			// 届いた上で拒否された。次回の差額で埋め直せる。
			if uerr := led.UpdateStatus(req.ClientOrderID, string(domain.OrderStatusRejected), nil, nil); uerr != nil {
				// 台帳が書けない状態で次の銘柄へ進むと、送った事実を残せないまま注文を出しうる。
				// 「発注拒否」（次へ進む）ではなく止める側に返す——拒否を %w で包むと
				// 呼び出し側の errors.As(OrderRejectedError) が真になり、次へ進んでいた
				return nil, &ErrRejectionNotRecorded{ClientOrderID: req.ClientOrderID, Path: led.Path(), Rejection: err, Err: uerr}
			}
			return nil, err
		}
		// 届いたかどうか分からない。送信中のまま残す。
		return nil, &ErrUnconfirmedOrder{ClientOrderID: req.ClientOrderID, Err: err}
	}

	if err := led.Record(req, string(ack.Status), ack.BrokerOrderID, &planMonth, &amount, &market); err != nil {
		// 送った事実は台帳に PENDING（注文番号なし）で残っている。次の run はこれを
		// 当日の注文一覧と突き合わせるまで同じ銘柄を出さない。ここで止めて人に知らせる
		return ack, &ErrOrderNotRecorded{ClientOrderID: req.ClientOrderID, BrokerOrderID: ack.BrokerOrderID, Err: err}
	}
	return ack, nil
}

// ErrOrderNotRecorded はブローカーが受理したのに台帳を受理の状態に更新できなかった注文。
//
// 以前は「発注拒否」と同じ扱いで次の銘柄へ進んでいた（A2）。台帳が書けない状態で
// 続けると、送った事実が残らないまま次の注文を出し、次の run で同じ額を買い直しうる。
// 台帳の行は送信前に書いた PENDING のまま残るので、次の run の照合（SyncOrderStatus）に回る。
type ErrOrderNotRecorded struct {
	ClientOrderID string
	BrokerOrderID *string
	Err           error
}

func (e *ErrOrderNotRecorded) Error() string {
	number := "不明"
	if e.BrokerOrderID != nil {
		number = *e.BrokerOrderID
	}
	return fmt.Sprintf("注文 %s（注文番号 %s）は受理されましたが台帳を更新できません（台帳は PENDING のまま。発注を止めます）: %v",
		e.ClientOrderID, number, e.Err)
}

func (e *ErrOrderNotRecorded) Unwrap() error { return e.Err }

// ErrRejectionNotRecorded は拒否された注文を台帳の REJECTED にできなかったもの。
//
// 台帳の行は送信前に書いた PENDING のまま残る（次の run の照合か `accum pending resolve` で
// 確定する）。台帳が書けない状態なので、ErrOrderNotRecorded と同じく以降の発注を止める。
// Unwrap は台帳のエラーだけを返し、拒否（OrderRejectedError）としては扱わせない。
type ErrRejectionNotRecorded struct {
	ClientOrderID string
	Path          string
	Rejection     error
	Err           error
}

func (e *ErrRejectionNotRecorded) Error() string {
	return fmt.Sprintf("注文 %s は拒否されました（%v）が、台帳を REJECTED にできません"+
		"（台帳 %s は PENDING のまま。発注を止めます）: %v", e.ClientOrderID, e.Rejection, e.Path, e.Err)
}

func (e *ErrRejectionNotRecorded) Unwrap() error { return e.Err }

// recordDecisions はその実行で決まった投下を履歴に追記する。
//
// 履歴が書けなくても発注は続ける（記録は運用の振り返り用であって、
// 発注の前提ではない）。呼び出し側で警告に留めているのはそのため。
func recordDecisions(store *wbhistory.Store, planned []PlannedOrder, day time.Time) error {
	decisions := make([]accumhist.Decision, 0, len(planned))
	for _, po := range planned {
		if po.JudgedOn == "" {
			continue // 足が無いなど、判断そのものが成立しなかった行
		}
		decisions = append(decisions, accumhist.Decision{
			Symbol:     po.Symbol,
			Market:     string(po.Market),
			JudgedOn:   po.JudgedOn,
			Month:      po.Month,
			Close:      po.Close,
			Due:        po.Amount,
			Target:     po.Target,
			Placed:     po.Placed,
			Multiplier: po.Multiplier,
			Tactic:     po.Tactic,
			Reason:     decisionReason(po),
		})
	}
	if len(decisions) == 0 {
		return nil
	}
	_, err := store.Append(accumhist.Kind, accumhist.DecisionFrame(decisions), day, wbhistory.AppendOptions{})
	return err
}

// decisionReason は履歴に残す理由。見送りの note があればそちらを優先する。
func decisionReason(po PlannedOrder) string {
	if po.Note != "" {
		if po.Reason == "" {
			return po.Note
		}
		return po.Reason + "／" + po.Note
	}
	return po.Reason
}
