package data

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

var fredSeries = map[string]string{
	"^GSPC": "SP500",
	"^IXIC": "NASDAQCOM",
	"^NDX":  "NASDAQ100",
	"^DJI":  "DJIA",
	"^VIX":  "VIXCLS",
}

type FREDProvider struct {
	client *http.Client
}

func NewFREDProvider(timeout time.Duration) *FREDProvider {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &FREDProvider{
		client: &http.Client{Timeout: timeout},
	}
}

func (p *FREDProvider) SeriesID(symbol string) (string, error) {
	sym := strings.TrimSpace(symbol)
	if id, ok := fredSeries[sym]; ok {
		return id, nil
	}
	if strings.HasPrefix(sym, "^") {
		return "", fmt.Errorf("FRED に対応していない指数です: %s", symbol)
	}
	return sym, nil
}

func (p *FREDProvider) FetchBars(symbol string, start, end string) ([]domain.Bar, error) {
	series, err := p.SeriesID(symbol)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("https://fred.stlouisfed.org/graph/fredgraph.csv?id=%s", series)
	if start != "" {
		url += fmt.Sprintf("&cosd=%s", start)
	}
	if end != "" {
		url += fmt.Sprintf("&coed=%s", end)
	}

	resp, err := p.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch FRED data for %s: %w", symbol, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("FRED HTTP error: status %d", resp.StatusCode)
	}

	reader := csv.NewReader(resp.Body)
	// ヘッダ行の読込 (DATE, SERIES_ID)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read FRED CSV header: %w", err)
	}
	_ = header

	var bars []domain.Bar
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read FRED CSV record: %w", err)
		}
		if len(record) < 2 {
			continue
		}

		dateStr := strings.TrimSpace(record[0])
		valStr := strings.TrimSpace(record[1])
		if valStr == "." || valStr == "" {
			continue // 欠損値スキップ
		}

		val, err := decimal.NewFromString(valStr)
		if err != nil {
			continue
		}

		bar, err := domain.NewBar(symbol, dateStr, val, val, val, val, decimal.Zero)
		if err == nil {
			bars = append(bars, bar)
		}
	}

	return bars, nil
}
