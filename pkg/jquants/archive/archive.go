package archive

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

// EndpointDef は J-Quants の端点定義。
type EndpointDef struct {
	Path        string
	Name        string
	DateColumn  string
	KeyColumns  []string
	Bulk        bool
}

var StandardEndpoints = []EndpointDef{
	{Path: "/markets/calendar", Name: "markets_calendar", DateColumn: "Date", KeyColumns: []string{"Date"}, Bulk: false},
	{Path: "/equities/master", Name: "equities_master", DateColumn: "Date", KeyColumns: []string{"Date", "Code"}, Bulk: true},
	{Path: "/equities/bars/daily", Name: "equities_bars_daily", DateColumn: "Date", KeyColumns: []string{"Date", "Code"}, Bulk: true},
	{Path: "/indices/bars/daily", Name: "indices_bars_daily", DateColumn: "Date", KeyColumns: []string{"Date", "Code"}, Bulk: true},
	{Path: "/fins/summary", Name: "fins_summary", DateColumn: "DiscDate", KeyColumns: []string{"DiscDate", "DiscTime", "Code", "DiscNo"}, Bulk: false},
	{Path: "/fins/earnings-date", Name: "fins_earnings_date", DateColumn: "PubDate", KeyColumns: []string{"PubDate", "Code"}, Bulk: false},
}

// IngestRecord は台帳（ingest テーブル）の1行。
type IngestRecord struct {
	Endpoint   string
	Target     string
	Source     string
	FetchedUTC string
	Rows       int
	Changed    int
	Digest     string
	RunID      string
}

// Archive は J-Quants データの Parquet 保管庫と台帳を管理する。
type Archive struct {
	Root string
	db   *sql.DB
}

func OpenArchive(root string) (*Archive, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("failed to create jquants archive dir: %w", err)
	}

	ledgerPath := filepath.Join(root, "ledger.db")
	db, err := storage.OpenSQLite(ledgerPath)
	if err != nil {
		return nil, err
	}

	schema := `CREATE TABLE IF NOT EXISTS ingest (
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

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize ledger schema: %w", err)
	}

	return &Archive{
		Root: root,
		db:   db,
	}, nil
}

func (a *Archive) Close() error {
	return a.db.Close()
}

func (a *Archive) RecordIngest(rec IngestRecord) error {
	if rec.FetchedUTC == "" {
		rec.FetchedUTC = clock.NowUTC().Format(time.RFC3339)
	}
	query := `INSERT OR REPLACE INTO ingest (endpoint, target, source, fetched_utc, rows, changed, digest, run_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?);`
	_, err := a.db.Exec(query, rec.Endpoint, rec.Target, rec.Source, rec.FetchedUTC, rec.Rows, rec.Changed, rec.Digest, rec.RunID)
	return err
}

func (a *Archive) EndpointStatus(epName string) (string, string, int, error) {
	// (oldestDate, latestDate, totalRows)
	var oldest, latest sql.NullString
	var total sql.NullInt64

	query := "SELECT MIN(target), MAX(target), SUM(rows) FROM ingest WHERE endpoint = ?;"
	err := a.db.QueryRow(query, epName).Scan(&oldest, &latest, &total)
	if err != nil {
		return "-", "-", 0, err
	}

	oldStr := "-"
	if oldest.Valid {
		oldStr = oldest.String
	}
	latStr := "-"
	if latest.Valid {
		latStr = latest.String
	}
	tot := 0
	if total.Valid {
		tot = int(total.Int64)
	}

	return oldStr, latStr, tot, nil
}

func (a *Archive) Directory(ep EndpointDef) string {
	return filepath.Join(a.Root, ep.Name)
}

func (a *Archive) ExistingParquetDirs() ([]string, error) {
	var dirs []string
	entries, err := os.ReadDir(a.Root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			p := filepath.Join(a.Root, e.Name())
			if hasParquet(p) {
				dirs = append(dirs, e.Name())
			}
		}
	}
	return dirs, nil
}

func hasParquet(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".parquet") {
			return true
		}
	}
	return false
}
