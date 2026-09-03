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

type PlannedOrder struct {
	Symbol     string
	Amount     decimal.Decimal
	Quantity   decimal.Decimal
	LimitPrice *decimal.Decimal
	Request    *domain.OrderRequest
	Reason     string
	Note       string

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

func BrokerSymbol(symbol string) string {
	s := strings.TrimPrefix(symbol, "^")
	return strings.TrimSuffix(s, ".T")
}

// PlanOrders は最新の足データと台帳情報から本日の発注計画を立てる。
func PlanOrders(
	cfg *accumcfg.AccumConfig,
	barStore *data.BarStore,
	led *ledger.Ledger,
	now time.Time,
	ignoreWindow bool,
) (orders []PlannedOrder, staleSignals []string, err error) {

	todayJST := clock.ToZone(now, clock.Tokyo).Format("2006-01-02")
	monthStart := todayJST[:7] + "-01"
	monthTime, _ := time.Parse("2006-01-02", monthStart)

	for _, entry := range cfg.Tactics {
		if !entry.IsEnabled() {
			continue
		}

		// 設定に書かれたパラメータ（倍率・段表・発注時間帯）をそのまま反映する。
		tactic, err := entry.Build()
		if err != nil {
			return nil, nil, err
		}

		var signalBars []domain.Bar
		if entry.SignalSymbol != "" {
			signalBars, _ = barStore.Read(entry.SignalSymbol, "", "")
		}

		for _, sym := range entry.Symbols {
			bars, err := barStore.Read(sym, "", "")
			if err != nil || len(bars) == 0 {
				orders = append(orders, PlannedOrder{
					Symbol: sym,
					Note:   "足データなし",
				})
				continue
			}

			// 確定足（前日以前）で計画を計算
			var completed []domain.Bar
			for _, b := range bars {
				if b.Date < todayJST {
					completed = append(completed, b)
				}
			}
			if len(completed) == 0 {
				continue
			}

			var completedSignal []domain.Bar
			for _, sb := range signalBars {
				if sb.Date < todayJST {
					completedSignal = append(completedSignal, sb)
				}
			}

			// 最終足が古すぎるなら判定しない。
			//
			// 取得元が止まっているのに気付かず、古い配列のまま増額判定を
			// 続けるのを防ぐ。判定用の銘柄が古い場合は投下を止めず警告に留める
			// （買う銘柄の足は新しいので、判定だけが前日基準になる）。
			stale, age := isStale(completed, todayJST, cfg.Execution.MaxStaleDays)
			if stale {
				orders = append(orders, PlannedOrder{
					Symbol: BrokerSymbol(sym),
					Note: fmt.Sprintf("足が %d 日前（%s）で古いため見送り（max_stale_days=%d）",
						age, completed[len(completed)-1].Date, cfg.Execution.MaxStaleDays),
				})
				continue
			}
			if len(completedSignal) > 0 {
				if staleSig, sigAge := isStale(completedSignal, todayJST, cfg.Execution.MaxStaleDays); staleSig {
					staleSignals = append(staleSignals,
						fmt.Sprintf("%s（%s, %d日前）", entry.SignalSymbol,
							completedSignal[len(completedSignal)-1].Date, sigAge))
				}
			}

			// 発注時間帯の外なら注文は作らない。
			if !ignoreWindow && !tactic.AllowsOrder(now) {
				orders = append(orders, PlannedOrder{
					Symbol: BrokerSymbol(sym),
					Note:   fmt.Sprintf("発注時間帯の外（%s）", tactic.Window().Describe()),
				})
				continue
			}

			p, err := plan.BuildPlanWithSignal(completed, completedSignal, entry.SignalLags(), tactic, entry.MonthlyBudget)
			if err != nil || len(p.Rows) == 0 {
				continue
			}

			bSym := BrokerSymbol(sym)

			// 今月ぶんの計画行だけを取り出す。
			var thisMonth []plan.PlanRow
			for _, r := range p.Rows {
				if strings.HasPrefix(r.Date, todayJST[:7]) {
					thisMonth = append(thisMonth, r)
				}
			}
			if len(thisMonth) == 0 {
				continue // 今月の確定足がまだ無い（月初の初日）
			}

			// 積立の開始日。月の途中から始めた月は日割りにする。
			var started *time.Time
			if s := led.StartedOn(bSym); s != nil {
				if t, err := time.Parse("2006-01-02", *s); err == nil {
					started = &t
				}
			}

			placedAmount := func(symbol string, month time.Time) decimal.Decimal {
				amt, err := led.PlacedAmount(symbol, month)
				if err != nil {
					return decimal.Zero
				}
				return amt
			}

			baseTarget, extras, prorated := MonthTarget(thisMonth, entry.MonthlyBudget, monthTime, started)
			carried := CarryOver(p.Rows, bSym, monthTime, entry.MonthlyBudget, started, led.HasOrders, placedAmount)
			target := baseTarget.Add(extras)
			already := placedAmount(bSym, monthTime)

			due := target.Add(carried).Sub(already)
			if due.LessThanOrEqual(decimal.Zero) {
				continue // 今月分は発注済み
			}

			lastRow := thisMonth[len(thisMonth)-1]

			// 端数や小さな予算増は、注文が出る日にまとめる。
			if !ShouldPlaceToday(thisMonth, due, baseTarget) {
				orders = append(orders, PlannedOrder{
					Symbol:     bSym,
					Amount:     due,
					Note:       fmt.Sprintf("差額 %s円は次のリリース日に持ち越し", due.Round(0)),
					Market:     entry.MarketResolved(),
					JudgedOn:   lastRow.Date,
					Month:      monthStart,
					Close:      lastRow.Close,
					Target:     target.Add(carried),
					Placed:     already,
					Multiplier: lastRow.Multiplier,
					Tactic:     entry.Tactic,
				})
				continue
			}

			reason := fmt.Sprintf("今月の目標 %s（基本 %s", target.Round(0), baseTarget.Round(0))
			if prorated != "" {
				reason += fmt.Sprintf("〔%s〕", prorated)
			}
			reason += fmt.Sprintf("＋増額 %s）", extras.Round(0))
			if carried.IsPositive() {
				reason += fmt.Sprintf("＋前月からの繰り越し %s", carried.Round(0))
			}
			reason += fmt.Sprintf("− 発注済み %s", already.Round(0))

			lastBar := completed[len(completed)-1]
			lastPrice := lastBar.Close

			// 単元株数（既定100、オーバーライドあり）
			lotSize := marketrules.DefaultLotSize
			if ov, ok := cfg.Execution.LotSizeOverrides[bSym]; ok && ov > 0 {
				lotSize = decimal.NewFromInt(int64(ov))
			}

			// 指値価格の計算 (終値 * (1 + offset))
			offset := decimal.RequireFromString("0.01")
			if cfg.Execution.LimitOffset != "" {
				if d, err := decimal.NewFromString(cfg.Execution.LimitOffset); err == nil {
					offset = d
				}
			}

			limitPrice := lastPrice.Mul(decimal.NewFromInt(1).Add(offset))
			snappedPrice, _ := marketrules.SnapToTick(limitPrice, domain.SideBuy, false, marketrules.RoundingConservative)

			// 株数 = floor(due / snappedPrice) を lotSize で切り捨て
			rawQty := due.Div(snappedPrice).Floor()
			qty, _ := marketrules.RoundToLot(rawQty, lotSize)

			if qty.LessThanOrEqual(decimal.Zero) {
				orders = append(orders, PlannedOrder{
					Symbol:     bSym,
					Amount:     due,
					Reason:     reason,
					Note:       fmt.Sprintf("単元株数（%s株）に満たないため見送り", lotSize),
					Market:     entry.MarketResolved(),
					JudgedOn:   lastRow.Date,
					Month:      monthStart,
					Close:      lastRow.Close,
					Target:     target.Add(carried),
					Placed:     already,
					Multiplier: lastRow.Multiplier,
					Tactic:     entry.Tactic,
				})
				continue
			}

			orderType := domain.OrderTypeLimit
			if cfg.Execution.OrderType == "market" {
				orderType = domain.OrderTypeMarket
			}

			taxType := domain.TaxAccountSpecific
			if cfg.Execution.TaxAccountType != "" {
				taxType = domain.TaxAccountType(cfg.Execution.TaxAccountType)
			}

			orderID := domain.MakeClientOrderID(todayJST, bSym, domain.SideBuy, qty)
			req, err := domain.NewOrderRequest(
				orderID,
				bSym,
				domain.SideBuy,
				orderType,
				qty,
				&snappedPrice,
				taxType,
				reason,
				domain.TradeTypeCash,
			)
			if err != nil {
				continue
			}

			orders = append(orders, PlannedOrder{
				Symbol:     bSym,
				Amount:     due,
				Quantity:   qty,
				LimitPrice: &snappedPrice,
				Request:    &req,
				Reason:     req.Reason,
				Market:     entry.MarketResolved(),
				JudgedOn:   lastRow.Date,
				Month:      monthStart,
				Close:      lastRow.Close,
				Target:     target.Add(carried),
				Placed:     already,
				Multiplier: lastRow.Multiplier,
				Tactic:     entry.Tactic,
			})
		}
	}

	return orders, staleSignals, nil
}

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
	// 照会できなかった注文は「発注済み」に数えたまま保留してある。次の run が
	// もう一度判定する。決められないものはダイジェストの異常として残す（AI が読む）。
	if len(synced.Unresolved) > 0 {
		digest.Anomaly("accum.pending_ambiguous",
			fmt.Sprintf("%d 件の注文を判定できず保留（次の run で再判定）", len(synced.Unresolved)))
	}
	for _, u := range synced.Unresolved {
		logger.Warn("accum.unresolved", "照会できず保留: "+u.Describe())
	}
	if len(synced.Unresolved) > 0 {
		var lines []string
		for _, u := range synced.Unresolved {
			lines = append(lines, u.Describe())
		}
		notify.Alert("積立: 前回の注文を照会できません（口座を確認してください）",
			strings.Join(lines, "\n"), logger)
		digest.Anomaly("accum.unresolved",
			fmt.Sprintf("%d 件の注文を照会できませんでした", len(synced.Unresolved)))
	}

	// 2. 本日の発注計画
	planned, staleSignals, err := PlanOrders(cfg, barStore, led, now, ignoreWindow)
	if err != nil {
		return err
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
	bal, err := b.GetBalance()
	buyingPower := decimal.Zero
	if err == nil && bal != nil {
		buyingPower = bal.BuyingPower
	}

	for _, po := range planned {
		if po.Request == nil {
			if po.Note != "" {
				logger.Info("accum.skip", fmt.Sprintf("%s: %s", po.Symbol, po.Note))
			}
			continue
		}

		req := *po.Request
		if led.WasPlaced(req.ClientOrderID) {
			logger.Info("accum.skip", fmt.Sprintf("%s: 既に発注済み (ID: %s)", po.Symbol, req.ClientOrderID))
			continue
		}

		// 見積りと買付余力チェック
		preview, err := b.Preview(req)
		if err != nil {
			logger.Warn("accum.error", fmt.Sprintf("%s 見積り失敗: %v", po.Symbol, err))
			continue
		}
		totalCost := preview.EstimatedCost.Add(preview.EstimatedFee)
		if totalCost.GreaterThan(buyingPower) {
			logger.Warn("accum.insufficient_funds", fmt.Sprintf("%s: 買付余力不足 (必要 %s / 余力 %s)", po.Symbol, totalCost, buyingPower))
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
			var unconfirmed *ErrUnconfirmedOrder
			if errors.As(err, &unconfirmed) {
				// 届いたかどうか分からない。以降を止めて人に確かめてもらう。
				logger.Error("accum.unconfirmed", unconfirmed.Error())
				return err
			}
			logger.Error("accum.order_failed", fmt.Sprintf("%s 発注拒否: %v", po.Symbol, err))
			continue
		}

		logger.Info("accum.order", fmt.Sprintf("発注成功: %s %s株 (ID: %s)", po.Symbol, req.Quantity, ack.ClientOrderID))
		buyingPower = buyingPower.Sub(totalCost)
	}

	return nil
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
				// PENDING のまま残ると次回 WasPlaced で弾かれ、差額が埋まらない。拒否は事実なので併記して返す
				return nil, fmt.Errorf("%w（さらに台帳を REJECTED にできませんでした。%s の行を確かめてください: %v）", err, led.Path(), uerr)
			}
			return nil, err
		}
		// 届いたかどうか分からない。送信中のまま残す。
		return nil, &ErrUnconfirmedOrder{ClientOrderID: req.ClientOrderID, Err: err}
	}

	if err := led.Record(req, string(ack.Status), ack.BrokerOrderID, &planMonth, &amount, &market); err != nil {
		return ack, fmt.Errorf("発注は成功しましたが台帳の更新に失敗しました: %w", err)
	}
	return ack, nil
}

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
