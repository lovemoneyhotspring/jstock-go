package archive

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

const ledgerSchema = `
CREATE TABLE IF NOT EXISTS ingest (
  endpoint    TEXT NOT NULL,
  target      TEXT NOT NULL,
  source      TEXT NOT NULL,
  fetched_utc TEXT NOT NULL,
  rows        INTEGER NOT NULL,
  changed     INTEGER NOT NULL,
  digest      TEXT NOT NULL,
  run_id      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (endpoint, target, fetched_utc)
);
CREATE INDEX IF NOT EXISTS ingest_latest ON ingest (endpoint, target, fetched_utc DESC);`

// stampLayout は台帳に入れる時刻の表記。文字列比較で時系列順になるよう固定。
const stampLayout = "2006-01-02T15:04:05.000000Z07:00"

// IngestRecord は台帳の 1 行。
type IngestRecord struct {
	Endpoint   string
	Target     string
	Source     string
	FetchedUTC time.Time
	Rows       int
	Changed    int
	Digest     string
	RunID      string
}

// EndpointSummary は端点ごとの取り込み実績（台帳の集計）。
type EndpointSummary struct {
	Endpoint    string
	Fetches     int
	FirstTarget string
	LastTarget  string
	LastFetched string
	Rows        int
}

// Ledger は取り込みの記録。「いつ・何を・何件」だけを持つ。データ本体は Parquet。
type Ledger struct {
	Path string
	db   *sql.DB
}

// OpenLedger は台帳を開き、必要なら表を作る。
func OpenLedger(path string) (*Ledger, error) {
	db, err := storage.OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ledgerSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("台帳のスキーマを作れません: %w", err)
	}
	return &Ledger{Path: path, db: db}, nil
}

func (l *Ledger) Close() error { return l.db.Close() }

// Record は 1 回の取り込みを書き残す。FetchedUTC が空なら現在時刻。
func (l *Ledger) Record(rec IngestRecord) error {
	if rec.FetchedUTC.IsZero() {
		rec.FetchedUTC = clock.NowUTC()
	}
	_, err := l.db.Exec(
		`INSERT OR REPLACE INTO ingest (endpoint, target, source, fetched_utc, rows, changed, digest, run_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Endpoint, rec.Target, rec.Source, rec.FetchedUTC.UTC().Format(stampLayout),
		rec.Rows, rec.Changed, rec.Digest, rec.RunID,
	)
	if err != nil {
		return fmt.Errorf("台帳への記録に失敗しました %s %s: %w", rec.Endpoint, rec.Target, err)
	}
	return nil
}

// Last は端点 × 対象の最新の記録。無ければ nil。
func (l *Ledger) Last(ep Endpoint, target string) (*IngestRecord, error) {
	row := l.db.QueryRow(
		`SELECT endpoint, target, source, fetched_utc, rows, changed, digest, run_id FROM ingest
		 WHERE endpoint = ? AND target = ? ORDER BY fetched_utc DESC LIMIT 1`,
		ep.Path, target,
	)
	rec, err := scanRecord(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("台帳を引けません %s %s: %w", ep.Path, target, err)
	}
	return rec, nil
}

// Targets は一度でも取った対象。
func (l *Ledger) Targets(ep Endpoint) ([]string, error) {
	rows, err := l.db.Query(
		"SELECT DISTINCT target FROM ingest WHERE endpoint = ? ORDER BY target", ep.Path)
	if err != nil {
		return nil, fmt.Errorf("台帳を引けません %s: %w", ep.Path, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

// History は端点の取り込み履歴（新しい順）。
func (l *Ledger) History(ep Endpoint, limit int) ([]IngestRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := l.db.Query(
		`SELECT endpoint, target, source, fetched_utc, rows, changed, digest, run_id FROM ingest
		 WHERE endpoint = ? ORDER BY fetched_utc DESC LIMIT ?`, ep.Path, limit)
	if err != nil {
		return nil, fmt.Errorf("台帳を引けません %s: %w", ep.Path, err)
	}
	defer rows.Close()
	var out []IngestRecord
	for rows.Next() {
		rec, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// Summary は端点ごとの実績。
func (l *Ledger) Summary() ([]EndpointSummary, error) {
	rows, err := l.db.Query(
		`SELECT endpoint, COUNT(*), MIN(target), MAX(target), MAX(fetched_utc), SUM(rows)
		 FROM ingest GROUP BY endpoint ORDER BY endpoint`)
	if err != nil {
		return nil, fmt.Errorf("台帳を集計できません: %w", err)
	}
	defer rows.Close()
	var out []EndpointSummary
	for rows.Next() {
		var s EndpointSummary
		var first, last, fetched sql.NullString
		var total sql.NullInt64
		if err := rows.Scan(&s.Endpoint, &s.Fetches, &first, &last, &fetched, &total); err != nil {
			return nil, err
		}
		s.FirstTarget, s.LastTarget, s.LastFetched = first.String, last.String, fetched.String
		s.Rows = int(total.Int64)
		out = append(out, s)
	}
	return out, rows.Err()
}

// scanRecord は Row / Rows のどちらの Scan でも使える読み出し。
func scanRecord(scan func(...any) error) (*IngestRecord, error) {
	var rec IngestRecord
	var stamp string
	if err := scan(&rec.Endpoint, &rec.Target, &rec.Source, &stamp,
		&rec.Rows, &rec.Changed, &rec.Digest, &rec.RunID); err != nil {
		return nil, err
	}
	rec.FetchedUTC = parseStamp(stamp)
	return &rec, nil
}

// parseStamp は台帳の時刻文字列を読む。Python 版が書いた ISO 8601 も読めるようにする。
func parseStamp(s string) time.Time {
	for _, layout := range []string{stampLayout, time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
