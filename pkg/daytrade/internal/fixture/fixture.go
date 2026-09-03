// Package fixture は試験用の小さな J-Quants アーカイブを組み立てる。
//
// 母集団の条件とパネルの組み立ては DuckDB の SQL に落としてあるので、Go の単体試験だけでは
// 「SQL が意図どおりか」を確かめられない。実際に Parquet を書いて読ませる土台をここに置く。
// ネットワークには繋がない（値はすべてここで作る）。
package fixture

import (
	"fmt"
	"math"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

const dateLayout = "2006-01-02"

// Symbol は 1 銘柄の作り方。
type Symbol struct {
	// Code は J-Quants の 5 桁コード。
	Code string
	Name string
	// Market は MktNm（"プライム" / "グロース" …）。
	Market string
	// ProdCat は 011 が株式。それ以外（ETF 等）は母集団から外れる。
	ProdCat string
	// Mrgn は 2 が貸借銘柄（信用新規売りができる）。
	Mrgn string
	// Base は始値・終値の基準。
	Base float64
	// Turnover は 1 日の売買代金。
	Turnover float64
	// MktCap は時価総額（分位を切るのに使う）。
	MktCap float64
	// GapOn は日付 → その日の寄付ギャップ（前日終値からの比率）。
	GapOn map[string]float64
	// IntradayOn は日付 → 寄付から引けへのリターン。
	IntradayOn map[string]float64
}

// Build は days の営業日ぶんのアーカイブを root に書く。
func Build(root string, days []time.Time, symbols []Symbol) (*archive.Archive, error) {
	arch := archive.NewArchive(root)

	// 取引カレンダー（HolDiv 1 = 営業日）
	calendar := &archive.Frame{Columns: []string{"Date", "HolDiv"}}
	for _, day := range days {
		calendar.AppendRow(map[string]*string{
			"Date": ptr(day.Format(dateLayout)), "HolDiv": ptr("1"),
		})
	}
	if _, err := arch.Upsert(archive.CalendarEndpoint(), calendar); err != nil {
		return nil, err
	}

	master := &archive.Frame{Columns: []string{"Date", "Code", "CoName", "MktNm", "ProdCat", "Mrgn"}}
	bars := &archive.Frame{Columns: []string{
		"Date", "Code", "O", "H", "L", "C", "Va", "MktCap", "AdjFactor", "UL", "LL",
	}}
	for _, s := range symbols {
		// 終値は前日終値 × (1 + ギャップ) × (1 + 日中リターン) で繋ぐ。
		// 「前日終値 → 寄付 → 引け」の関係が壊れるとギャップの検証にならない
		prevClose := s.Base
		for _, day := range days {
			key := day.Format(dateLayout)
			open := prevClose * (1 + s.GapOn[key])
			closePrice := open * (1 + s.IntradayOn[key])
			high := math.Max(open, closePrice)
			low := math.Min(open, closePrice)
			bars.AppendRow(map[string]*string{
				"Date": ptr(key), "Code": ptr(s.Code),
				"O": num(open), "H": num(high), "L": num(low), "C": num(closePrice),
				"Va": num(s.Turnover), "MktCap": num(s.MktCap),
				"AdjFactor": ptr("1"), "UL": ptr("0"), "LL": ptr("0"),
			})
			master.AppendRow(map[string]*string{
				"Date": ptr(key), "Code": ptr(s.Code), "CoName": ptr(s.Name),
				"MktNm": ptr(s.Market), "ProdCat": ptr(s.ProdCat), "Mrgn": ptr(s.Mrgn),
			})
			prevClose = closePrice
		}
	}
	if _, err := arch.Upsert(archive.MustEndpoint("equities_bars_daily"), bars); err != nil {
		return nil, err
	}
	if _, err := arch.Upsert(archive.MustEndpoint("equities_master"), master); err != nil {
		return nil, err
	}
	return arch, nil
}

// AddEarnings は前日引け後の決算開示を足す（母集団から外れることの確認用）。
func AddEarnings(arch *archive.Archive, code string, discDate time.Time, discTime string) error {
	frame := &archive.Frame{Columns: []string{"DiscDate", "DiscTime", "Code", "DiscNo"}}
	frame.AppendRow(map[string]*string{
		"DiscDate": ptr(discDate.Format(dateLayout)), "DiscTime": ptr(discTime),
		"Code": ptr(code), "DiscNo": ptr("1"),
	})
	_, err := arch.Upsert(archive.MustEndpoint("fins_summary"), frame)
	return err
}

// AddMarginAlert は信用規制の公表を足す。jsfStop が真なら売り禁（新規売り不可）。
func AddMarginAlert(arch *archive.Archive, code string, pubDate time.Time, jsfStop bool) error {
	reason := `{"RestrictedByJSF": "0"}`
	if jsfStop {
		reason = `{"RestrictedByJSF": "1"}`
	}
	frame := &archive.Frame{Columns: []string{"PubDate", "Code", "AppDate", "PubReason"}}
	frame.AppendRow(map[string]*string{
		"PubDate": ptr(pubDate.Format(dateLayout)), "Code": ptr(code),
		"AppDate": ptr(pubDate.Format(dateLayout)), "PubReason": ptr(reason),
	})
	_, err := arch.Upsert(archive.MustEndpoint("markets_margin_alert"), frame)
	return err
}

// AddEarningsDate は当日の決算発表予定を足す。
func AddEarningsDate(arch *archive.Archive, code string, pubDate, schDate time.Time) error {
	frame := &archive.Frame{Columns: []string{"PubDate", "Code", "SchDate"}}
	frame.AppendRow(map[string]*string{
		"PubDate": ptr(pubDate.Format(dateLayout)), "Code": ptr(code),
		"SchDate": ptr(schDate.Format(dateLayout)),
	})
	_, err := arch.Upsert(archive.MustEndpoint("fins_earnings_date"), frame)
	return err
}

// BusinessDays は start から n 日ぶんの平日。
func BusinessDays(start time.Time, n int) []time.Time {
	var out []time.Time
	day := start
	for len(out) < n {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			out = append(out, day)
		}
		day = day.AddDate(0, 0, 1)
	}
	return out
}

func ptr(v string) *string { return &v }

func num(v float64) *string {
	s := fmt.Sprintf("%.1f", v)
	return &s
}
