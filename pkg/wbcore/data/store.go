package data

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/parquet-go/parquet-go"
	"github.com/shopspring/decimal"
)

var safeSymbolRegex = regexp.MustCompile(`^[A-Za-z0-9.^_-]+$`)

// BarRecord は Parquet に保存される1行の構造体。
type BarRecord struct {
	Date   string  `parquet:"date"`
	Open   float64 `parquet:"open"`
	High   float64 `parquet:"high"`
	Low    float64 `parquet:"low"`
	Close  float64 `parquet:"close"`
	Volume float64 `parquet:"volume"`
	Symbol string  `parquet:"symbol,optional"`
}

// BarStore は Parquet による日足の保管庫。
type BarStore struct {
	root string
}

func NewBarStore(root string) *BarStore {
	return &BarStore{root: root}
}

func (s *BarStore) PathFor(symbol string) (string, error) {
	if !safeSymbolRegex.MatchString(symbol) {
		return "", fmt.Errorf("銘柄コードに使えない文字が含まれます: %q", symbol)
	}
	return filepath.Join(s.root, fmt.Sprintf("%s.parquet", symbol)), nil
}

func (s *BarStore) Has(symbol string) bool {
	path, err := s.PathFor(symbol)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (s *BarStore) Symbols() ([]string, error) {
	if _, err := os.Stat(s.root); os.IsNotExist(err) {
		return nil, nil
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}

	var symbols []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".parquet") {
			symbols = append(symbols, strings.TrimSuffix(e.Name(), ".parquet"))
		}
	}
	sort.Strings(symbols)
	return symbols, nil
}

func (s *BarStore) Read(symbol string, start, end string) ([]domain.Bar, error) {
	path, err := s.PathFor(symbol)
	if err != nil {
		return nil, err
	}
	if !s.Has(symbol) {
		return nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open parquet %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	reader := parquet.NewGenericReader[BarRecord](f)
	defer reader.Close()

	records := make([]BarRecord, reader.NumRows())
	if _, err := reader.Read(records); err != nil && err.Error() != "EOF" {
		// 行数分読めたか
	}

	var bars []domain.Bar
	for _, rec := range records {
		if rec.Date == "" {
			continue
		}
		if start != "" && rec.Date < start {
			continue
		}
		if end != "" && rec.Date > end {
			continue
		}

		bar, err := domain.NewBar(
			symbol,
			rec.Date,
			decimal.NewFromFloat(rec.Open),
			decimal.NewFromFloat(rec.High),
			decimal.NewFromFloat(rec.Low),
			decimal.NewFromFloat(rec.Close),
			decimal.NewFromFloat(rec.Volume),
		)
		if err == nil {
			bars = append(bars, bar)
		}
	}

	sort.Slice(bars, func(i, j int) bool {
		return bars[i].Date < bars[j].Date
	})

	_ = info
	return bars, nil
}

func (s *BarStore) Write(symbol string, bars []domain.Bar) error {
	path, err := s.PathFor(symbol)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	records := make([]BarRecord, len(bars))
	for i, b := range bars {
		o, _ := b.Open.Float64()
		h, _ := b.High.Float64()
		l, _ := b.Low.Float64()
		c, _ := b.Close.Float64()
		v, _ := b.Volume.Float64()
		records[i] = BarRecord{
			Date:   b.Date,
			Open:   o,
			High:   h,
			Low:    l,
			Close:  c,
			Volume: v,
			Symbol: symbol,
		}
	}

	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create tmp parquet %s: %w", tmpPath, err)
	}

	writer := parquet.NewGenericWriter[BarRecord](f, parquet.Compression(&parquet.Zstd))
	if _, err := writer.Write(records); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to write parquet records: %w", err)
	}
	if err := writer.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to close parquet writer: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, path)
}

// ReadMany は複数銘柄をまとめて読む。足が 1 本も無い銘柄は含めない
// （「データが無い」と「値動きが無い」を区別できるようにするため）。
func (s *BarStore) ReadMany(symbols []string, start, end string) (map[string][]domain.Bar, error) {
	result := make(map[string][]domain.Bar, len(symbols))
	for _, symbol := range symbols {
		bars, err := s.Read(symbol, start, end)
		if err != nil {
			return nil, err
		}
		if len(bars) > 0 {
			result[symbol] = bars
		}
	}
	return result, nil
}

// LastDate は保存済みの最終取引日（YYYY-MM-DD）。未保存なら空文字。
// 増分取得の起点に使う。
func (s *BarStore) LastDate(symbol string) (string, error) {
	bars, err := s.Read(symbol, "", "")
	if err != nil || len(bars) == 0 {
		return "", err
	}
	return bars[len(bars)-1].Date, nil
}

// Upsert は既存データに継ぎ足す。同じ日は新しい方（引数側）で上書きする。
// 戻り値は保存後の総本数。
//
// 「後勝ち」なのは、直近の足が取引所側で後から訂正されることがあるため。
func (s *BarStore) Upsert(symbol string, bars []domain.Bar) (int, error) {
	incoming, err := NormalizeBars(bars)
	if err != nil {
		return 0, err
	}
	existing, err := s.Read(symbol, "", "")
	if err != nil {
		return 0, err
	}
	if len(incoming) == 0 {
		return len(existing), nil
	}
	// NormalizeBars が重複を後勝ちで潰すので、後ろに置いた incoming が残る
	merged, err := NormalizeBars(append(existing, incoming...))
	if err != nil {
		return 0, err
	}
	if err := s.Write(symbol, merged); err != nil {
		return 0, err
	}
	return len(merged), nil
}

// Query は保存済みの全銘柄に SQL を投げる（DuckDB）。
//
// テーブル名 bars で全銘柄を横断できる。列は日足の正規スキーマに symbol を
// 加えたもの。1 銘柄 1 ファイルの Parquet をそのまま読ませられるので、
// 集計のためにデータを別の形へ複製せずに済む。
//
//	store.Query("SELECT symbol, count(*) AS n FROM bars GROUP BY symbol")
func (s *BarStore) Query(query string) (history.Frame, error) {
	symbols, err := s.Symbols()
	if err != nil {
		return history.Frame{}, err
	}
	if len(symbols) == 0 {
		return history.NewFrame([]history.Column{}, nil), nil
	}

	db, err := storage.OpenDuckDB()
	if err != nil {
		return history.Frame{}, fmt.Errorf("DuckDB を開けません: %w", err)
	}
	defer db.Close()

	pattern := strings.ReplaceAll(filepath.Join(s.root, "*.parquet"), "'", "''")
	if _, err := db.Exec("CREATE VIEW bars AS SELECT * FROM read_parquet('" + pattern + "')"); err != nil {
		return history.Frame{}, fmt.Errorf("bars ビューを作れません: %w", err)
	}
	return history.QueryFrame(db, query)
}

// Summary は保存状況の一覧（銘柄・本数・最初と最後の日）。データの穴を見つけるのに使う。
func (s *BarStore) Summary() (history.Frame, error) {
	return s.Query(
		"SELECT symbol, count(*) AS bars, min(date) AS first, max(date) AS last " +
			"FROM bars GROUP BY symbol ORDER BY symbol",
	)
}
