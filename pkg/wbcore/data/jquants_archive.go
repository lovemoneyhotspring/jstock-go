package data

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// calendarLookback は「直近営業日」を求めるために取引カレンダーを遡る日数。
// 年末年始の連休でも取引日が 1 日は入る長さ。
const calendarLookback = 14

// BarArchive は日足の保管庫。pkg/jquants/archive.Archive を受ける口だけを切り出す。
//
// インタフェースにしておくのは、保管庫が無い環境（アーカイブを作っていない、
// 読めない）で nil を渡して API だけで動かせるようにするため。
type BarArchive interface {
	Read(ep archive.Endpoint, start, end time.Time) (*archive.Frame, error)
	Upsert(ep archive.Endpoint, f *archive.Frame) (int, error)
}

// archiveDailyBars は要求範囲がアーカイブに揃っていればそこから日足を作る。
//
// 「揃っている」は、その銘柄の保存済み最終日が end までの直近営業日以上であること。
// 判定には取引カレンダーが要るので、無ければ使わない（API に倒す）。
// これがあると、cron で毎日蓄積している端点を二度取りせずに済む。
func archiveDailyBars(
	arch BarArchive,
	symbol, code string,
	isIndex bool,
	start, end time.Time,
) ([]domain.Bar, bool) {
	if arch == nil || start.IsZero() || end.IsZero() {
		return nil, false
	}
	ep, err := archive.LookupEndpoint(JQuantsDailyPath(isIndex))
	if err != nil {
		return nil, false
	}
	stored, err := arch.Read(ep, start, end)
	if err != nil {
		// 壊れたファイル等。API に倒す
		fmt.Fprintf(os.Stderr, "[warn] アーカイブを読めません（%s）: %v\n", ep.Path, err)
		return nil, false
	}
	if stored == nil || stored.Height() == 0 || !stored.HasColumn("Code") {
		return nil, false
	}

	lastTrading, ok := lastTradingDay(arch, end)
	if !ok {
		return nil, false
	}

	var bars []domain.Bar
	haveLast := ""
	for i := 0; i < stored.Height(); i++ {
		if !codeMatches(stored.Get(i, "Code"), code) {
			continue
		}
		date := stored.Get(i, ep.DateColumn)
		if date == nil || *date == "" {
			continue
		}
		if *date > haveLast {
			haveLast = *date
		}
		bars = append(bars, archiveRowToBar(stored, i, *date, symbol, isIndex))
	}
	if len(bars) == 0 || haveLast < lastTrading.Format("2006-01-02") {
		// 直近営業日ぶんが未着なら、揃っていないとみなして API から取り直す
		return nil, false
	}
	return bars, true
}

// codeMatches は入力の 4 桁コード（7203）と保存の 5 桁（72030）を突き合わせる。
func codeMatches(stored *string, code string) bool {
	if stored == nil {
		return false
	}
	if len(code) == 5 {
		return *stored == code
	}
	if len(*stored) < 4 {
		return false
	}
	return (*stored)[:4] == code
}

func archiveRowToBar(f *archive.Frame, i int, date, symbol string, isIndex bool) domain.Bar {
	get := func(name string) any {
		if v := f.Get(i, name); v != nil {
			return *v
		}
		return nil
	}
	raw := JQuantsDailyBarRaw{Date: date}
	if isIndex {
		raw.IndexOpen, raw.IndexHigh = get("O"), get("H")
		raw.IndexLow, raw.IndexClose = get("L"), get("C")
	} else {
		raw.AdjOpen, raw.AdjHigh = get("AdjO"), get("AdjH")
		raw.AdjLow, raw.AdjClose = get("AdjL"), get("AdjC")
		raw.AdjVolume = get("AdjVo")
	}
	return raw.ToBar(symbol, isIndex)
}

// lastTradingDay は end までの直近営業日を取引カレンダーから求める。
func lastTradingDay(arch BarArchive, end time.Time) (time.Time, bool) {
	ep, err := archive.LookupEndpoint("/markets/calendar")
	if err != nil {
		return time.Time{}, false
	}
	cal, err := arch.Read(ep, end.AddDate(0, 0, -calendarLookback), end)
	if err != nil || cal == nil || cal.Height() == 0 || !cal.HasColumn("HolDiv") {
		return time.Time{}, false
	}
	best := ""
	for i := 0; i < cal.Height(); i++ {
		div := cal.Get(i, "HolDiv")
		if div == nil || !archive.TradingDayDivisions[*div] {
			continue
		}
		day := cal.Get(i, ep.DateColumn)
		if day != nil && *day > best {
			best = *day
		}
	}
	if best == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse("2006-01-02", best)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// storeToArchive は API から取った行をアーカイブに書き戻す。
// 蓄積の失敗で足の取得は止めない。
func storeToArchive(arch BarArchive, isIndex bool, items []json.RawMessage) {
	if arch == nil || len(items) == 0 {
		return
	}
	ep, err := archive.LookupEndpoint(JQuantsDailyPath(isIndex))
	if err != nil {
		return
	}
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		var row map[string]any
		if err := json.Unmarshal(item, &row); err == nil {
			rows = append(rows, row)
		}
	}
	frame, err := archive.RowsToFrame(rows, ep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] アーカイブへの整形に失敗（%s）: %v\n", ep.Path, err)
		return
	}
	if _, err := arch.Upsert(ep, frame); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] アーカイブに書けません（%s）: %v\n", ep.Path, err)
	}
}

// archiveDailyBarsFor は ISO 日付（YYYY-MM-DD）を受ける archiveDailyBars。
// どちらかが空なら「範囲が揃っているか」を判定できないので使わない。
func archiveDailyBarsFor(
	arch BarArchive,
	symbol, code string,
	isIndex bool,
	start, end string,
) ([]domain.Bar, bool) {
	if start == "" || end == "" {
		return nil, false
	}
	from, err := time.Parse("2006-01-02", start)
	if err != nil {
		return nil, false
	}
	to, err := time.Parse("2006-01-02", end)
	if err != nil {
		return nil, false
	}
	return archiveDailyBars(arch, symbol, code, isIndex, from, to)
}
