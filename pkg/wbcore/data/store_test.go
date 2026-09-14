package data

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestBarStore(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "barstore_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store := NewBarStore(tempDir)
	symbol := "7203"

	if store.Has(symbol) {
		t.Fatalf("expected store not to have symbol initially")
	}

	b1, _ := domain.NewBar(symbol, "2026-08-01", decimal.NewFromInt(2500), decimal.NewFromInt(2550), decimal.NewFromInt(2490), decimal.NewFromInt(2530), decimal.NewFromInt(100000))
	b2, _ := domain.NewBar(symbol, "2026-08-02", decimal.NewFromInt(2530), decimal.NewFromInt(2580), decimal.NewFromInt(2520), decimal.NewFromInt(2570), decimal.NewFromInt(120000))

	if err := store.Write(symbol, []domain.Bar{b1, b2}); err != nil {
		t.Fatalf("failed to write bars: %v", err)
	}

	if !store.Has(symbol) {
		t.Fatalf("expected store to have symbol after write")
	}

	symbols, err := store.Symbols()
	if err != nil || len(symbols) != 1 || symbols[0] != symbol {
		t.Fatalf("unexpected symbols: %v", symbols)
	}

	readBars, err := store.Read(symbol, "2026-08-01", "2026-08-02")
	if err != nil {
		t.Fatalf("failed to read bars: %v", err)
	}
	if len(readBars) != 2 {
		t.Fatalf("expected 2 bars, got %d", len(readBars))
	}
	if !readBars[0].Close.Equal(decimal.NewFromInt(2530)) {
		t.Errorf("readBars[0].Close = %s, want 2530", readBars[0].Close)
	}

	// フィルタテスト
	readFiltered, err := store.Read(symbol, "2026-08-02", "2026-08-02")
	if err != nil {
		t.Fatalf("failed to read filtered bars: %v", err)
	}
	if len(readFiltered) != 1 {
		t.Fatalf("expected 1 bar, got %d", len(readFiltered))
	}
	if readFiltered[0].Date != "2026-08-02" {
		t.Errorf("unexpected date: %s", readFiltered[0].Date)
	}

	_ = filepath.Join(tempDir, symbol)
}

func tmpFiles(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// 書き出しは一時ファイルを経由し、成功しても失敗しても一時ファイルを残さない。
func TestBarStoreWriteLeavesNoTmp(t *testing.T) {
	dir := t.TempDir()
	store := NewBarStore(dir)
	bar, _ := domain.NewBar("7203", "2026-08-01", decimal.NewFromInt(2500), decimal.NewFromInt(2550), decimal.NewFromInt(2490), decimal.NewFromInt(2530), decimal.NewFromInt(100000))

	if err := store.Write("7203", []domain.Bar{bar}); err != nil {
		t.Fatal(err)
	}
	if left := tmpFiles(t, dir); len(left) != 0 {
		t.Errorf("成功後に一時ファイルが残っている: %v", left)
	}
	// 2 回目（上書き）も同じ
	if err := store.Write("7203", []domain.Bar{bar}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Read("7203", "", ""); len(got) != 1 {
		t.Errorf("上書き後の本数 = %d, want 1", len(got))
	}

	// 失敗の経路: 本体の名前がディレクトリで rename できない
	path, _ := store.PathFor("6758")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("6758", []domain.Bar{bar}); err == nil {
		t.Fatal("rename できないのに成功している")
	}
	if left := tmpFiles(t, dir); len(left) != 0 {
		t.Errorf("失敗後に一時ファイルが残っている: %v", left)
	}
}
