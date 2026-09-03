package data

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// CsvReplayProvider は CSV から足を供給する。
//
// 用途:
//   - 決定論的なデバッグ。ネットワークにも外部サービスにも依存せず、同じ入力から
//     必ず同じ結果が出る。「あの日おかしな注文が出た」を再現するときに要る
//   - テストで特定の値動き（ストップ高、連続ギャップ、薄商い）を意図的に作り込む
//
// CSV の形式は date,open,high,low,close,volume のヘッダ付き。
// ファイル名が銘柄コードになる（7203.csv → 7203）。
//
// 設定からは選べない（Providers に登録しない）。バックテスト用の取得元を設定で
// 選べるようにすると、本番の cron が誤って過去データで発注しうるため。
type CsvReplayProvider struct {
	Directory string
}

// NewCsvReplayProvider はディレクトリ内の CSV を読むプロバイダを作る。
func NewCsvReplayProvider(directory string) *CsvReplayProvider {
	return &CsvReplayProvider{Directory: directory}
}

func (p *CsvReplayProvider) Name() string { return "csv_replay" }

// AvailableSymbols はディレクトリにある銘柄コード。
func (p *CsvReplayProvider) AvailableSymbols() []string {
	entries, err := os.ReadDir(p.Directory)
	if err != nil {
		return []string{}
	}
	symbols := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".csv") {
			symbols = append(symbols, strings.TrimSuffix(e.Name(), ".csv"))
		}
	}
	sort.Strings(symbols)
	return symbols
}

func (p *CsvReplayProvider) FetchBars(symbols []string, start, end string) (map[string][]domain.Bar, error) {
	result := map[string][]domain.Bar{}
	for _, symbol := range symbols {
		path := filepath.Join(p.Directory, symbol+".csv")
		bars, err := readBarsCSV(path, symbol)
		if err != nil {
			if os.IsNotExist(err) {
				// 「そのファイルは無い」は失敗ではない。取得できなかった銘柄は
				// キーごと省く約束なので、そのまま次へ
				continue
			}
			return nil, NewMarketDataError(p.Name(), symbol+" の CSV を読めません", err)
		}
		normalized, err := NormalizeBars(bars)
		if err != nil {
			return nil, NewMarketDataError(p.Name(), symbol+" の足が不正です", err)
		}
		windowed := FilterBars(normalized, start, end)
		if len(windowed) > 0 {
			result[symbol] = windowed
		}
	}
	return result, nil
}

func readBarsCSV(path, symbol string) ([]domain.Bar, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}

	index := map[string]int{}
	for i, name := range records[0] {
		index[strings.ToLower(strings.TrimSpace(name))] = i
	}
	for _, needed := range BarColumns {
		if _, ok := index[needed]; !ok {
			return nil, fmt.Errorf("足データに列が不足しています: %s", needed)
		}
	}

	bars := make([]domain.Bar, 0, len(records)-1)
	for _, record := range records[1:] {
		if len(record) < len(records[0]) {
			continue
		}
		get := func(name string) string { return strings.TrimSpace(record[index[name]]) }
		bar := domain.Bar{
			Symbol: symbol,
			Date:   get("date"),
			Open:   parseDec(get("open")),
			High:   parseDec(get("high")),
			Low:    parseDec(get("low")),
			Close:  parseDec(get("close")),
			Volume: parseDec(get("volume")),
		}
		bars = append(bars, bar)
	}
	return bars, nil
}

// InMemoryProvider はあらかじめ用意した足を返す。テスト専用。
type InMemoryProvider struct {
	bars map[string][]domain.Bar
}

// NewInMemoryProvider は足を正規化して抱えるプロバイダを作る。
func NewInMemoryProvider(bars map[string][]domain.Bar) (*InMemoryProvider, error) {
	normalized := make(map[string][]domain.Bar, len(bars))
	for symbol, series := range bars {
		clean, err := NormalizeBars(series)
		if err != nil {
			return nil, NewMarketDataError("in_memory", symbol+" の足が不正です", err)
		}
		normalized[symbol] = clean
	}
	return &InMemoryProvider{bars: normalized}, nil
}

func (p *InMemoryProvider) Name() string { return "in_memory" }

func (p *InMemoryProvider) FetchBars(symbols []string, start, end string) (map[string][]domain.Bar, error) {
	result := map[string][]domain.Bar{}
	for _, symbol := range symbols {
		series, ok := p.bars[symbol]
		if !ok {
			continue
		}
		windowed := FilterBars(series, start, end)
		if len(windowed) > 0 {
			result[symbol] = windowed
		}
	}
	return result, nil
}
