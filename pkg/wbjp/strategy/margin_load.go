package strategy

import (
	"strconv"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
)

// MarginRow はアーカイブの信用残 1 行（文字列のまま）。
type MarginRow struct {
	Code  string // 5 桁コード（72030）
	Date  string // 基準日 YYYY-MM-DD
	Long  string // 買い残
	Short string // 売り残
}

// MarginCodes は銘柄をアーカイブのコード（5 桁）で引けるようにする。返すのは 5 桁 → 銘柄。
//
// 東証の株式以外（指数・米国）は信用残が無いので含めない。
func MarginCodes(symbols []string) map[string]string {
	want := make(map[string]string, len(symbols))
	for _, sym := range symbols {
		code, isIndex, err := data.ToJQuantsCode(sym)
		if err != nil || isIndex {
			continue
		}
		if len(code) == 4 {
			code += "0"
		}
		want[code] = sym
	}
	return want
}

// NewMarginBookFromRows はアーカイブの行から信用残の台帳を組み立てる。
//
// codes に無いコード、日付の無い行、数値にならない行は飛ばす。使える行が
// 1 つも無ければ nil（戦略は意見を出さない）。
func NewMarginBookFromRows(codes map[string]string, rows []MarginRow, lagDays int) *MarginBook {
	records := make(map[string][]MarginRecord)
	for _, row := range rows {
		sym, ok := codes[row.Code]
		if !ok || row.Date == "" {
			continue
		}
		long, err1 := strconv.ParseFloat(row.Long, 64)
		short, err2 := strconv.ParseFloat(row.Short, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		records[sym] = append(records[sym], MarginRecord{Date: row.Date, Long: long, Short: short})
	}
	if len(records) == 0 {
		return nil
	}
	return NewMarginBookWithLag(records, lagDays)
}
