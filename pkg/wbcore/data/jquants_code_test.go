package data

import (
	"testing"

	"github.com/shopspring/decimal"
)

// dec は文字列から Decimal を作るテスト用の短縮。
func dec(v string) decimal.Decimal { return decimal.RequireFromString(v) }

func TestToJQuantsCode(t *testing.T) {
	cases := []struct {
		symbol  string
		code    string
		isIndex bool
	}{
		{"7203", "7203", false},
		{"7203.T", "7203", false},
		{"452A.T", "452A", false},
		{"72030", "72030", false}, // J-Quants の 5 桁コードもそのまま
		{"^TOPIX", "0000", true},
		{"^0028", "0028", true},
	}
	for _, c := range cases {
		code, isIndex, err := ToJQuantsCode(c.symbol)
		if err != nil {
			t.Errorf("%s: %v", c.symbol, err)
			continue
		}
		if code != c.code || isIndex != c.isIndex {
			t.Errorf("%s → (%s, %v), want (%s, %v)", c.symbol, code, isIndex, c.code, c.isIndex)
		}
	}
}

// 米国の指数は J-Quants で取れない。ここで弾かないと、^GSPC を株式コードとして
// 問い合わせて空を掴み、無音でデータが欠ける。
func TestToJQuantsCodeRejectsUnsupported(t *testing.T) {
	for _, symbol := range []string{"", "^GSPC", "^IXIC", "AAPL", "^N225"} {
		if _, _, err := ToJQuantsCode(symbol); err == nil {
			t.Errorf("%q を受け付けてしまいました", symbol)
		}
	}
}

func TestJQuantsDailyPath(t *testing.T) {
	if got := JQuantsDailyPath(false); got != "/equities/bars/daily" {
		t.Errorf("株式のパス = %s", got)
	}
	if got := JQuantsDailyPath(true); got != "/indices/bars/daily" {
		t.Errorf("指数のパス = %s", got)
	}
}

// 株式は調整済み（Adj*）を使う。未調整だと分割日に偽の下落が現れる。
func TestDailyBarRawUsesAdjustedPrices(t *testing.T) {
	raw := JQuantsDailyBarRaw{
		Date:     "20260803",
		AdjOpen:  "100", // 調整済み
		AdjHigh:  "110",
		AdjLow:   "90",
		AdjClose: "105",

		AdjVolume: "1000",
	}
	bar := raw.ToBar("7203.T", false)
	if bar.Date != "2026-08-03" {
		t.Errorf("日付 = %s, want 2026-08-03", bar.Date)
	}
	if !bar.Close.Equal(dec("105")) || !bar.Volume.Equal(dec("1000")) {
		t.Errorf("終値/出来高 = %s / %s", bar.Close, bar.Volume)
	}
}

// 指数は調整の概念が無く出来高も無い。O/H/L/C で返り、出来高は 0。
func TestDailyBarRawIndexColumns(t *testing.T) {
	raw := JQuantsDailyBarRaw{
		Date:       "2026-08-03",
		IndexOpen:  "2800",
		IndexHigh:  "2850",
		IndexLow:   "2790",
		IndexClose: "2840",
	}
	bar := raw.ToBar("^TOPIX", true)
	if !bar.Close.Equal(dec("2840")) {
		t.Errorf("終値 = %s, want 2840", bar.Close)
	}
	if !bar.Volume.IsZero() {
		t.Errorf("出来高 = %s, want 0", bar.Volume)
	}
}

func TestCodeMatches(t *testing.T) {
	stored := "72030"
	if !codeMatches(&stored, "7203") {
		t.Error("4 桁の入力が 5 桁の保存に一致しません")
	}
	if !codeMatches(&stored, "72030") {
		t.Error("5 桁どうしが一致しません")
	}
	other := "13060"
	if codeMatches(&other, "7203") {
		t.Error("別の銘柄に一致してしまいました")
	}
	if codeMatches(nil, "7203") {
		t.Error("nil に一致してしまいました")
	}
}
