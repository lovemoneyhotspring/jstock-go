package strategy

import "testing"

func TestMarginCodes(t *testing.T) {
	got := MarginCodes([]string{"7203", "6758.T", "^N225", "", "AAPL"})
	if got["72030"] != "7203" || got["67580"] != "6758.T" {
		t.Errorf("東証の銘柄が 5 桁で引けない: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("指数・空・東証以外は含めない: %v", got)
	}
}

func TestNewMarginBookFromRows(t *testing.T) {
	codes := MarginCodes([]string{"7203", "6758"})
	rows := []MarginRow{
		{Code: "72030", Date: "2026-08-28", Long: "1000", Short: "500"},
		{Code: "72030", Date: "2026-09-04", Long: "1200", Short: "400"},
		{Code: "99990", Date: "2026-09-04", Long: "1", Short: "1"},   // 対象外のコード
		{Code: "67580", Date: "", Long: "1", Short: "1"},             // 日付が無い
		{Code: "67580", Date: "2026-09-04", Long: "abc", Short: "1"}, // 数値でない
		{Code: "67580", Date: "2026-09-04", Long: "1", Short: ""},
	}
	book := NewMarginBookFromRows(codes, rows, MarginPublicationLag)
	if book == nil {
		t.Fatal("使える行があるのに nil")
	}
	if book.Len() != 1 {
		t.Errorf("読めない行・対象外の行を取り込んだ: %d 銘柄", book.Len())
	}
	// 2026-09-04（金）の残高は公表の遅れ（5 日）を過ぎた 09-09 から見える
	if rec, ok := book.AsOf("7203", "2026-09-09"); !ok || rec.Long != 1200 || rec.Short != 400 {
		t.Errorf("09-09 時点: %+v ok=%v", rec, ok)
	}
	if rec, ok := book.AsOf("7203", "2026-09-08"); !ok || rec.Long != 1000 {
		t.Errorf("公表前は前の週: %+v ok=%v", rec, ok)
	}
	if _, ok := book.AsOf("6758", "2026-09-30"); ok {
		t.Error("壊れた行しか無い銘柄が引ける")
	}

	if NewMarginBookFromRows(codes, rows[2:], MarginPublicationLag) != nil {
		t.Error("使える行が無ければ nil（戦略は黙る）")
	}
	if NewMarginBookFromRows(codes, nil, MarginPublicationLag) != nil {
		t.Error("行が無ければ nil")
	}
}
