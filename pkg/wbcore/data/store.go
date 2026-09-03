package data

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
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
