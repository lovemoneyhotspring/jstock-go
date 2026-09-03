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
	PlacedAt       string
	UpdatedAt      *string
	Reason         string
	// Trade は現物 / 信用新規 / 信用返済。古い台帳（列が無い）は現物。
	Trade domain.TradeType
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
func (o Order) IsDead() bool {
	if o.IsDryRun() {
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
			updated_at, trade)
		 VALUES (?, ?, ?, ?, ?, ?, '0', ?, ?, NULL, ?, ?, NULL, ?)`,
		req.ClientOrderID, brokerOrderID, day.Format(dayLayout), req.Symbol, string(req.Side),
		req.Quantity.String(), status, decimalPtrString(price), req.Reason,
		clock.NowUTC().Format(time.RFC3339), string(req.Trade))
	if err != nil {
		return fmt.Errorf("台帳への記録に失敗しました: %w", err)
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
func (l *Ledger) WasPlaced(clientOrderID string) bool {
	var status string
	err := l.db.QueryRow("SELECT status FROM orders WHERE client_order_id = ?", clientOrderID).Scan(&status)
	if err != nil || status == DryRunStatus {
		return false
	}
	_, dead := deadStatuses[status]
	return !dead
}

// DeadCount はその日・その銘柄・その売買で、拒否・取消・失効に終わった注文の数
// （再送の ID の種に使う）。
func (l *Ledger) DeadCount(day time.Time, symbol string, side domain.Side) int {
	orders, err := l.OrdersOn(day, &side)
	if err != nil {
		return 0
	}
	n := 0
	for _, o := range orders {
		if o.Symbol == symbol && o.IsDead() {
			n++
		}
	}
	return n
}

// Get は 1 件の注文。無ければ ok が偽。
func (l *Ledger) Get(clientOrderID string) (Order, bool, error) {
	orders, err := l.query("SELECT client_order_id, broker_order_id, day, symbol, side, quantity,"+
		" filled_quantity, status, price, avg_fill_price, placed_at, updated_at, reason, trade"+
		" FROM orders WHERE client_order_id = ?", clientOrderID)
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
	query := "SELECT client_order_id, broker_order_id, day, symbol, side, quantity, filled_quantity," +
		" status, price, avg_fill_price, placed_at, updated_at, reason, trade FROM orders WHERE day = ?"
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
	orders, err := l.query("SELECT client_order_id, broker_order_id, day, symbol, side, quantity," +
		" filled_quantity, status, price, avg_fill_price, placed_at, updated_at, reason, trade" +
		" FROM orders ORDER BY placed_at")
	if err != nil {
		return nil, err
	}
	return filter(orders, Order.IsOpen), nil
}

// Recent は新しい順の注文。
func (l *Ledger) Recent(limit int) ([]Order, error) {
	return l.query("SELECT client_order_id, broker_order_id, day, symbol, side, quantity,"+
		" filled_quantity, status, price, avg_fill_price, placed_at, updated_at, reason, trade"+
		" FROM orders ORDER BY placed_at DESC LIMIT ?", limit)
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
		)
		if err := rows.Scan(&o.ClientOrderID, &brokerOrderID, &dayText, &o.Symbol, &side,
			&quantity, &filled, &o.Status, &price, &avgFillPrice, &o.PlacedAt, &updatedAt,
			&reason, &trade); err != nil {
			return nil, err
		}
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
			if leg != "" && o.Leg() != leg {
				continue
			}
			orders = append(orders, o)
		}
		entries := map[string]Order{}
		exits := map[string]Order{}
		for _, o := range orders {
			if o.IsDead() {
				continue
			}
			k := o.Symbol + "|" + o.Leg()
			if o.IsEntry() {
				entries[k] = o
			} else {
				exits[k] = o
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
			exit, ok := exits[k]
			if !ok || exit.AvgFillPrice == nil || entry.AvgFillPrice == nil {
				complete = false
				continue
			}
			buy, sell := *entry.AvgFillPrice, *exit.AvgFillPrice
			if entry.Side != domain.SideBuy {
				buy, sell = *exit.AvgFillPrice, *entry.AvgFillPrice
			}
			pnl, _ := sell.Sub(buy).Mul(exit.FilledQuantity).Float64()
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
