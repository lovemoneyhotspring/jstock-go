package ledger

import (
	"database/sql"
	"errors"
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

// WasPlaced はその注文 ID を既に発注済みかを返す。
//
// 送信後・記録前に落ちた場合の再送を止めるための鍵。拒否・未送信・dry-run の
// 注文は「出していない」と同じ扱いにして、次回もう一度出せるようにする。
//
// 台帳が読めないときはエラー。「読めない」を「出していない」と読むと、
// プロセスをまたいだ二重発注の唯一の柵が外れる。
func (l *Ledger) WasPlaced(clientOrderID string) (bool, error) {
	var dummy int
	query := "SELECT 1 FROM orders WHERE client_order_id = ? AND status NOT IN (?, ?, ?);"
	err := l.db.QueryRow(query, clientOrderID, DryRunStatus, string(domain.OrderStatusRejected), string(domain.OrderStatusUnsent)).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("注文 %s が発注済みかを確かめられません: %w", clientOrderID, err)
	}
	return true, nil
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

// PlacedAmount はその銘柄・計画月に「発注済み」として数える額の合計。
//
// 読めない行が 1 つでもあればエラーにする。行を飛ばして小さな額を返すと、
// 当月の差額が大きく出て同じ月の予算をもう一度買う（2026-09-24 のレビュー A3）。
func (l *Ledger) PlacedAmount(symbol string, month time.Time) (decimal.Decimal, error) {
	planMonth := fmt.Sprintf("%04d-%02d-01", month.Year(), month.Month())
	query := "SELECT " + orderColumns + " FROM orders WHERE symbol = ? AND plan_month = ?;"
	rows, err := l.db.Query(query, symbol, planMonth)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s の %s の発注済み額を読めません: %w", symbol, planMonth, err)
	}
	defer rows.Close()

	total := decimal.Zero
	for rows.Next() {
		order, err := l.scanOrder(rows)
		if err != nil {
			return decimal.Zero, fmt.Errorf("%s の %s の発注済み額を読めません: %w", symbol, planMonth, err)
		}
		total = total.Add(order.EffectiveAmount())
	}
	if err := rows.Err(); err != nil {
		return decimal.Zero, fmt.Errorf("%s の %s の発注済み額を読めません: %w", symbol, planMonth, err)
	}
	return total, nil
}

// HasOrders はその銘柄・計画月に dry-run 以外の注文記録があるか（前月からの繰り越しの条件）。
// 読めないときはエラー（「無い」と読むと繰り越しの有無が黙って変わる）。
func (l *Ledger) HasOrders(symbol string, month time.Time) (bool, error) {
	planMonth := fmt.Sprintf("%04d-%02d-01", month.Year(), month.Month())
	var dummy int
	query := "SELECT 1 FROM orders WHERE symbol = ? AND plan_month = ? AND status != ? LIMIT 1;"
	err := l.db.QueryRow(query, symbol, planMonth, DryRunStatus).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s の %s の注文記録を読めません: %w", symbol, planMonth, err)
	}
	return true, nil
}

// OpenOrders は結果の確定していない注文（dry-run を除く）。
// 読めない行はエラーにする——黙って飛ばすと、その注文は照会も判定もされないまま残る。
func (l *Ledger) OpenOrders() ([]LedgerOrder, error) {
	query := "SELECT " + orderColumns + " FROM orders WHERE status != ? ORDER BY placed_at;"
	rows, err := l.db.Query(query, DryRunStatus)
	if err != nil {
		return nil, fmt.Errorf("台帳の未確定の注文を読めません: %w", err)
	}
	defer rows.Close()

	var orders []LedgerOrder
	for rows.Next() {
		order, err := l.scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("台帳の未確定の注文を読めません: %w", err)
		}
		if order.IsOpen() {
			orders = append(orders, order)
		}
	}
	return orders, rows.Err()
}

// PendingSymbols は送信結果不明（PENDING）の注文が残っている銘柄。
//
// 届いたかどうか分からない注文がある銘柄に次の注文を出すと、届いていたとき二重買付になる。
// run はこの銘柄を発注せず、人（か AI）が `accum pending resolve` で確定するのを待つ。
func (l *Ledger) PendingSymbols() (map[string][]string, error) {
	rows, err := l.db.Query("SELECT symbol, client_order_id FROM orders WHERE status = ? ORDER BY placed_at;",
		string(domain.OrderStatusPending))
	if err != nil {
		return nil, fmt.Errorf("台帳の送信結果不明の注文を読めません: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var symbol, id string
		if err := rows.Scan(&symbol, &id); err != nil {
			return nil, fmt.Errorf("台帳の送信結果不明の注文を読めません: %w", err)
		}
		out[symbol] = append(out[symbol], id)
	}
	return out, rows.Err()
}

// BrokerOrderIDsPlacedBetween は [start, end) に送って注文番号まで分かっている注文の番号。
//
// 送信結果不明の注文を当日の注文一覧で判定するとき、一覧が生きているかの目印にする
// （reconcile.Options.Expected）。dry-run は送っていないので除く。
func (l *Ledger) BrokerOrderIDsPlacedBetween(start, end time.Time) (map[string]struct{}, error) {
	rows, err := l.db.Query(`SELECT broker_order_id, placed_at FROM orders
		WHERE broker_order_id IS NOT NULL AND broker_order_id != '' AND status != ?;`, DryRunStatus)
	if err != nil {
		return nil, fmt.Errorf("台帳の注文番号を読めません: %w", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id, placedAt string
		if err := rows.Scan(&id, &placedAt); err != nil {
			return nil, fmt.Errorf("台帳の注文番号を読めません: %w", err)
		}
		at, err := time.Parse(time.RFC3339, placedAt)
		if err != nil || at.Before(start) || !at.Before(end) {
			continue
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

func (l *Ledger) Recent(limit int) ([]LedgerOrder, error) {
	if limit <= 0 {
		limit = 20
	}
	query := "SELECT " + orderColumns + " FROM orders ORDER BY placed_at DESC LIMIT ?;"
	rows, err := l.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []LedgerOrder
	for rows.Next() {
		order, err := l.scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

// StartedOn は積立の開始日（YYYY-MM-DD）。記録が無ければ nil。
// 読めないときはエラー（「無い」と読むと開始月の日割りが黙って外れる）。
func (l *Ledger) StartedOn(symbol string) (*string, error) {
	var started string
	err := l.db.QueryRow("SELECT started_on FROM accumulation WHERE symbol = ?;", symbol).Scan(&started)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s の積立の開始日を読めません: %w", symbol, err)
	}
	return &started, nil
}

// FirstOrderDay はその銘柄で最初に dry-run 以外の注文を記録した日（loc の暦日、YYYY-MM-DD）。
// 注文が無ければ nil。
//
// 開始日の記録（accumulation）が無いまま注文だけある台帳の開始日に使う。MarkStarted を
// 呼ぶ経路が無かった間（dbd3040 から 2026-09-24 まで）に発注・取り込みした銘柄は、
// 今日を開始日とすると既に積み立てている月を日割りしてしまう。
func (l *Ledger) FirstOrderDay(symbol string, loc *time.Location) (*string, error) {
	var first sql.NullString
	err := l.db.QueryRow("SELECT MIN(placed_at) FROM orders WHERE symbol = ? AND status != ?;",
		symbol, DryRunStatus).Scan(&first)
	if err != nil {
		return nil, fmt.Errorf("%s の最初の注文日を読めません: %w", symbol, err)
	}
	if !first.Valid || first.String == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, first.String)
	if err != nil {
		return nil, fmt.Errorf("%s の最初の注文日 %q を読めません: %w", symbol, first.String, err)
	}
	day := at.In(loc).Format("2006-01-02")
	return &day, nil
}

// MarkStarted は積立の開始日を記録する。すでにあれば変えない（最初の日が開始日）。
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

	// 同じ client_order_id の行（同日・同株数の dry-run 行、REJECTED / UNSENT の行）へ
	// PENDING を記録し直すのは「これから送り直す」とき。placed_at を付け直さないと、
	// 照合（SyncOrderStatus・reconcile）の送信直後の猶予（UnconfirmedGrace）が古い行の
	// 時刻から数えられ、送った直後の注文を「届いていない」（UNSENT）として送り直しうる。
	// 送った内容（株数・金額・市場・verify の印）もこの回のものに揃える。
	// 受理・拒否などの記録し直し（PENDING 以外）は送信時刻を変えない。
	pending := string(domain.OrderStatusPending)
	query := `INSERT INTO orders (
		client_order_id, broker_order_id, symbol, quantity, status, reason, placed_at,
		plan_month, amount, market, filled_quantity, verify
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(client_order_id) DO UPDATE SET
		broker_order_id = excluded.broker_order_id,
		status = excluded.status,
		placed_at = CASE WHEN excluded.status = ? THEN excluded.placed_at ELSE orders.placed_at END,
		quantity = CASE WHEN excluded.status = ? THEN excluded.quantity ELSE orders.quantity END,
		reason = CASE WHEN excluded.status = ? THEN excluded.reason ELSE orders.reason END,
		plan_month = CASE WHEN excluded.status = ? THEN excluded.plan_month ELSE orders.plan_month END,
		amount = CASE WHEN excluded.status = ? THEN excluded.amount ELSE orders.amount END,
		market = CASE WHEN excluded.status = ? THEN excluded.market ELSE orders.market END,
		verify = CASE WHEN excluded.status = ? THEN excluded.verify ELSE orders.verify END,
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
		pending, pending, pending, pending, pending, pending, pending,
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

// orderColumns は scanOrder が読む列の並び。
const orderColumns = "client_order_id, broker_order_id, symbol, market, quantity, filled_quantity, status, amount, plan_month, placed_at, updated_at, avg_fill_price"

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

	// 数値の列が読めない行は 0 や nil に倒さずエラーにする。額が nil なら「発注済み」は 0 と
	// 数えられ、同じ月の予算をもう一度買う
	qty, err := decimal.NewFromString(strings.TrimSpace(qtyStr))
	if err != nil {
		return LedgerOrder{}, fmt.Errorf("注文 %s の quantity %q を読めません: %w", clientID, qtyStr, err)
	}
	filled := decimal.Zero
	if filledStr != nil {
		filled, err = decimal.NewFromString(strings.TrimSpace(*filledStr))
		if err != nil {
			return LedgerOrder{}, fmt.Errorf("注文 %s の filled_quantity %q を読めません: %w", clientID, *filledStr, err)
		}
	}

	var mkt *domain.Market
	if marketStr != nil {
		m := domain.Market(*marketStr)
		mkt = &m
	}

	var amt *decimal.Decimal
	if amtStr != nil {
		a, err := decimal.NewFromString(strings.TrimSpace(*amtStr))
		if err != nil {
			return LedgerOrder{}, fmt.Errorf("注文 %s の amount %q を読めません: %w", clientID, *amtStr, err)
		}
		amt = &a
	}

	var avgPrice *decimal.Decimal
	if avgStr != nil {
		ap, err := decimal.NewFromString(strings.TrimSpace(*avgStr))
		if err != nil {
			return LedgerOrder{}, fmt.Errorf("注文 %s の avg_fill_price %q を読めません: %w", clientID, *avgStr, err)
		}
		avgPrice = &ap
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
