package ledger

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/backup"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/shopspring/decimal"
)

const DryRunStatus = "dry_run"

var deadStatuses = map[string]struct{}{
	string(domain.OrderStatusCancelled): {},
	string(domain.OrderStatusRejected):  {},
	string(domain.OrderStatusExpired):   {},
	string(domain.OrderStatusUnsent):    {},
}

type LedgerOrder struct {
	ClientOrderID  string
	BrokerOrderID  *string
	Symbol         string
	Market         *domain.Market
	Quantity       decimal.Decimal
	FilledQuantity decimal.Decimal
	Status         string
	Amount         *decimal.Decimal
	PlanMonth      *string // YYYY-MM-DD
	PlacedAt       string
	UpdatedAt      *string
	AvgFillPrice   *decimal.Decimal
}

func (o LedgerOrder) IsOpen() bool {
	if o.Status == DryRunStatus {
		return false
	}
	status := domain.OrderStatus(o.Status)
	return status.IsOpen()
}

func (o LedgerOrder) EffectiveAmount() decimal.Decimal {
	if o.Amount == nil || o.Status == DryRunStatus {
		return decimal.Zero
	}
	if _, isDead := deadStatuses[o.Status]; isDead {
		if o.Quantity.LessThanOrEqual(decimal.Zero) {
			return decimal.Zero
		}
		// amount * filled_quantity / quantity
		return o.Amount.Mul(o.FilledQuantity).Div(o.Quantity).Round(0)
	}
	return *o.Amount
}

type Ledger struct {
	db   *sql.DB
	path string
	// Verify が真なら、この実行で書く注文に実機検証の印を付ける（--broker-verify）。
	// 買った株は本物なので「発注済み」には数える（実際に払っている）が、
	// evaluate の集計からは外す。戦略の判断ではないため。
	Verify bool
}

// Path は台帳ファイルの置き場所。二重買付を疑う場面で人に示す。
func (l *Ledger) Path() string { return l.path }

// migrations は台帳のスキーマの履歴。版は PRAGMA user_version（storage.Migrate）。
// 列を足すときは末尾に段を足す——既存の段を書き換えても適用済みの DB には効かない。
var migrations = []storage.Migration{
	{Name: "orders+accumulation", Up: storage.Exec(`CREATE TABLE IF NOT EXISTS orders (
		client_order_id TEXT PRIMARY KEY,
		broker_order_id TEXT,
		symbol TEXT NOT NULL,
		quantity TEXT NOT NULL,
		status TEXT NOT NULL,
		reason TEXT,
		placed_at TEXT NOT NULL,
		plan_month TEXT,
		amount TEXT,
		market TEXT,
		filled_quantity TEXT,
		avg_fill_price TEXT,
		updated_at TEXT
	)`, `CREATE TABLE IF NOT EXISTS accumulation (
		symbol TEXT PRIMARY KEY,
		started_on TEXT NOT NULL
	)`)},
	// 古い台帳（初期の 7 列だけ）に後から足した列
	{Name: "orders.columns", Up: storage.AddColumns("orders", map[string]string{
		"plan_month": "TEXT", "amount": "TEXT", "market": "TEXT",
		"filled_quantity": "TEXT", "avg_fill_price": "TEXT", "updated_at": "TEXT",
	})},
	// 実機検証（docs/BROKER_VERIFY.md）の注文に印を付ける。既定 0 = 通常の注文
	{Name: "orders.verify", Up: storage.AddColumns("orders", map[string]string{
		"verify": "INTEGER NOT NULL DEFAULT 0",
	})},
}

// OpenLedger は台帳を開き、スキーマを最新に揃える。
func OpenLedger(dbPath string) (*Ledger, error) {
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

func (l *Ledger) Close() error {
	return l.db.Close()
}

func (l *Ledger) WasPlaced(clientOrderID string) bool {
	var dummy int
	query := "SELECT 1 FROM orders WHERE client_order_id = ? AND status NOT IN (?, ?, ?);"
	err := l.db.QueryRow(query, clientOrderID, DryRunStatus, string(domain.OrderStatusRejected), string(domain.OrderStatusUnsent)).Scan(&dummy)
	return err == nil
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

// RecordedIDs は台帳が知っている注文 ID を全て返す。
//
// ブローカーの約定履歴と突き合わせて「台帳に無い約定」を探すために使う。
// dry-run は実際には発注していないので除く。
func (l *Ledger) RecordedIDs() (map[string]struct{}, error) {
	rows, err := l.db.Query("SELECT client_order_id FROM orders WHERE status != ?;", DryRunStatus)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

func (l *Ledger) PlacedAmount(symbol string, month time.Time) (decimal.Decimal, error) {
	planMonth := fmt.Sprintf("%04d-%02d-01", month.Year(), month.Month())
	query := "SELECT client_order_id, broker_order_id, symbol, market, quantity, filled_quantity, status, amount, plan_month, placed_at, updated_at, avg_fill_price FROM orders WHERE symbol = ? AND plan_month = ?;"
	rows, err := l.db.Query(query, symbol, planMonth)
	if err != nil {
		return decimal.Zero, err
	}
	defer rows.Close()

	total := decimal.Zero
	for rows.Next() {
		order, err := l.scanOrder(rows)
		if err == nil {
			total = total.Add(order.EffectiveAmount())
		}
	}
	return total, nil
}

func (l *Ledger) HasOrders(symbol string, month time.Time) bool {
	planMonth := fmt.Sprintf("%04d-%02d-01", month.Year(), month.Month())
	var dummy int
	query := "SELECT 1 FROM orders WHERE symbol = ? AND plan_month = ? AND status != ? LIMIT 1;"
	err := l.db.QueryRow(query, symbol, planMonth, DryRunStatus).Scan(&dummy)
	return err == nil
}

func (l *Ledger) OpenOrders() ([]LedgerOrder, error) {
	query := "SELECT client_order_id, broker_order_id, symbol, market, quantity, filled_quantity, status, amount, plan_month, placed_at, updated_at, avg_fill_price FROM orders WHERE status != ? ORDER BY placed_at;"
	rows, err := l.db.Query(query, DryRunStatus)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []LedgerOrder
	for rows.Next() {
		order, err := l.scanOrder(rows)
		if err == nil && order.IsOpen() {
			orders = append(orders, order)
		}
	}
	return orders, nil
}

func (l *Ledger) Recent(limit int) ([]LedgerOrder, error) {
	if limit <= 0 {
		limit = 20
	}
	query := "SELECT client_order_id, broker_order_id, symbol, market, quantity, filled_quantity, status, amount, plan_month, placed_at, updated_at, avg_fill_price FROM orders ORDER BY placed_at DESC LIMIT ?;"
	rows, err := l.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []LedgerOrder
	for rows.Next() {
		order, err := l.scanOrder(rows)
		if err == nil {
			orders = append(orders, order)
		}
	}
	return orders, nil
}

func (l *Ledger) StartedOn(symbol string) *string {
	var started string
	err := l.db.QueryRow("SELECT started_on FROM accumulation WHERE symbol = ?;", symbol).Scan(&started)
	if err != nil {
		return nil
	}
	return &started
}

func (l *Ledger) MarkStarted(symbol, day string) error {
	query := "INSERT OR IGNORE INTO accumulation (symbol, started_on) VALUES (?, ?);"
	_, err := l.db.Exec(query, symbol, day)
	return err
}

func (l *Ledger) Record(req domain.OrderRequest, status string, brokerOrderID *string, planMonth *string, amount *decimal.Decimal, market *domain.Market) error {
	nowUTC := clock.NowUTC().Format(time.RFC3339)
	var amtStr *string
	if amount != nil {
		s := amount.String()
		amtStr = &s
	}
	var mktStr *string
	if market != nil {
		s := string(*market)
		mktStr = &s
	}

	query := `INSERT INTO orders (
		client_order_id, broker_order_id, symbol, quantity, status, reason, placed_at,
		plan_month, amount, market, filled_quantity, verify
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(client_order_id) DO UPDATE SET
		broker_order_id = excluded.broker_order_id,
		status = excluded.status,
		updated_at = excluded.placed_at;`

	_, err := l.db.Exec(query,
		req.ClientOrderID,
		brokerOrderID,
		req.Symbol,
		req.Quantity.String(),
		status,
		req.Reason,
		nowUTC,
		planMonth,
		amtStr,
		mktStr,
		"0",
		boolToInt(l.Verify),
	)
	return err
}

// boolToInt は SQLite に真偽を入れるための 0 / 1。
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Backup は台帳を別ファイルに複製する。
//
// 台帳は「今月いくら発注済みか」の唯一の記録で、ブローカーから再構築できない。
// 失うと次の実行で当月の予算を買い直す。単純なファイルコピーだと cron が
// 書いている最中の中途半端な状態を写しうるので、SQLite の一貫した
// スナップショット（wbcore/backup）を使う。
func (l *Ledger) Backup(destination string) (string, error) {
	return backup.BackupSQLite(l.path, destination)
}

func (l *Ledger) UpdateStatus(clientOrderID, status string, filledQty *decimal.Decimal, avgFillPrice *decimal.Decimal) error {
	return l.UpdateStatusDetail(clientOrderID, status, filledQty, avgFillPrice, nil, nil)
}

// UpdateStatusDetail はブローカーに照会した結果で約定状況を更新する。
//
// brokerOrderID と amount は nil なら変えない。amount は「発注済み」として数える額
// （株数 × 約定単価）の上書き。発注時は指値・成行の想定額で記録しているので、
// 約定額が分かった時点で置き換えないと当月の残りの計算がずれる。
func (l *Ledger) UpdateStatusDetail(
	clientOrderID, status string,
	filledQty *decimal.Decimal,
	avgFillPrice *decimal.Decimal,
	brokerOrderID *string,
	amount *decimal.Decimal,
) error {
	nowUTC := clock.NowUTC().Format(time.RFC3339)
	var filledStr *string
	if filledQty != nil {
		s := filledQty.String()
		filledStr = &s
	}
	var avgStr *string
	if avgFillPrice != nil {
		s := avgFillPrice.String()
		avgStr = &s
	}

	var amtStr *string
	if amount != nil {
		s := amount.String()
		amtStr = &s
	}

	query := `UPDATE orders SET status = ?, filled_quantity = COALESCE(?, filled_quantity),
		avg_fill_price = COALESCE(?, avg_fill_price),
		broker_order_id = COALESCE(?, broker_order_id),
		amount = COALESCE(?, amount),
		updated_at = ?
		WHERE client_order_id = ?;`
	_, err := l.db.Exec(query, status, filledStr, avgStr, brokerOrderID, amtStr, nowUTC, clientOrderID)
	return err
}

func (l *Ledger) scanOrder(rows *sql.Rows) (LedgerOrder, error) {
	var clientID, symbol, qtyStr, status, placedAt string
	var brokerID, marketStr, filledStr, amtStr, planMonth, updatedAt, avgStr *string

	err := rows.Scan(
		&clientID, &brokerID, &symbol, &marketStr, &qtyStr, &filledStr,
		&status, &amtStr, &planMonth, &placedAt, &updatedAt, &avgStr,
	)
	if err != nil {
		return LedgerOrder{}, err
	}

	qty, _ := decimal.NewFromString(strings.TrimSpace(qtyStr))
	filled := decimal.Zero
	if filledStr != nil {
		filled, _ = decimal.NewFromString(strings.TrimSpace(*filledStr))
	}

	var mkt *domain.Market
	if marketStr != nil {
		m := domain.Market(*marketStr)
		mkt = &m
	}

	var amt *decimal.Decimal
	if amtStr != nil {
		a, err := decimal.NewFromString(strings.TrimSpace(*amtStr))
		if err == nil {
			amt = &a
		}
	}

	var avgPrice *decimal.Decimal
	if avgStr != nil {
		ap, err := decimal.NewFromString(strings.TrimSpace(*avgStr))
		if err == nil {
			avgPrice = &ap
		}
	}

	return LedgerOrder{
		ClientOrderID:  clientID,
		BrokerOrderID:  brokerID,
		Symbol:         symbol,
		Market:         mkt,
		Quantity:       qty,
		FilledQuantity: filled,
		Status:         status,
		Amount:         amt,
		PlanMonth:      planMonth,
		PlacedAt:       placedAt,
		UpdatedAt:      updatedAt,
		AvgFillPrice:   avgPrice,
	}, nil
}
