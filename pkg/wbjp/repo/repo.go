package repo

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/shopspring/decimal"
)

type Repo struct {
	db *sql.DB
}

type RunRecord struct {
	RunID      string
	StartedAt  string
	FinishedAt *string
	AsOf       string
	Env        string
	Mode       string
	Equity     *decimal.Decimal
	Cash       *decimal.Decimal
	Status     string
	Error      *string
}

type StopRecord struct {
	Symbol      string
	StopPrice   decimal.Decimal
	EntryPrice  decimal.Decimal
	CreatedOn   string
	Trailing    bool
	ATRMultiple decimal.Decimal
	// TrailingPct は %トレーリングの比率。nil なら ATR 追従。
	// 保存しないと 2 日目から ATR 追従に戻ってしまう（設定した比率が初日しか効かない）。
	TrailingPct      *decimal.Decimal
	HighestClose     *decimal.Decimal
	InitialStopPrice *decimal.Decimal
	InitialQuantity  *decimal.Decimal
	ScaledOut        bool
	UpdatedAt        *string
}

func OpenRepo(dbPath string) (*Repo, error) {
	db, err := storage.OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}

	// スキーマの履歴。版は PRAGMA user_version（storage.Migrate）。
	// 列を足すときは末尾に段を足す——既存の段を書き換えても適用済みの DB には効かない
	migrations := []storage.Migration{{Name: "initial", Up: storage.Exec(`
	CREATE TABLE IF NOT EXISTS runs (
		run_id        TEXT PRIMARY KEY,
		started_at    TEXT NOT NULL,
		finished_at   TEXT,
		as_of         TEXT NOT NULL,
		env           TEXT NOT NULL,
		mode          TEXT NOT NULL,
		equity        TEXT,
		cash          TEXT,
		status        TEXT NOT NULL DEFAULT 'running',
		error         TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_runs_as_of ON runs(as_of);

	CREATE TABLE IF NOT EXISTS signals (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id        TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
		strategy      TEXT NOT NULL,
		symbol        TEXT NOT NULL,
		direction     REAL NOT NULL,
		confidence    REAL NOT NULL,
		reason        TEXT NOT NULL DEFAULT '',
		meta_json     TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_signals_run ON signals(run_id);
	CREATE INDEX IF NOT EXISTS idx_signals_symbol ON signals(symbol);

	CREATE TABLE IF NOT EXISTS combined_signals (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id             TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
		symbol             TEXT NOT NULL,
		direction          REAL NOT NULL,
		contributions_json TEXT,
		reason             TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_combined_run ON combined_signals(run_id);

	CREATE TABLE IF NOT EXISTS targets (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id    TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
		symbol    TEXT NOT NULL,
		quantity  TEXT NOT NULL,
		reason    TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_targets_run ON targets(run_id);

	CREATE TABLE IF NOT EXISTS orders (
		client_order_id  TEXT PRIMARY KEY,
		run_id           TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
		broker_order_id  TEXT,
		symbol           TEXT NOT NULL,
		side             TEXT NOT NULL,
		order_type       TEXT NOT NULL,
		quantity         TEXT NOT NULL,
		limit_price      TEXT,
		status           TEXT NOT NULL,
		filled_quantity  TEXT NOT NULL DEFAULT '0',
		avg_fill_price   TEXT,
		reason           TEXT NOT NULL DEFAULT '',
		placed_at        TEXT NOT NULL,
		updated_at       TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_orders_run ON orders(run_id);
	CREATE INDEX IF NOT EXISTS idx_orders_symbol ON orders(symbol);
	CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);

	CREATE TABLE IF NOT EXISTS fills (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		client_order_id  TEXT NOT NULL,
		run_id           TEXT,
		symbol           TEXT NOT NULL,
		side             TEXT NOT NULL,
		quantity         TEXT NOT NULL,
		price            TEXT NOT NULL,
		fee              TEXT NOT NULL DEFAULT '0',
		filled_at        TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_fills_order ON fills(client_order_id);

	CREATE TABLE IF NOT EXISTS stops (
		symbol        TEXT PRIMARY KEY,
		stop_price    TEXT NOT NULL,
		entry_price   TEXT NOT NULL,
		created_on    TEXT NOT NULL,
		trailing      INTEGER NOT NULL DEFAULT 0,
		atr_multiple  TEXT NOT NULL DEFAULT '2.0',
		highest_close TEXT,
		initial_stop_price TEXT,
		initial_quantity   TEXT,
		scaled_out    INTEGER NOT NULL DEFAULT 0,
		updated_at    TEXT
	);

	CREATE TABLE IF NOT EXISTS risk_events (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id    TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
		symbol    TEXT NOT NULL,
		reason    TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_risk_run ON risk_events(run_id);

	CREATE TABLE IF NOT EXISTS position_snapshots (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
		as_of       TEXT NOT NULL,
		symbol      TEXT NOT NULL,
		quantity    TEXT NOT NULL,
		cost_price  TEXT NOT NULL,
		last_price  TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_snapshots_as_of ON position_snapshots(as_of);
	`)},
		// placed_at は UTC。JST の日付で数える（当日の発注件数・当日買付）のに
		// substr(placed_at) を使うと 00:00〜08:59 JST の注文が前日に落ちる。
		// JST の日付を別の列に持ち、既存行は placed_at から埋める
		{Name: "orders.placed_on", Up: func(tx *sql.Tx) error {
			if err := storage.AddColumn(tx, "orders", "placed_on", "TEXT"); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE orders
				SET placed_on = substr(datetime(placed_at, '+9 hours'), 1, 10)
				WHERE placed_on IS NULL AND placed_at != '';`)
			return err
		}},
		// %トレーリングの比率。無いと翌日から ATR 追従に戻る
		{Name: "stops.trailing_pct", Up: storage.AddColumns("stops", map[string]string{
			"trailing_pct": "TEXT",
		})},
	}

	if err := storage.Migrate(db, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("wbjp の台帳 %s: %w", dbPath, err)
	}

	return &Repo{db: db}, nil
}

func (r *Repo) Close() error {
	return r.db.Close()
}

func (r *Repo) StartRun(runID, asOf, env, mode string) error {
	now := clock.NowUTC().Format(time.RFC3339)
	query := `INSERT INTO runs (run_id, started_at, as_of, env, mode, status)
		VALUES (?, ?, ?, ?, ?, 'running');`
	_, err := r.db.Exec(query, runID, now, asOf, env, mode)
	return err
}

func (r *Repo) FinishRun(runID, status string, equity, cash *decimal.Decimal, errStr *string) error {
	now := clock.NowUTC().Format(time.RFC3339)
	var eqStr, cashStr *string
	if equity != nil {
		s := equity.String()
		eqStr = &s
	}
	if cash != nil {
		s := cash.String()
		cashStr = &s
	}
	query := `UPDATE runs SET finished_at = ?, status = ?, equity = ?, cash = ?, error = ?
		WHERE run_id = ?;`
	_, err := r.db.Exec(query, now, status, eqStr, cashStr, errStr, runID)
	return err
}

func (r *Repo) RecordSignals(runID string, signals []domain.Signal) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO signals (run_id, strategy, symbol, direction, confidence, reason, meta_json)
		VALUES (?, ?, ?, ?, ?, ?, ?);`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, s := range signals {
		metaBytes, _ := json.Marshal(s.Meta)
		if _, err := stmt.Exec(runID, s.Strategy, s.Symbol, s.Direction, s.Confidence, s.Reason, string(metaBytes)); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (r *Repo) RecordCombinedSignals(runID string, signals []domain.CombinedSignal) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO combined_signals (run_id, symbol, direction, contributions_json, reason)
		VALUES (?, ?, ?, ?, ?);`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, s := range signals {
		contBytes, _ := json.Marshal(s.Contributions)
		if _, err := stmt.Exec(runID, s.Symbol, s.Direction, string(contBytes), s.Reason); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (r *Repo) RecordTargets(runID string, targets []domain.TargetPosition) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO targets (run_id, symbol, quantity, reason)
		VALUES (?, ?, ?, ?);`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, t := range targets {
		if _, err := stmt.Exec(runID, t.Symbol, t.Quantity.String(), t.Reason); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RecordOrder は注文を台帳に書く。同じ ID が既にあれば発注の記録だけを書き換える。
//
// 同じ ID で書き直すのは、拒否・未送信（REJECTED / UNSENT）や dry-run の行を
// 同じ日にもう一度出すとき。約定の記録（filled_quantity / avg_fill_price）は
// 触らない——INSERT OR REPLACE だと約定済みの行を 0 株に巻き戻す。
func (r *Repo) RecordOrder(runID string, req domain.OrderRequest, status string, brokerOrderID *string) error {
	return r.recordOrderAt(runID, req, status, brokerOrderID, clock.NowUTC())
}

func (r *Repo) recordOrderAt(runID string, req domain.OrderRequest, status string, brokerOrderID *string, now time.Time) error {
	var limitStr *string
	if req.LimitPrice != nil {
		s := req.LimitPrice.String()
		limitStr = &s
	}

	query := `INSERT INTO orders (
		client_order_id, run_id, broker_order_id, symbol, side, order_type,
		quantity, limit_price, status, reason, placed_at, placed_on
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(client_order_id) DO UPDATE SET
		run_id = excluded.run_id,
		broker_order_id = excluded.broker_order_id,
		limit_price = excluded.limit_price,
		status = excluded.status,
		reason = excluded.reason,
		placed_at = excluded.placed_at,
		placed_on = excluded.placed_on,
		updated_at = excluded.placed_at;`

	_, err := r.db.Exec(query,
		req.ClientOrderID, runID, brokerOrderID, req.Symbol, string(req.Side), string(req.OrderType),
		req.Quantity.String(), limitStr, status, req.Reason,
		now.Format(time.RFC3339), dayJST(now),
	)
	return err
}

// dayJST は UTC の時刻を JST の日付（YYYY-MM-DD）にする。
func dayJST(t time.Time) string {
	return clock.ToZone(t, clock.Tokyo).Format("2006-01-02")
}

// GetStops は保存済みのストップを全て読む。
//
// 数値が壊れていれば失敗にする。黙って 0 にすると「ストップ 0 円」が
// 抵触しないまま残り、損切りが二度と効かない。
func (r *Repo) GetStops() (map[string]StopRecord, error) {
	rows, err := r.db.Query(`SELECT symbol, stop_price, entry_price, created_on, trailing,
		atr_multiple, trailing_pct, highest_close, initial_stop_price, initial_quantity, scaled_out, updated_at
		FROM stops;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stops := make(map[string]StopRecord)
	for rows.Next() {
		var sym, stopStr, entryStr, createdOn, atrStr string
		var trailingInt, scaledOutInt int
		var pctStr, highStr, initStopStr, initQtyStr, updatedStr *string

		if err := rows.Scan(&sym, &stopStr, &entryStr, &createdOn, &trailingInt,
			&atrStr, &pctStr, &highStr, &initStopStr, &initQtyStr, &scaledOutInt, &updatedStr); err != nil {
			return nil, err
		}

		rec := StopRecord{
			Symbol:    sym,
			CreatedOn: createdOn,
			Trailing:  trailingInt != 0,
			ScaledOut: scaledOutInt != 0,
			UpdatedAt: updatedStr,
		}
		fields := []struct {
			name string
			dst  *decimal.Decimal
			src  string
		}{
			{"stop_price", &rec.StopPrice, stopStr},
			{"entry_price", &rec.EntryPrice, entryStr},
			{"atr_multiple", &rec.ATRMultiple, atrStr},
		}
		for _, f := range fields {
			d, err := decimal.NewFromString(f.src)
			if err != nil {
				return nil, fmt.Errorf("ストップ %s の %s が数値ではありません (%q): %w", sym, f.name, f.src, err)
			}
			*f.dst = d
		}
		optional := []struct {
			name string
			dst  **decimal.Decimal
			src  *string
		}{
			{"trailing_pct", &rec.TrailingPct, pctStr},
			{"highest_close", &rec.HighestClose, highStr},
			{"initial_stop_price", &rec.InitialStopPrice, initStopStr},
			{"initial_quantity", &rec.InitialQuantity, initQtyStr},
		}
		for _, f := range optional {
			d, err := parseDecimalPtrStrict(f.src)
			if err != nil {
				return nil, fmt.Errorf("ストップ %s の %s が数値ではありません (%q): %w", sym, f.name, *f.src, err)
			}
			*f.dst = d
		}
		stops[sym] = rec
	}

	return stops, rows.Err()
}

// execer は *sql.DB と *sql.Tx の共通部分。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func (r *Repo) SaveStop(rec StopRecord) error {
	return saveStop(r.db, rec)
}

func saveStop(db execer, rec StopRecord) error {
	now := clock.NowUTC().Format(time.RFC3339)
	var pctStr, hcStr, initStopStr, initQtyStr *string
	if rec.TrailingPct != nil {
		s := rec.TrailingPct.String()
		pctStr = &s
	}
	if rec.HighestClose != nil {
		s := rec.HighestClose.String()
		hcStr = &s
	}
	if rec.InitialStopPrice != nil {
		s := rec.InitialStopPrice.String()
		initStopStr = &s
	}
	if rec.InitialQuantity != nil {
		s := rec.InitialQuantity.String()
		initQtyStr = &s
	}

	trailingInt := 0
	if rec.Trailing {
		trailingInt = 1
	}
	scaledOutInt := 0
	if rec.ScaledOut {
		scaledOutInt = 1
	}

	query := `INSERT OR REPLACE INTO stops (
		symbol, stop_price, entry_price, created_on, trailing, atr_multiple, trailing_pct,
		highest_close, initial_stop_price, initial_quantity, scaled_out, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`

	_, err := db.Exec(query,
		rec.Symbol, rec.StopPrice.String(), rec.EntryPrice.String(), rec.CreatedOn,
		trailingInt, rec.ATRMultiple.String(), pctStr, hcStr, initStopStr, initQtyStr, scaledOutInt, now,
	)
	return err
}

func (r *Repo) DeleteStop(symbol string) error {
	_, err := r.db.Exec("DELETE FROM stops WHERE symbol = ?;", symbol)
	return err
}

// SyncStops は台帳のストップを records の中身に揃える。records に無い銘柄の行は消す。
//
// 手仕舞った銘柄のストップを残すと、次に同じ銘柄を建てたとき古いストップ
// （古い建値・古い作成日・古い最高値）を引き継ぎ、R の計算が狂い、
// 建てた直後に時間切れで手仕舞う。全部を 1 トランザクションで書き、
// 途中で失敗したら丸ごと戻す。
func (r *Repo) SyncStops(records map[string]StopRecord) error {
	return r.withTx(func(tx *sql.Tx) error {
		for _, rec := range records {
			if err := saveStop(tx, rec); err != nil {
				return err
			}
		}
		rows, err := tx.Query("SELECT symbol FROM stops;")
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var sym string
			if err := rows.Scan(&sym); err != nil {
				rows.Close()
				return err
			}
			if _, keep := records[sym]; !keep {
				stale = append(stale, sym)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, sym := range stale {
			if _, err := tx.Exec("DELETE FROM stops WHERE symbol = ?;", sym); err != nil {
				return err
			}
		}
		return nil
	})
}

// WasPlaced はその注文 ID を既に発注済みかを返す。
//
// 送信後・記録前に落ちた場合の再送を止めるための鍵。拒否された注文は
// 「出していない」と同じ扱いにして、次回もう一度出せるようにする。
//
// 台帳が読めないときはエラー。「読めない」を「出していない」と読むと、
// プロセスをまたいだ二重発注の唯一の柵が外れる。
func (r *Repo) WasPlaced(clientOrderID string) (bool, error) {
	var dummy int
	err := r.db.QueryRow(
		"SELECT 1 FROM orders WHERE client_order_id = ? AND status NOT IN (?, ?, ?);",
		clientOrderID, "dry_run", string(domain.OrderStatusRejected), string(domain.OrderStatusUnsent),
	).Scan(&dummy)
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
func (r *Repo) BrokerOrderIDs() (map[string]struct{}, error) {
	rows, err := r.db.Query("SELECT broker_order_id FROM orders WHERE broker_order_id IS NOT NULL AND broker_order_id != ''")
	if err != nil {
		return nil, err
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

// OrdersToday はその日に実際に発注した件数。
//
// max_orders_per_day はプロセスをまたいで効かないと意味がない。
// 実行ごとに 0 から数え直すと、1日に何度 run しても上限に達しない。
func (r *Repo) OrdersToday(dayJST string) (int, error) {
	var count int
	err := r.db.QueryRow(
		"SELECT COUNT(*) FROM orders WHERE placed_on = ? AND status != ?;",
		dayJST, "dry_run",
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// BoughtToday はその日に買い付けた銘柄。現物の差金決済回避に使う。
func (r *Repo) BoughtToday(dayJST string) (map[string]struct{}, error) {
	rows, err := r.db.Query(
		`SELECT DISTINCT symbol FROM orders
		 WHERE placed_on = ? AND side = ? AND status != ?
		   AND CAST(filled_quantity AS REAL) > 0;`,
		dayJST, string(domain.SideBuy), "dry_run",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			return nil, err
		}
		out[sym] = struct{}{}
	}
	return out, rows.Err()
}

// PendingBuyValue は未約定の買い注文が押さえている金額を銘柄ごとに返す。
//
// リスク検査はこれを見ないと、同じ資金を何度も当てにして上限を超える。
func (r *Repo) PendingBuyValue(basePrices map[string]decimal.Decimal) (map[string]decimal.Decimal, error) {
	orders, err := r.UnresolvedOrders()
	if err != nil {
		return nil, err
	}

	out := make(map[string]decimal.Decimal)
	for _, o := range orders {
		if o.Side != domain.SideBuy {
			continue
		}
		remaining := o.Quantity.Sub(o.FilledQuantity)
		if remaining.LessThanOrEqual(decimal.Zero) {
			continue
		}
		// 指値があればそれを、無ければ直近値を単価にする。
		price := decimal.Zero
		if o.LimitPrice != nil {
			price = *o.LimitPrice
		} else if p, ok := basePrices[o.Symbol]; ok {
			price = p
		}
		out[o.Symbol] = out[o.Symbol].Add(remaining.Mul(price))
	}
	return out, nil
}

// OrderRecord は台帳に記録された注文。
type OrderRecord struct {
	ClientOrderID  string
	RunID          string
	BrokerOrderID  *string
	Symbol         string
	Side           domain.Side
	OrderType      domain.OrderType
	Quantity       decimal.Decimal
	LimitPrice     *decimal.Decimal
	Status         domain.OrderStatus
	FilledQuantity decimal.Decimal
	AvgFillPrice   *decimal.Decimal
	Reason         string
	PlacedAt       string
}

// UnresolvedOrders はまだ終了状態になっていない注文。
//
// 次の実行でブローカーに照会し、約定・失効を台帳へ反映するために使う。
func (r *Repo) UnresolvedOrders() ([]OrderRecord, error) {
	rows, err := r.db.Query(
		orderSelect+`
		 WHERE status NOT IN (?, ?, ?, ?, ?, ?)
		 ORDER BY placed_at;`,
		string(domain.OrderStatusFilled), string(domain.OrderStatusCancelled),
		string(domain.OrderStatusRejected), string(domain.OrderStatusExpired),
		string(domain.OrderStatusUnsent), "dry_run",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OrderRecord
	for rows.Next() {
		rec, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// GetOrder は注文 1 件を client_order_id で引く。無ければ nil。
func (r *Repo) GetOrder(clientOrderID string) (*OrderRecord, error) {
	row := r.db.QueryRow(orderSelect+` WHERE client_order_id = ?;`, clientOrderID)
	rec, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

const orderSelect = `SELECT client_order_id, run_id, broker_order_id, symbol, side, order_type,
		        quantity, limit_price, status, filled_quantity, avg_fill_price, reason, placed_at
		 FROM orders`

func scanOrder(src scanner) (*OrderRecord, error) {
	var rec OrderRecord
	var side, orderType, status, quantity, filledQty string
	var limitPrice, avgFillPrice *string
	if err := src.Scan(&rec.ClientOrderID, &rec.RunID, &rec.BrokerOrderID, &rec.Symbol,
		&side, &orderType, &quantity, &limitPrice, &status, &filledQty,
		&avgFillPrice, &rec.Reason, &rec.PlacedAt); err != nil {
		return nil, err
	}
	rec.Side = domain.Side(side)
	rec.OrderType = domain.OrderType(orderType)
	rec.Status = domain.OrderStatus(status)
	var err error
	if rec.Quantity, err = decimal.NewFromString(quantity); err != nil {
		return nil, fmt.Errorf("注文 %s の quantity が数値ではありません (%q): %w", rec.ClientOrderID, quantity, err)
	}
	if rec.FilledQuantity, err = decimal.NewFromString(filledQty); err != nil {
		return nil, fmt.Errorf("注文 %s の filled_quantity が数値ではありません (%q): %w", rec.ClientOrderID, filledQty, err)
	}
	if rec.LimitPrice, err = parseDecimalPtrStrict(limitPrice); err != nil {
		return nil, fmt.Errorf("注文 %s の limit_price が数値ではありません: %w", rec.ClientOrderID, err)
	}
	if rec.AvgFillPrice, err = parseDecimalPtrStrict(avgFillPrice); err != nil {
		return nil, fmt.Errorf("注文 %s の avg_fill_price が数値ではありません: %w", rec.ClientOrderID, err)
	}
	return &rec, nil
}

// UpdateOrderStatus は状態だけを書き換える。約定の記録（filled_quantity 等）は触らない。
//
// 取消の記録に使う。UpdateOrder は約定数量を必ず受けるので、部分約定した注文を
// 取り消したときに約定分を 0 に戻してしまう。
func (r *Repo) UpdateOrderStatus(clientOrderID string, status domain.OrderStatus) error {
	_, err := r.db.Exec(
		`UPDATE orders SET status = ?, updated_at = ? WHERE client_order_id = ?;`,
		string(status), clock.NowUTC().Format(time.RFC3339), clientOrderID,
	)
	return err
}

// UpdateOrder は注文の約定状況を書き戻す。
func (r *Repo) UpdateOrder(clientOrderID string, status domain.OrderStatus,
	filledQty decimal.Decimal, avgFillPrice *decimal.Decimal, brokerOrderID *string) error {
	var avg *string
	if avgFillPrice != nil {
		s := avgFillPrice.String()
		avg = &s
	}
	_, err := r.db.Exec(
		`UPDATE orders
		 SET status = ?, filled_quantity = ?, avg_fill_price = COALESCE(?, avg_fill_price),
		     broker_order_id = COALESCE(?, broker_order_id), updated_at = ?
		 WHERE client_order_id = ?;`,
		string(status), filledQty.String(), avg, brokerOrderID,
		clock.NowUTC().Format(time.RFC3339), clientOrderID,
	)
	return err
}

// parseDecimalPtr は NULL 許容の数値文字列を Decimal のポインタにする。壊れていれば nil。
//
// 実行の記録（equity / cash）のような表示用の値にだけ使う。判断に使う値は
// parseDecimalPtrStrict で読み、壊れていたら失敗にする。
func parseDecimalPtr(s *string) *decimal.Decimal {
	d, err := parseDecimalPtrStrict(s)
	if err != nil {
		return nil
	}
	return d
}

// parseDecimalPtrStrict は NULL 許容の数値文字列を Decimal のポインタにする。壊れていればエラー。
func parseDecimalPtrStrict(s *string) (*decimal.Decimal, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	d, err := decimal.NewFromString(*s)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ---- 調査（explain / runs） ----------------------------------------------

// withTx はまとめて書き、途中で失敗したら丸ごと戻す。
//
// 判断の記録は「シグナルだけ入って目標が入っていない」中途半端な状態になると、
// 後から explain で追ったときに何が起きたのか分からなくなる。
func (r *Repo) withTx(fn func(tx *sql.Tx) error) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// GetRun は 1 回の実行を引く。無ければ nil。
func (r *Repo) GetRun(runID string) (*RunRecord, error) {
	row := r.db.QueryRow(
		`SELECT run_id, started_at, finished_at, as_of, env, mode, equity, cash, status, error
		 FROM runs WHERE run_id = ?;`, runID)

	rec, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// RecentRuns は新しい順に実行を並べる。
func (r *Repo) RecentRuns(limit int) ([]RunRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.Query(
		`SELECT run_id, started_at, finished_at, as_of, env, mode, equity, cash, status, error
		 FROM runs ORDER BY started_at DESC LIMIT ?;`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RunRecord{}
	for rows.Next() {
		rec, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// scanner は *sql.Row と *sql.Rows のどちらでも受けるための最小の口。
type scanner interface {
	Scan(dest ...any) error
}

func scanRun(src scanner) (*RunRecord, error) {
	var rec RunRecord
	var equity, cash *string
	if err := src.Scan(&rec.RunID, &rec.StartedAt, &rec.FinishedAt, &rec.AsOf, &rec.Env,
		&rec.Mode, &equity, &cash, &rec.Status, &rec.Error); err != nil {
		return nil, err
	}
	rec.Equity = parseDecimalPtr(equity)
	rec.Cash = parseDecimalPtr(cash)
	return &rec, nil
}

// ExplainSection は explain の 1 区画（テーブル 1 つぶん）。
//
// マップではなく並びで返すのは、表示の順序（シグナル → 合成 → 目標 → 注文 →
// 拒否理由）そのものが「なぜそうなったか」の筋道だから。
type ExplainSection struct {
	Name    string
	Columns []string
	Rows    []map[string]any
}

// explainTables は explain が辿るテーブル。判断の流れの順に並べる。
var explainTables = []string{"signals", "combined_signals", "targets", "orders", "risk_events"}

// Explain は 1 回の実行を丸ごと取り出す。
//
// 「なぜこの注文が出たのか」「なぜ出なかったのか」を、シグナルから拒否理由まで
// 一覧で追える。
func (r *Repo) Explain(runID string) ([]ExplainSection, error) {
	out := make([]ExplainSection, 0, len(explainTables))
	for _, table := range explainTables {
		// テーブル名は上の定数リストからしか来ないので、埋め込んでも注入にならない
		rows, err := r.db.Query("SELECT * FROM "+table+" WHERE run_id = ?;", runID)
		if err != nil {
			return nil, err
		}
		section, err := scanSection(table, rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, section)
	}
	return out, nil
}

func scanSection(name string, rows *sql.Rows) (ExplainSection, error) {
	columns, err := rows.Columns()
	if err != nil {
		return ExplainSection{}, err
	}
	section := ExplainSection{Name: name, Columns: columns, Rows: []map[string]any{}}

	for rows.Next() {
		cells := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range cells {
			pointers[i] = &cells[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return ExplainSection{}, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			// []byte のままだと JSON で base64 になるので文字列に直す
			if raw, ok := cells[i].([]byte); ok {
				row[column] = string(raw)
			} else {
				row[column] = cells[i]
			}
		}
		section.Rows = append(section.Rows, row)
	}
	return section, rows.Err()
}

// ---- 判断の記録 -----------------------------------------------------------

// RecordRiskEvents はリスク検査で弾いた銘柄と理由を残す。
//
// 「出さなかった」も判断のうち。残しておかないと、後から explain で
// 注文が無かった理由を説明できない。
func (r *Repo) RecordRiskEvents(runID string, rejected map[string]string) error {
	if len(rejected) == 0 {
		return nil
	}
	now := clock.NowUTC().Format(time.RFC3339)

	// 実行ごとの並びを安定させる（差分を取りやすくするため）
	symbols := make([]string, 0, len(rejected))
	for symbol := range rejected {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)

	return r.withTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(
			`INSERT INTO risk_events (run_id, symbol, reason, created_at) VALUES (?, ?, ?, ?);`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, symbol := range symbols {
			if _, err := stmt.Exec(runID, symbol, rejected[symbol], now); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordSnapshot はその時点の建玉を残す。asOf は YYYY-MM-DD。
func (r *Repo) RecordSnapshot(runID, asOf string, positions []domain.Position) error {
	if len(positions) == 0 {
		return nil
	}
	return r.withTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(
			`INSERT INTO position_snapshots (run_id, as_of, symbol, quantity, cost_price, last_price)
			 VALUES (?, ?, ?, ?, ?, ?);`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, p := range positions {
			if _, err := stmt.Exec(runID, asOf, p.Symbol,
				p.Quantity.String(), p.CostPrice.String(), p.LastPrice.String()); err != nil {
				return err
			}
		}
		return nil
	})
}
