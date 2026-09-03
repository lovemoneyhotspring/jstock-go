package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 1 か月ぶんの日足（2,600 銘柄 × 20 営業日 ≈ 5.2 万行、15 列）を模した表。
// 数字は docs/JQUANTS_ARCHIVE.md「メモリ」の実測と比べるためのもの。
func benchFrame(rows int) *Frame {
	columns := []string{"Date", "Code", "O", "H", "L", "C", "UL", "LL", "Vo", "Va", "AdjFactor", "AdjO", "AdjH", "AdjL", "AdjC"}
	f := &Frame{Columns: columns, Rows: make([]Row, 0, rows)}
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	for i := 0; i < rows; i++ {
		code := fmt.Sprintf("%05d", 10000+i%2600)
		d := day.AddDate(0, 0, (i/2600)%20).Format(dateLayout)
		row := make(Row, len(columns))
		row[0], row[1] = strptr(d), strptr(code)
		for j := 2; j < len(columns); j++ {
			row[j] = strptr(fmt.Sprintf("%d.%d", 1000+i%977, i%10))
		}
		f.Rows = append(f.Rows, row)
	}
	return f
}

func benchParquet(b *testing.B, rows int) (string, Endpoint) {
	b.Helper()
	ep := MustEndpoint("equities_bars_daily")
	path := filepath.Join(b.TempDir(), "bench.parquet")
	if err := writeParquet(path, benchFrame(rows), ep.dateColumnSet()); err != nil {
		b.Fatal(err)
	}
	return path, ep
}

func BenchmarkReadParquetAllColumns(b *testing.B) {
	path, _ := benchParquet(b, 52_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := readParquet(path)
		if err != nil || f.Height() != 52_000 {
			b.Fatalf("%v %d", err, f.Height())
		}
	}
}

func BenchmarkReadParquetProjected(b *testing.B) {
	path, ep := benchParquet(b, 52_000)
	opt := scanOptions{dateColumn: ep.DateColumn, columns: map[string]bool{"Date": true, "Code": true, "C": true}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := scanParquet(path, opt); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDigestOf(b *testing.B) {
	f := benchFrame(52_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DigestOf(f)
	}
}

func BenchmarkUpsertMonth(b *testing.B) {
	ep := MustEndpoint("equities_bars_daily")
	f := benchFrame(52_000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		arch := NewArchive(b.TempDir())
		if _, err := arch.Upsert(ep, f); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		// 2 回目: 既存の月を読み直して突き合わせ、書き戻す（日次の sync の経路）
		if _, err := arch.Upsert(ep, f); err != nil {
			b.Fatal(err)
		}
		os.RemoveAll(arch.Root)
	}
}

func BenchmarkCSVToFrame(b *testing.B) {
	ep := MustEndpoint("equities_bars_daily")
	f := benchFrame(52_000)
	var buf []byte
	buf = append(buf, "Date,Code,O,H,L,C,UL,LL,Vo,Va,AdjFactor,AdjO,AdjH,AdjL,AdjC\n"...)
	for _, row := range f.Rows {
		for i := range f.Columns {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, *row[i]...)
		}
		buf = append(buf, '\n')
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := CSVToFrame(buf, ep); err != nil {
			b.Fatal(err)
		}
	}
}
