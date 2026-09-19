// Package ledger はデイトレの発注台帳。「今日もう買ったか」「何を手仕舞うべきか」を
// 実行をまたいで覚える。
//
// cron は open と close を別プロセスで呼ぶ。close が売るべき数量は、open が送った
// 注文とその約定状況にしか無い。ブローカーの建玉を無条件に売ると、他の戦略（積立）の
// 保有まで手放す。
package ledger

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/shopspring/decimal"
)

// DryRunStatus は dry-run の記録に付ける状態。「発注済み」には数えない。
const DryRunStatus = "dry_run"

const dayLayout = "2006-01-02"

// orderColumns は Order を読み出す列。query の Scan と同じ並び。
const orderColumns = "client_order_id, broker_order_id, day, symbol, side, quantity," +
	" filled_quantity, status, price, avg_fill_price, placed_at, updated_at, reason, trade, verify," +
	" ref_price, ref_bid, ref_ask, ref_at, condition"

// Ref は注文を**送る直前**の時価。約定単価と比べると執行そのものの滑りが出る。
// 取れなければ記録しないだけで、発注は続ける。
type Ref struct {
	// Price は現在値（無ければ最良気配の仲値）。
	Price decimal.Decimal
	// Bid / Ask は最良買気配値・最良売気配値（無ければゼロ）。成行が食う側の値。
	Bid, Ask decimal.Decimal
	// At は現在値の時刻。
	At time.Time
}

// deadStatuses は未約定のまま終わった状態。同じ判断を送り直してよい。
var deadStatuses = map[string]struct{}{
	string(domain.OrderStatusCancelled): {},
	string(domain.OrderStatusRejected):  {},
	string(domain.OrderStatusExpired):   {},
	string(domain.OrderStatusUnsent):    {},
}

// Order は台帳の 1 行。
type Order struct {
	ClientOrderID  string
	BrokerOrderID  *string
	Day            time.Time
	Symbol         string
	Side           domain.Side
	Quantity       decimal.Decimal
	FilledQuantity decimal.Decimal
	Status         string
	Price          *decimal.Decimal
	AvgFillPrice   *decimal.Decimal
	// RefPrice / RefBid / RefAsk は**送る直前**に照会した時価（現在値・最良買気配・
	// 最良売気配）。AvgFillPrice との差が執行そのものの滑り。判断時の Price（9:00 の
	// 気配）との差は、選定から発注までの**遅れ**も混じるので別物。
	RefPrice, RefBid, RefAsk *decimal.Decimal
	// RefAt は RefPrice の時刻（現在値の時刻。RFC3339 UTC）。時価がどれだけ古いか。
	RefAt     *string
	PlacedAt  string
	UpdatedAt *string
	Reason    string
	// Trade は現物 / 信用新規 / 信用返済。古い台帳（列が無い）は現物。
	Trade domain.TradeType
	// Condition は執行条件。空 = ザラ場の成行、OPENING = 寄成（寄る前に出した）。
	// 滑り（RefPrice と AvgFillPrice の差）を寄成とザラ場の成行で分けて見るために残す。
	Condition domain.OrderCondition
	// Verify は発注経路の実機検証（docs/BROKER_VERIFY.md）で出した注文か。
	// 建玉としては本物なので close / verify は同じように扱うが、成績の集計
	//（資産曲線のゲート・evaluate・レポート）からは外す。戦略の判断ではないため。
	Verify bool
}

// IsDryRun は dry-run の記録か。
func (o Order) IsDryRun() bool { return o.Status == DryRunStatus }

// IsOpen は結果が確定していない（照会が要る）か。
func (o Order) IsOpen() bool {
	if o.IsDryRun() {
		return false
	}
	return !domain.OrderStatus(o.Status).IsTerminal()
}

// IsDead は未約定のまま終わったか（同じ判断を送り直してよい）。
//
// 一部約定の後に取消・失効した注文は**死んでいない**——約定した株数の建玉（返済なら返済済みの株数）が
// 残る。状態だけで死んだと読むと、建てた売建を「建てていない」と数え、open が枠を埋め直し、
// 引けの判定が台帳外の建玉として二重に返済する（材料での取消 daytrade guard で起きる）。
func (o Order) IsDead() bool {
	if o.IsDryRun() || o.FilledQuantity.IsPositive() {
		return false
	}
	_, dead := deadStatuses[o.Status]
	return dead
}

// IsEntry は建てる側の注文か（現物の買い、信用の新規建て）。手仕舞う側なら偽。
func (o Order) IsEntry() bool {
	switch o.Trade {
	case domain.TradeTypeMarginOpen:
		return true
	case domain.TradeTypeMarginClose:
		return false
	default:
		return o.Side == domain.SideBuy
	}
}

// IsExit は手仕舞う側の注文か。
func (o Order) IsExit() bool { return !o.IsEntry() }

// Leg は "long"（買って売る）か "short"（売建てて買い戻す）か。
func (o Order) Leg() string {
	opensWithBuy := (o.IsEntry() && o.Side == domain.SideBuy) ||
		(o.IsExit() && o.Side == domain.SideSell)
	if opensWithBuy {
		return "long"
	}
	return "short"
}

// Ledger は SQLite の台帳。1 環境 1 ファイル（state/daytrade-<env>.db）。
type Ledger struct {
	db   *sql.DB
	path string
	// Verify が真なら、この実行で書く注文に検証の印を付ける（--verify）。
	// 実機検証は本番の台帳に本物の建玉を作るので、別の DB に逃がすと close /
	// verify が拾えなくなる。同じ台帳に置いたまま、印で成績から外す。
	Verify bool
}

// Path は台帳ファイルの置き場所。二重発注を疑う場面で人に示す。
func (l *Ledger) Path() string { return l.path }

// migrations は台帳のスキーマの履歴。版は PRAGMA user_version（storage.Migrate）。
// 列を足すときは末尾に段を足す——既存の段を書き換えても適用済みの DB には効かない。
var migrations = []storage.Migration{
	{Name: "orders", Up: storage.Exec(`CREATE TABLE IF NOT EXISTS orders (
		client_order_id TEXT PRIMARY KEY,
		broker_order_id TEXT,
		day TEXT NOT NULL,
		symbol TEXT NOT NULL,
		side TEXT NOT NULL,
		quantity TEXT NOT NULL,
		filled_quantity TEXT NOT NULL DEFAULT '0',
		status TEXT NOT NULL,
		price TEXT,
		avg_fill_price TEXT,
		reason TEXT,
		placed_at TEXT NOT NULL,
		updated_at TEXT,
		trade TEXT NOT NULL DEFAULT 'CASH'
	)`, "CREATE INDEX IF NOT EXISTS orders_day ON orders(day, side)")},
	// 既存の台帳（trade 列が無い）を壊さずに列を足す。既定 CASH = 従来の現物
	{Name: "orders.trade", Up: storage.AddColumns("orders", map[string]string{
		"trade": "TEXT NOT NULL DEFAULT 'CASH'",
	})},
	// 実機検証の注文に印を付ける。既定 0 = 従来どおりの本番の注文
	{Name: "orders.verify", Up: storage.AddColumns("orders", map[string]string{
		"verify": "INTEGER NOT NULL DEFAULT 0",
	})},
	// 送る直前の時価（Ref）。約定単価との差が執行の滑り。古い行は null のまま
	{Name: "orders.ref_price", Up: storage.AddColumns("orders", map[string]string{
		"ref_price": "TEXT",
		"ref_bid":   "TEXT",
		"ref_ask":   "TEXT",
		"ref_at":    "TEXT",
	})},
	// 執行条件。空文字 = 従来どおりザラ場の成行、OPENING = 寄成。
	// 滑り（ref_price と約定単価の差）を寄成とザラ場の成行で分けて見るために残す
	{Name: "orders.condition", Up: storage.AddColumns("orders", map[string]string{
		"condition": "TEXT NOT NULL DEFAULT ''",
	})},
}

// Open は台帳を開き、スキーマを最新に揃える。
func Open(dbPath string) (*Ledger, error) {
	db, err := storage.OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	if err := storage.Migrate(db, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("台帳 %s: %w", dbPath, err)
	}
	return &Ledger{db: db, path: dbPath}, nil
}

// Close は台帳を閉じる。
func (l *Ledger) Close() error { return l.db.Close() }

// Record は発注の結果を残す。同じ ID なら上書き（dry-run → 本発注の順で来る）。
func (l *Ledger) Record(req domain.OrderRequest, day time.Time, status string, price *decimal.Decimal, brokerOrderID *string) error {
	_, err := l.db.Exec(
		`INSERT OR REPLACE INTO orders (client_order_id, broker_order_id, day, symbol, side,
			quantity, filled_quantity, status, price, avg_fill_price, reason, placed_at,
			updated_at, trade, verify, condition)
		 VALUES (?, ?, ?, ?, ?, ?, '0', ?, ?, NULL, ?, ?, NULL, ?, ?, ?)`,
		req.ClientOrderID, brokerOrderID, day.Format(dayLayout), req.Symbol, string(req.Side),
		req.Quantity.String(), status, decimalPtrString(price), req.Reason,
		clock.NowUTC().Format(time.RFC3339), string(req.Trade), boolToInt(l.Verify),
		string(req.Condition))
	if err != nil {
		return fmt.Errorf("台帳への記録に失敗しました: %w", err)
	}
	return nil
}

// SetRef は送る直前の時価を控える（Record の直後、発注の前に呼ぶ）。
//
// Record と分けてあるのは、これが**測るためだけ**の記録だから。取れなくても
// 発注は続ける——ここで失敗しても注文の可否には関わらせない。
func (l *Ledger) SetRef(clientOrderID string, ref Ref) error {
	_, err := l.db.Exec(
		"UPDATE orders SET ref_price = ?, ref_bid = ?, ref_ask = ?, ref_at = ? WHERE client_order_id = ?",
		decimalString(ref.Price), decimalString(ref.Bid), decimalString(ref.Ask),
		ref.At.UTC().Format(time.RFC3339), clientOrderID)
	if err != nil {
		return fmt.Errorf("台帳に執行時の時価を記録できません: %w", err)
	}
	return nil
}

// UpdateStatus は照会の結果で状態と約定を更新する。
func (l *Ledger) UpdateStatus(clientOrderID string, status domain.OrderStatus, filled decimal.Decimal, avgFillPrice *decimal.Decimal, brokerOrderID *string) error {
	_, err := l.db.Exec(
		`UPDATE orders SET status = ?, filled_quantity = ?, avg_fill_price = ?,
			broker_order_id = COALESCE(?, broker_order_id), updated_at = ?
		 WHERE client_order_id = ?`,
		string(status), filled.String(), decimalPtrString(avgFillPrice), brokerOrderID,
		clock.NowUTC().Format(time.RFC3339), clientOrderID)
	if err != nil {
		return fmt.Errorf("台帳の更新に失敗しました: %w", err)
	}
	return nil
}

// ClearDryRun はその日の dry-run の記録を消す
// （確認のたびに増えて台帳が読みにくくなるため）。
func (l *Ledger) ClearDryRun(day time.Time) (int, error) {
	res, err := l.db.Exec("DELETE FROM orders WHERE day = ? AND status = ?", day.Format(dayLayout), DryRunStatus)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// WasPlaced は本発注として送り、まだ生きているか約定した記録があるか。
//
// dry-run は数えない。拒否・取消・失効で終わった注文も数えない——同じ判断を
// もう一度送ってよい（再送は呼び出し側が ID の種を変える）。
//
// 台帳を読めなければ error。「読めない」を「未発注」と読むと、既に送った注文を
// もう一度送る（二重発注）。呼び出し側は error で止めること。
func (l *Ledger) WasPlaced(clientOrderID string) (bool, error) {
	o, ok, err := l.Get(clientOrderID)
	if err != nil {
		return false, fmt.Errorf("台帳の読み出しに失敗しました（発注済みか判定できません）: %w", err)
	}
	if !ok || o.IsDryRun() {
		return false, nil
	}
	return !o.IsDead(), nil
}

// DeadCount はその日・その銘柄・その売買で、拒否・取消・失効に終わった注文の数
// （再送の ID の種に使う）。一部約定して終わった注文も数える——ID は使い終わっているので、
// 残りを送り直すときに同じ ID を作らない。
func (l *Ledger) DeadCount(day time.Time, symbol string, side domain.Side) int {
	orders, err := l.OrdersOn(day, &side)
	if err != nil {
		return 0
	}
	n := 0
	for _, o := range orders {
		if _, dead := deadStatuses[o.Status]; o.Symbol == symbol && dead {
			n++
		}
	}
	return n
}

// Get は 1 件の注文。無ければ ok が偽。
func (l *Ledger) Get(clientOrderID string) (Order, bool, error) {
	orders, err := l.query("SELECT "+orderColumns+" FROM orders WHERE client_order_id = ?", clientOrderID)
	if err != nil || len(orders) == 0 {
		return Order{}, false, err
	}
	return orders[0], true, nil
}

// BrokerOrderIDs は台帳が知っている注文番号（broker_order_id）の集合。
// 送信結果不明の注文をブローカーの一覧と突き合わせるとき、既に帰属済みのものを除くのに使う。
func (l *Ledger) BrokerOrderIDs() (map[string]struct{}, error) {
	rows, err := l.db.Query("SELECT broker_order_id FROM orders WHERE broker_order_id IS NOT NULL AND broker_order_id != ''")
	if err != nil {
		return nil, fmt.Errorf("台帳の注文番号を読めません: %w", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// OrdersOn はその日の注文（dry-run を含む）。side が nil なら全部。
func (l *Ledger) OrdersOn(day time.Time, side *domain.Side) ([]Order, error) {
	query := "SELECT " + orderColumns + " FROM orders WHERE day = ?"
	args := []any{day.Format(dayLayout)}
	if side != nil {
		query += " AND side = ?"
		args = append(args, string(*side))
	}
	query += " ORDER BY placed_at"
	return l.query(query, args...)
}

// EntriesOn はその日の建てる側の注文（dry-run を含む）。現物の買い・信用の新規建て。
func (l *Ledger) EntriesOn(day time.Time) ([]Order, error) {
	orders, err := l.OrdersOn(day, nil)
	if err != nil {
		return nil, err
	}
	return filter(orders, Order.IsEntry), nil
}

// ExitsOn はその日の手仕舞う側の注文（dry-run を含む）。現物の売り・信用の返済。
func (l *Ledger) ExitsOn(day time.Time) ([]Order, error) {
	orders, err := l.OrdersOn(day, nil)
	if err != nil {
		return nil, err
	}
	return filter(orders, Order.IsExit), nil
}

// OpenOrders は結果が確定していない注文（全期間）。
func (l *Ledger) OpenOrders() ([]Order, error) {
	orders, err := l.query("SELECT " + orderColumns + " FROM orders ORDER BY placed_at")
	if err != nil {
		return nil, err
	}
	return filter(orders, Order.IsOpen), nil
}

// PlacedBetween は発注時刻（placed_at）が [start, end) の注文（dry-run を含む、全部の日）。
//
// 台帳の day は建てた日で、持ち越しの返済は前の日の下に積まれる。「今日送った注文」は
// day ではなく placed_at で引く（ブローカーの当日の注文一覧と突き合わせるとき）。
func (l *Ledger) PlacedBetween(start, end time.Time) ([]Order, error) {
	// placed_at は RFC3339 の UTC（末尾 Z）で書いているので、文字列の比較が時刻の比較になる
	return l.query("SELECT "+orderColumns+" FROM orders WHERE placed_at >= ? AND placed_at < ? ORDER BY placed_at",
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
}

// Recent は新しい順の注文。
func (l *Ledger) Recent(limit int) ([]Order, error) {
	return l.query("SELECT "+orderColumns+" FROM orders ORDER BY placed_at DESC LIMIT ?", limit)
}

func filter(orders []Order, keep func(Order) bool) []Order {
	out := make([]Order, 0, len(orders))
	for _, o := range orders {
		if keep(o) {
			out = append(out, o)
		}
	}
	return out
}

func (l *Ledger) query(query string, args ...any) ([]Order, error) {
	rows, err := l.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("台帳の読み出しに失敗しました: %w", err)
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var (
			o                      Order
			brokerOrderID          *string
			dayText                string
			side, quantity, filled string
			price, avgFillPrice    *string
			updatedAt, reason      *string
			trade                  *string
			verify                 *int64
			refPrice, refBid       *string
			refAsk, condition      *string
		)
		if err := rows.Scan(&o.ClientOrderID, &brokerOrderID, &dayText, &o.Symbol, &side,
			&quantity, &filled, &o.Status, &price, &avgFillPrice, &o.PlacedAt, &updatedAt,
			&reason, &trade, &verify, &refPrice, &refBid, &refAsk, &o.RefAt, &condition); err != nil {
			return nil, err
		}
		if condition != nil {
			o.Condition = domain.OrderCondition(*condition)
		}
		o.RefPrice = parseDecimalPtr(refPrice)
		o.RefBid = parseDecimalPtr(refBid)
		o.RefAsk = parseDecimalPtr(refAsk)
		o.BrokerOrderID = brokerOrderID
		o.Day, _ = time.Parse(dayLayout, dayText)
		o.Side = domain.Side(side)
		o.Quantity = parseDecimal(quantity)
		o.FilledQuantity = parseDecimal(filled)
		o.Price = parseDecimalPtr(price)
		o.AvgFillPrice = parseDecimalPtr(avgFillPrice)
		o.UpdatedAt = updatedAt
		if reason != nil {
			o.Reason = *reason
		}
		o.Trade = domain.TradeTypeCash
		if trade != nil && *trade != "" {
			o.Trade = domain.TradeType(*trade)
		}
		o.Verify = verify != nil && *verify != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

// Backup は台帳を別ファイルに複製する。
func (l *Ledger) Backup(destination string) error {
	// SQLite の VACUUM INTO は実行中の接続から一貫したコピーを作る
	// （ファイルコピーは WAL の途中を掴む恐れがある）。
	_, err := l.db.Exec("VACUUM INTO ?", destination)
	if err != nil {
		return fmt.Errorf("台帳のバックアップに失敗しました: %w", err)
	}
	return nil
}

// RealizedPnL は日ごとの実現損益（円）。建てた注文と手仕舞った注文の単価差 × 数量
// （手数料は含まない）。
//
// ロング（買って売る）もショート（売建てて買い戻す）も「売り単価 − 買い単価」で
// 同じ式になる。leg を "long" / "short" にするとその脚だけ
// （資産曲線の合図は**ロング側**で見る）。
//
// dry-run は数えない。本発注で建てた注文が無い日は 0。建てて約定したのに手仕舞いの
// 約定単価が無い（照会前・未約定・記録なし）日は **nil**——0 と混ぜると「負けた」と誤読する。
//
// 同じ銘柄・同じ脚の手仕舞いが複数本（引けの一部約定 + 翌寄りの持ち越し返済）あれば、
// 約定数量で加重して合わせる。最後の 1 本だけを見ると残りの損益が落ちる。
func (l *Ledger) RealizedPnL(days []time.Time, leg string) (map[string]*float64, error) {
	result := make(map[string]*float64, len(days))
	for _, day := range days {
		key := day.Format(dayLayout)
		all, err := l.OrdersOn(day, nil)
		if err != nil {
			return nil, err
		}
		var orders []Order
		for _, o := range all {
			if o.IsDryRun() {
				continue
			}
			// 実機検証の注文は戦略の判断ではないので、資産曲線のゲートに数えない
			// （1 単元の検証取引で当日の資金が半分に縮むのを避ける）
			if o.Verify {
				continue
			}
			if leg != "" && o.Leg() != leg {
				continue
			}
			orders = append(orders, o)
		}
		entries := map[string]Order{}
		exits := map[string][]Order{}
		for _, o := range orders {
			if o.IsDead() {
				continue
			}
			k := o.Symbol + "|" + o.Leg()
			if o.IsEntry() {
				entries[k] = o
			} else {
				exits[k] = append(exits[k], o)
			}
		}
		if len(entries) == 0 {
			zero := 0.0
			result[key] = &zero
			continue
		}
		total := 0.0
		complete := true
		for k, entry := range entries {
			if entry.FilledQuantity.LessThanOrEqual(decimal.Zero) {
				continue // 約定していないなら手仕舞う物が無い
			}
			pnl, ok := RealizedOf(entry, exits[k])
			if !ok {
				complete = false
				continue
			}
			total += pnl
		}
		if complete {
			v := total
			result[key] = &v
		} else {
			result[key] = nil
		}
	}
	return result, nil
}

// RecentPnL は資産曲線ゲートの入力——leg の直近の日（days）の実現損益の合計。
//
// その脚を本発注で建てた日が 1 日も無ければ nil（始めたばかりの口座を「負けている」と
// 誤読して縮めないため）。実機検証の注文は数えない（RealizedPnL と同じ）——検証だけの
// 20 日を「建てた」と数えると、損益 0 で資金が半分に縮む。確定していない日は除いて合計し、
// incomplete に返す（呼び出し側が警告する）。確定した日が 1 日も無ければ nil。
func (l *Ledger) RecentPnL(days []time.Time, leg string) (total *float64, incomplete []string, err error) {
	history, err := l.RealizedPnL(days, leg)
	if err != nil {
		return nil, nil, err
	}
	traded := false
	for _, d := range days {
		entries, err := l.EntriesOn(d)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range entries {
			if !o.IsDryRun() && !o.Verify && o.Leg() == leg {
				traded = true
				break
			}
		}
		if traded {
			break
		}
	}
	sum := 0.0
	known := 0
	for _, d := range days {
		v := history[d.Format(dayLayout)]
		if v == nil {
			incomplete = append(incomplete, d.Format(dayLayout))
			continue
		}
		sum += *v
		known++
	}
	if !traded || known == 0 {
		return nil, incomplete, nil
	}
	return &sum, incomplete, nil
}

// RealizedOf は 1 建玉とその手仕舞い（複数本）の実現損益（円、手数料は含まない）。
//
// 「売り単価 − 買い単価」× 手仕舞いの約定数量を手仕舞いごとに足す——一部約定 200 株と
// 持ち越しの返済 100 株が別の注文になっても、300 株ぶんの損益になる。手仕舞いが 1 本も
// 無い、約定単価の無い手仕舞いがある（照会前・未約定）、または手仕舞いの約定が建玉の約定に
// 届かない（持ち越しの残りをまだ返済していない）なら ok = false（未確定）。
// 約定せずに終わった手仕舞い（照会済み）は数に入れない。
func RealizedOf(entry Order, exits []Order) (pnl float64, ok bool) {
	if entry.AvgFillPrice == nil {
		return 0, false
	}
	total := decimal.Zero
	closed := decimal.Zero
	counted := 0
	for _, exit := range exits {
		if exit.FilledQuantity.LessThanOrEqual(decimal.Zero) && exit.AvgFillPrice == nil && !exit.IsOpen() {
			continue // 何も約定せずに終わった手仕舞い（照会済み）
		}
		if exit.AvgFillPrice == nil {
			return 0, false
		}
		buy, sell := *entry.AvgFillPrice, *exit.AvgFillPrice
		if entry.Side != domain.SideBuy {
			buy, sell = *exit.AvgFillPrice, *entry.AvgFillPrice
		}
		total = total.Add(sell.Sub(buy).Mul(exit.FilledQuantity))
		closed = closed.Add(exit.FilledQuantity)
		counted++
	}
	if counted == 0 || closed.LessThan(entry.FilledQuantity) {
		return 0, false
	}
	pnl, _ = total.Float64()
	return pnl, true
}

// ExitAvgPrice は手仕舞い（複数本）の約定数量で加重した平均単価と、約定数量の合計。
// 約定した手仕舞いが無ければ ok = false。
func ExitAvgPrice(exits []Order) (avg, filled decimal.Decimal, ok bool) {
	amount := decimal.Zero
	for _, exit := range exits {
		if exit.AvgFillPrice == nil || exit.FilledQuantity.LessThanOrEqual(decimal.Zero) {
			continue
		}
		amount = amount.Add(exit.AvgFillPrice.Mul(exit.FilledQuantity))
		filled = filled.Add(exit.FilledQuantity)
	}
	if !filled.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}
	return amount.Div(filled), filled, true
}

// ExitAvgRef は手仕舞いの「送る直前の時価」を約定数量で加重平均したもの。
// 手仕舞いが複数本（引けの一部約定 + 翌寄りの返済）でも 1 つの基準値にする。
//
// 加重に使った約定数量も返す——ref_price を持つ返済が一部しか無いと、ExitAvgPrice
// （約定した返済を全部使う）と母集団がずれ、別物どうしの差が「執行の滑り」として
// 記録される。呼び出し側は数量が一致するときだけ滑りを出す（2026-09-16 のレビュー）。
func ExitAvgRef(exits []Order) (avg decimal.Decimal, filled decimal.Decimal, ok bool) {
	amount, qty := decimal.Zero, decimal.Zero
	for _, exit := range exits {
		if exit.RefPrice == nil || exit.FilledQuantity.LessThanOrEqual(decimal.Zero) {
			continue
		}
		amount = amount.Add(exit.RefPrice.Mul(exit.FilledQuantity))
		qty = qty.Add(exit.FilledQuantity)
	}
	if !qty.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}
	return amount.Div(qty), qty, true
}

// boolToInt は SQLite に真偽を入れるための 0 / 1。
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func parseDecimal(text string) decimal.Decimal {
	d, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func parseDecimalPtr(text *string) *decimal.Decimal {
	if text == nil || *text == "" {
		return nil
	}
	d, err := decimal.NewFromString(*text)
	if err != nil {
		return nil
	}
	return &d
}

func decimalPtrString(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}

// decimalString はゼロを null にする（取れなかった値と 0 円を見分ける）。
func decimalString(d decimal.Decimal) any {
	if d.IsZero() {
		return nil
	}
	return d.String()
}
