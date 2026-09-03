package repo

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
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
	Symbol           string
	StopPrice        decimal.Decimal
	EntryPrice       decimal.Decimal
	CreatedOn        string
	Trailing         bool
	ATRMultiple      decimal.Decimal
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

	// schema.sql を埋め込んでテーブル作成
	ddl := `
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
	`

	if _, err := db.Exec(ddl); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to init wbjp tables: %w", err)
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

func (r *Repo) RecordOrder(runID string, req domain.OrderRequest, status string, brokerOrderID *string) error {
	now := clock.NowUTC().Format(time.RFC3339)
	var limitStr *string
	if req.LimitPrice != nil {
		s := req.LimitPrice.String()
		limitStr = &s
	}

	query := `INSERT OR REPLACE INTO orders (
		client_order_id, run_id, broker_order_id, symbol, side, order_type,
		quantity, limit_price, status, reason, placed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`

	_, err := r.db.Exec(query,
		req.ClientOrderID, runID, brokerOrderID, req.Symbol, string(req.Side), string(req.OrderType),
		req.Quantity.String(), limitStr, status, req.Reason, now,
	)
	return err
}

func (r *Repo) GetStops() (map[string]StopRecord, error) {
	rows, err := r.db.Query(`SELECT symbol, stop_price, entry_price, created_on, trailing,
		atr_multiple, highest_close, initial_stop_price, initial_quantity, scaled_out, updated_at
		FROM stops;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stops := make(map[string]StopRecord)
	for rows.Next() {
		var sym, stopStr, entryStr, createdOn, atrStr string
		var trailingInt, scaledOutInt int
		var highStr, initStopStr, initQtyStr, updatedStr *string

		if err := rows.Scan(&sym, &stopStr, &entryStr, &createdOn, &trailingInt,
			&atrStr, &highStr, &initStopStr, &initQtyStr, &scaledOutInt, &updatedStr); err != nil {
			return nil, err
		}

		sp, _ := decimal.NewFromString(stopStr)
		ep, _ := decimal.NewFromString(entryStr)
		atr, _ := decimal.NewFromString(atrStr)

		var hc, isp, iq *decimal.Decimal
		if highStr != nil {
			d, _ := decimal.NewFromString(*highStr)
			hc = &d
		}
		if initStopStr != nil {
			d, _ := decimal.NewFromString(*initStopStr)
			isp = &d
		}
		if initQtyStr != nil {
			d, _ := decimal.NewFromString(*initQtyStr)
			iq = &d
		}

		stops[sym] = StopRecord{
			Symbol:           sym,
			StopPrice:        sp,
			EntryPrice:       ep,
			CreatedOn:        createdOn,
			Trailing:         trailingInt != 0,
			ATRMultiple:      atr,
			HighestClose:     hc,
			InitialStopPrice: isp,
			InitialQuantity:  iq,
			ScaledOut:        scaledOutInt != 0,
			UpdatedAt:        updatedStr,
		}
	}

	return stops, nil
}
