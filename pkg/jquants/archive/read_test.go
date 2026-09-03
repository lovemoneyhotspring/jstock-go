package archive

import (
	"strings"
	"testing"
	"time"
)

// storeMonths は月をまたぐ足を保管庫に入れる（銘柄 × 日付の総当たり）。
func storeMonths(t *testing.T, a *Archive, ep Endpoint, months []string, days int, codes []string) {
	t.Helper()
	for _, month := range months {
		var rows []map[string]any
		for d := 1; d <= days; d++ {
			for _, code := range codes {
				rows = append(rows, map[string]any{
					"Date": month + "-" + pad(d), "Code": code,
					"AdjO": "100", "AdjH": "110", "AdjL": "90", "AdjC": "105",
					"AdjVo": "1000", "TurnoverValue": "12345",
				})
			}
		}
		if _, err := a.Upsert(ep, frameOf(t, ep, rows...)); err != nil {
			t.Fatalf("Upsert %s: %v", month, err)
		}
	}
}

func pad(d int) string {
	if d < 10 {
		return "0" + string(rune('0'+d))
	}
	return string(rune('0'+d/10)) + string(rune('0'+d%10))
}

func day(iso string) time.Time {
	t, _ := time.Parse(dateLayout, iso)
	return t
}

func TestReadWhereProjectsColumns(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	storeMonths(t, a, ep, []string{"2025-01", "2025-02"}, 3, []string{"13010", "72030"})

	got, err := a.ReadWhere(ep, ReadOptions{Columns: []string{"Code", "AdjC"}})
	if err != nil {
		t.Fatalf("ReadWhere: %v", err)
	}
	if got.Height() != 12 {
		t.Fatalf("行数 = %d, want 12", got.Height())
	}
	// 指定した列と、黙って足される日付列だけが載る
	for _, name := range []string{"Code", "AdjC", "Date"} {
		if !got.HasColumn(name) {
			t.Errorf("%s が落ちている: %v", name, got.Columns)
		}
	}
	for _, name := range []string{"AdjO", "AdjH", "AdjL", "AdjVo", "TurnoverValue"} {
		if got.HasColumn(name) {
			t.Errorf("%s は要求していないのに載っている", name)
		}
	}
	if v := got.Get(0, "AdjC"); v == nil || *v != "105" {
		t.Errorf("AdjC = %v, want 105", v)
	}
}

func TestReadWhereKeepFiltersBeforeLoading(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	storeMonths(t, a, ep, []string{"2025-01", "2025-02", "2025-03"}, 4, []string{"13010", "72030", "99840"})

	got, err := a.ReadWhere(ep, ReadOptions{
		Keep: func(row RowView) bool { return row.Equal("Code", "72030") },
	})
	if err != nil {
		t.Fatalf("ReadWhere: %v", err)
	}
	if got.Height() != 12 { // 3 か月 × 4 日
		t.Fatalf("行数 = %d, want 12", got.Height())
	}
	for i := 0; i < got.Height(); i++ {
		if v := got.Get(i, "Code"); v == nil || *v != "72030" {
			t.Fatalf("%d 行目の Code = %v", i, v)
		}
	}
}

func TestReadWhereKeepSeesUnprojectedColumns(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	storeMonths(t, a, ep, []string{"2025-01"}, 2, []string{"13010", "72030"})

	// Columns に入れていない列でも Keep からは覗ける（保存されていれば）
	got, err := a.ReadWhere(ep, ReadOptions{
		Columns: []string{"AdjC"},
		Keep:    func(row RowView) bool { return row.HasPrefix("Code", "7203") },
	})
	if err != nil {
		t.Fatalf("ReadWhere: %v", err)
	}
	if got.Height() != 2 {
		t.Fatalf("行数 = %d, want 2", got.Height())
	}
}

func TestReadWhereNarrowsByDate(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	storeMonths(t, a, ep, []string{"2025-01", "2025-02", "2025-03"}, 5, []string{"13010"})

	// 月ファイルの境目をまたぐ範囲。月で取ったあと日付でも絞られる
	got, err := a.Read(ep, day("2025-01-04"), day("2025-02-02"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Height() != 4 { // 01-04, 01-05, 02-01, 02-02
		t.Fatalf("行数 = %d, want 4", got.Height())
	}
	for i := 0; i < got.Height(); i++ {
		d := *got.Get(i, "Date")
		if d < "2025-01-04" || d > "2025-02-02" {
			t.Errorf("範囲外の日付が載っている: %s", d)
		}
	}
}

func TestDatesReadsOnlyDateColumn(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	storeMonths(t, a, ep, []string{"2025-01", "2025-02"}, 3, []string{"13010", "72030"})

	dates, err := a.Dates(ep)
	if err != nil {
		t.Fatalf("Dates: %v", err)
	}
	want := []string{"2025-01-01", "2025-01-02", "2025-01-03", "2025-02-01", "2025-02-02", "2025-02-03"}
	if len(dates) != len(want) {
		t.Fatalf("日付の数 = %d, want %d (%v)", len(dates), len(want), dates)
	}
	for i, iso := range want {
		if got := dates[i].Format(dateLayout); got != iso {
			t.Errorf("%d 番目 = %s, want %s（昇順で重複なしのはず）", i, got, iso)
		}
	}
}

func TestReadBudgetStopsInsteadOfExhaustingMemory(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	storeMonths(t, a, ep, []string{"2025-01", "2025-02"}, 10, []string{"13010", "72030", "99840"})

	original := ReadBudget
	defer func() { ReadBudget = original }()
	ReadBudget = 1 << 10 // 1 KB。全期間は必ず超える

	if _, err := a.Scan(ep); err == nil {
		t.Fatal("上限を超えたのにエラーにならない")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Errorf("何が起きたか分かるエラーになっていない: %v", err)
	}

	// 絞れば通る（上限は「載せた量」に対するもの）
	got, err := a.ReadWhere(ep, ReadOptions{
		Columns: []string{"AdjC"},
		Keep:    func(row RowView) bool { return row.Equal("Date", "2025-01-01") },
	})
	if err != nil {
		t.Fatalf("絞った読み出しが通らない: %v", err)
	}
	if got.Height() != 3 {
		t.Errorf("行数 = %d, want 3", got.Height())
	}
}

// Dates が行を組み立てずに日付列だけを読んでいることの歯止め。
// 全期間スキャンに戻すと、読み出しの上限に当たって落ちる。
func TestDatesDoesNotLoadRows(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	storeMonths(t, a, ep, []string{"2025-01", "2025-02", "2025-03"}, 10, []string{"13010", "72030", "99840"})

	original := ReadBudget
	defer func() { ReadBudget = original }()
	ReadBudget = 1 << 10 // 1 KB。行を載せたら必ず超える

	dates, err := a.Dates(ep)
	if err != nil {
		t.Fatalf("Dates が行を載せている（上限に当たった）: %v", err)
	}
	if len(dates) != 30 {
		t.Fatalf("日付の数 = %d, want 30", len(dates))
	}
}

func TestRowViewDoesNotConfuseNullAndEmpty(t *testing.T) {
	view := RowViewOf(map[string]string{"Code": "72030", "Blank": ""})
	if !view.Equal("Code", "72030") {
		t.Error("Equal が一致しない")
	}
	if view.Equal("Code", "7203") {
		t.Error("前方一致で Equal になってしまう")
	}
	if !view.HasPrefix("Code", "7203") {
		t.Error("HasPrefix が一致しない")
	}
	if view.Equal("Missing", "") {
		t.Error("無い列が空文字に一致してしまう")
	}
	if view.Text("Missing") != "" {
		t.Error("無い列の Text が空でない")
	}
	if !view.Equal("Blank", "") {
		t.Error("空文字の列が一致しない")
	}
}
