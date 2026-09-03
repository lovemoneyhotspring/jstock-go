package data

import (
	"os"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/parquet-go/parquet-go"
	"github.com/shopspring/decimal"
)

// writeRawBars は検証を通さずに Parquet を書く。
// 「壊れたファイルが既にある」状態を作るためのテスト専用の裏口。
func writeRawBars(s *BarStore, symbol string, records []BarRecord) error {
	path, err := s.PathFor(symbol)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := parquet.NewGenericWriter[BarRecord](f, parquet.Compression(&parquet.Zstd))
	if _, err := w.Write(records); err != nil {
		_ = f.Close()
		return err
	}
	if err := w.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// 四本値が 0 の足は保存させない。
//
// 取得元の項目名が変わって全部 0 になったことが実際にあり、そのときは
// Write が素通しだったため Parquet に残った。境界は入口で閉じる。
func TestBarStoreRejectsZeroPriceBars(t *testing.T) {
	s := NewBarStore(t.TempDir())
	zero := []domain.Bar{{
		Symbol: "7203.T", Date: "2026-08-03",
		Open: decimal.Zero, High: decimal.Zero, Low: decimal.Zero,
		Close: decimal.Zero, Volume: decimal.Zero,
	}}
	if err := s.Write("7203.T", zero); err == nil {
		t.Fatal("ゼロ価格の足が保存できてしまいました")
	}
	if bars, _ := s.Read("7203.T", "", ""); len(bars) != 0 {
		t.Errorf("保存されていないはずが %d 本読めました", len(bars))
	}
}

// 万一ゼロの足が残っているファイルを読んでも、戦略までは届かせない。
func TestBarStoreReadDropsInvalidRows(t *testing.T) {
	s := NewBarStore(t.TempDir())
	good, err := domain.NewBar("7203.T", "2026-08-04",
		dec("2500"), dec("2550"), dec("2490"), dec("2530"), dec("100000"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write("7203.T", []domain.Bar{good}); err != nil {
		t.Fatal(err)
	}
	// 既存ファイルにゼロ行が混ざっている状態を、書き込み検証を迂回して作る
	if err := writeRawBars(s, "7203.T", []BarRecord{
		{Date: "2026-08-03", Symbol: "7203.T"}, // 全部 0
		{Date: "2026-08-04", Open: 2500, High: 2550, Low: 2490, Close: 2530, Volume: 100000, Symbol: "7203.T"},
	}); err != nil {
		t.Fatal(err)
	}

	bars, err := s.Read("7203.T", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 1 || bars[0].Date != "2026-08-04" {
		t.Fatalf("ゼロ行が読み出しで落ちていません: %+v", bars)
	}
}

// Upsert は NormalizeBars を通すので、ゼロの足は静かに落ちる（エラーにしない）。
func TestBarStoreUpsertDropsZeroBars(t *testing.T) {
	s := NewBarStore(t.TempDir())
	total, err := s.Upsert("7203.T", []domain.Bar{{
		Symbol: "7203.T", Date: "2026-08-03",
		Open: decimal.Zero, High: decimal.Zero, Low: decimal.Zero, Close: decimal.Zero,
	}})
	if err != nil {
		t.Fatalf("Upsert がエラーになりました: %v", err)
	}
	if total != 0 {
		t.Errorf("保存後の本数 = %d, want 0", total)
	}
}
