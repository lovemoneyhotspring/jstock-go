package data

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

func bar(symbol, date string, close_ float64) domain.Bar {
	price := decimal.NewFromFloat(close_)
	return domain.Bar{
		Symbol: symbol, Date: date,
		Open: price, High: price, Low: price, Close: price,
		Volume: decimal.NewFromInt(1000),
	}
}

func TestNormalizeBarsSortsAndDedupes(t *testing.T) {
	// 増分取得と既存データを継ぎ足すと境界が二重になる。後勝ちで 1 本にまとめる
	bars, err := NormalizeBars([]domain.Bar{
		bar("7203", "2026-09-02", 100),
		bar("7203", "2026-09-01", 90),
		bar("7203", "2026-09-02", 101), // 訂正された足
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 {
		t.Fatalf("本数 = %d, want 2", len(bars))
	}
	if bars[0].Date != "2026-09-01" {
		t.Errorf("日付昇順になっていない: %v", bars[0].Date)
	}
	if !bars[1].Close.Equal(decimal.NewFromInt(101)) {
		t.Errorf("後勝ちになっていない: %s", bars[1].Close)
	}
}

func TestNormalizeBarsDropsMissingPrices(t *testing.T) {
	bars, err := NormalizeBars([]domain.Bar{
		bar("7203", "2026-09-01", 100),
		{Symbol: "7203", Date: "2026-09-02"}, // 価格が無い
		{Symbol: "7203", Date: "", Close: decimal.NewFromInt(10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 1 {
		t.Fatalf("本数 = %d, want 1", len(bars))
	}
}

func TestNormalizeBarsRejectsHighBelowLow(t *testing.T) {
	broken := domain.Bar{
		Symbol: "7203", Date: "2026-09-01",
		Open: decimal.NewFromInt(100), High: decimal.NewFromInt(90),
		Low: decimal.NewFromInt(95), Close: decimal.NewFromInt(98),
		Volume: decimal.NewFromInt(1),
	}
	if _, err := NormalizeBars([]domain.Bar{broken}); err == nil {
		t.Fatal("high < low は弾くべき")
	}
}

func TestEmptyBars(t *testing.T) {
	if len(EmptyBars()) != 0 {
		t.Fatal("空でない")
	}
}

func TestCsvReplayProvider(t *testing.T) {
	dir := t.TempDir()
	csv := "date,open,high,low,close,volume\n" +
		"2026-09-01,100,110,90,105,1000\n" +
		"2026-09-02,105,115,100,110,2000\n" +
		"2026-09-03,110,120,105,115,3000\n"
	if err := os.WriteFile(filepath.Join(dir, "7203.csv"), []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := NewCsvReplayProvider(dir)
	if provider.Name() != "csv_replay" {
		t.Errorf("Name = %s", provider.Name())
	}
	if got := provider.AvailableSymbols(); len(got) != 1 || got[0] != "7203" {
		t.Fatalf("AvailableSymbols = %v", got)
	}

	// 取得できなかった銘柄はキーごと省く
	bars, err := provider.FetchBars([]string{"7203", "9999"}, "2026-09-02", "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := bars["9999"]; present {
		t.Error("存在しない銘柄はキーごと省くべき")
	}
	if len(bars["7203"]) != 2 {
		t.Fatalf("本数 = %d, want 2（期間で絞る）", len(bars["7203"]))
	}
	if !bars["7203"][0].Close.Equal(decimal.NewFromInt(110)) {
		t.Errorf("close = %s", bars["7203"][0].Close)
	}
}

func TestCsvReplayRejectsMissingColumns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "7203.csv"), []byte("date,close\n2026-09-01,100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewCsvReplayProvider(dir).FetchBars([]string{"7203"}, "", "")
	if err == nil {
		t.Fatal("列が足りない CSV は弾くべき")
	}
	if !errors.Is(err, ErrMarketData) {
		t.Errorf("MarketDataError であるべき: %v", err)
	}
}

func TestInMemoryProvider(t *testing.T) {
	provider, err := NewInMemoryProvider(map[string][]domain.Bar{
		"7203": {bar("7203", "2026-09-02", 100), bar("7203", "2026-09-01", 90)},
	})
	if err != nil {
		t.Fatal(err)
	}
	bars, err := provider.FetchBars([]string{"7203", "6758"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 1 {
		t.Fatalf("銘柄数 = %d", len(bars))
	}
	if bars["7203"][0].Date != "2026-09-01" {
		t.Error("正規化（日付昇順）されていない")
	}
}

func TestDefaultProviderAndConnect(t *testing.T) {
	if DefaultProvider(domain.MarketJP) != ProviderJQuants {
		t.Errorf("日本株の既定 = %s", DefaultProvider(domain.MarketJP))
	}
	if DefaultProvider(domain.MarketUS) != ProviderFred {
		t.Errorf("米国の既定 = %s", DefaultProvider(domain.MarketUS))
	}
	if _, err := Connect("nonexistent", ProviderParams{Market: domain.MarketJP}); err == nil {
		t.Fatal("未知の名前は弾くべき")
	}
	// 対応していない市場は MarketDataError
	_, err := Connect(ProviderFred, ProviderParams{Market: domain.MarketJP})
	if err == nil || !errors.Is(err, ErrMarketData) {
		t.Fatalf("市場違いは MarketDataError であるべき: %v", err)
	}
	provider, err := Connect(ProviderFred, ProviderParams{Market: domain.MarketUS, Settings: &settings.AppSettings{}})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != ProviderFred {
		t.Errorf("Name = %s", provider.Name())
	}
	if len(Available()) != 2 {
		t.Errorf("登録数 = %v", Available())
	}
}
