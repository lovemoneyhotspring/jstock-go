// Package archive は J-Quants（Standard）の全データをローカルに蓄積する。
//
// 方針（Python 版 wbcore.data.jquants_archive からの移植）:
//   - 生のまま残す。列名は応答どおり、値はすべて文字列（日付列だけ Date 型）。
//     API は数値を文字列で返すことがあり、一括 CSV は全部文字列なので、
//     取り込み時に型を揃えようとすると経路で食い違う。整形は読むとき。
//   - 端点 × 月の Parquet。data/jquants/<端点>/<YYYY-MM>.parquet。
//   - 鍵で後勝ち。同じ鍵の行は新しい取り込みが勝つ。速報→確報、過誤訂正、
//     取り直しがすべてこの規則で片付き、何度実行しても同じになる。
//   - 台帳（SQLite）に「端点・対象・取得時刻・件数・変化した行数」を残す。
package archive

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Mode は増分の取り方。
type Mode string

const (
	// ModeDate は 1 日 1 リクエスト（date= などの日付引数で全銘柄）。対象は暦日。
	ModeDate Mode = "date"
	// ModeRange は from / to の範囲で 1 リクエスト。対象は範囲の終端日。
	ModeRange Mode = "range"
	// ModeAll は引数無しで全件。対象は取得日。
	ModeAll Mode = "all"
)

// TradingDayDivisions は取引カレンダーの HolDiv で「株式の取引がある日」とみなす値。
// 1 = 営業日、2 = 半日（大納会・大発会）。0 = 非営業日、3 = 祝日取引（先物のみ）。
var TradingDayDivisions = map[string]bool{"1": true, "2": true}

// TimeOfDay は時刻（JST）のみを表す。Go には Python の datetime.time に当たる型が無い。
type TimeOfDay struct {
	Hour   int
	Minute int
}

// On は指定日のその時刻を JST で組み立てる。
func (t TimeOfDay) On(day time.Time, jst *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), t.Hour, t.Minute, 0, 0, jst)
}

// Endpoint は蓄積する端点 1 つぶんの定義。
type Endpoint struct {
	Path string
	// Key は 1 行を一意にする列。上書きの単位。
	Key []string
	// DateColumn は月のファイルを切る日付列。鍵の先頭であることが多い。
	DateColumn string
	Mode       Mode
	// DateParam は日付を渡す引数名（date / disc_date）。
	DateParam string
	// AvailableAt はその日ぶんが API に乗る時刻（JST）。これより前には取りに行かない。
	AvailableAt TimeOfDay
	// SettleDays は訂正に備え、対象日から何日ぶん取り直しを続けるか。
	SettleDays int
	// MinIntervalHours は取り直しの最短間隔（時間）。cron が 20 分おきでも 1 日 1 回に抑える。
	MinIntervalHours int
	// TradingDaysOnly は取引日だけ対象にする（false なら平日すべて。EDINET の提出日など）。
	TradingDaysOnly bool
	// Bulk は一括ダウンロード（/bulk）にあるか。
	Bulk bool
	// RangeDays は ModeRange のとき、何日ぶん重ねて取るか。
	RangeDays int
	// ExtraDateColumns は日付列以外にも日付として扱う列。
	ExtraDateColumns []string
}

// Name はディレクトリ名。"/equities/bars/daily" → "equities_bars_daily"。
func (e Endpoint) Name() string {
	s := strings.Trim(e.Path, "/")
	s = strings.ReplaceAll(s, "/", "_")
	return strings.ReplaceAll(s, "-", "_")
}

// DateColumns は日付として扱う列すべて。
func (e Endpoint) DateColumns() []string {
	out := make([]string, 0, 1+len(e.ExtraDateColumns))
	out = append(out, e.DateColumn)
	out = append(out, e.ExtraDateColumns...)
	return out
}

// dateColumnSet は日付列の集合。Parquet の型付けに使う。
func (e Endpoint) dateColumnSet() map[string]bool {
	set := make(map[string]bool, 1+len(e.ExtraDateColumns))
	for _, c := range e.DateColumns() {
		set[c] = true
	}
	return set
}

var (
	t1005 = TimeOfDay{10, 5}
	t1630 = TimeOfDay{16, 30}
	t1730 = TimeOfDay{17, 30}
	t1800 = TimeOfDay{18, 0}
	t1900 = TimeOfDay{19, 0}
	t0000 = TimeOfDay{0, 0}
)

// StandardEndpoints は Standard プランで取れる端点の目録（Python の ENDPOINTS）。
var StandardEndpoints = []Endpoint{
	{
		Path: "/markets/calendar", Key: []string{"Date"}, DateColumn: "Date",
		Mode: ModeAll, DateParam: "date", AvailableAt: t0000,
		SettleDays: 0, MinIntervalHours: 24 * 7, TradingDaysOnly: true, Bulk: false,
	},
	{
		Path: "/equities/master", Key: []string{"Date", "Code"}, DateColumn: "Date",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1730,
		SettleDays: 1, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
	},
	{
		Path: "/equities/bars/daily", Key: []string{"Date", "Code"}, DateColumn: "Date",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1630,
		SettleDays: 5, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
	},
	{
		Path: "/indices/bars/daily", Key: []string{"Date", "Code"}, DateColumn: "Date",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1630,
		SettleDays: 5, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
	},
	{
		Path: "/indices/bars/daily/topix", Key: []string{"Date"}, DateColumn: "Date",
		Mode: ModeRange, DateParam: "date", AvailableAt: t1630,
		SettleDays: 5, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true, RangeDays: 10,
	},
	{
		Path: "/fins/summary", Key: []string{"DiscDate", "DiscTime", "Code", "DiscNo"},
		DateColumn: "DiscDate", Mode: ModeDate, DateParam: "date", AvailableAt: t1800,
		SettleDays: 2, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
		ExtraDateColumns: []string{"CurPerSt", "CurPerEn", "CurFYSt", "CurFYEn", "NxtFYSt", "NxtFYEn"},
	},
	{
		Path: "/fins/earnings-date", Key: []string{"PubDate", "Code"}, DateColumn: "PubDate",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1005,
		SettleDays: 1, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
		ExtraDateColumns: []string{"SchDate"},
	},
	{
		Path: "/equities/earnings-calendar", Key: []string{"Date", "Code"}, DateColumn: "Date",
		Mode: ModeAll, DateParam: "date", AvailableAt: t1900,
		SettleDays: 0, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: false,
	},
	{
		// 過誤訂正は公表の翌営業日に反映される。8 週ぶん重ねる
		Path: "/equities/investor-types", Key: []string{"PubDate", "StDate", "EnDate", "Section"},
		DateColumn: "PubDate", Mode: ModeRange, DateParam: "date", AvailableAt: t1800,
		SettleDays: 5, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true, RangeDays: 56,
		ExtraDateColumns: []string{"StDate", "EnDate"},
	},
	{
		// 週次（金曜日付、第 2 営業日に公開）。API は date か code が必須で from/to
		// だけでは呼べない（400）ため、毎営業日 date= で叩く。残高の無い日は 0 行
		Path: "/markets/margin-interest", Key: []string{"Date", "Code"}, DateColumn: "Date",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1630,
		SettleDays: 7, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
	},
	{
		Path: "/markets/margin-alert", Key: []string{"PubDate", "Code", "AppDate"},
		DateColumn: "PubDate", Mode: ModeDate, DateParam: "date", AvailableAt: t1630,
		SettleDays: 5, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
		ExtraDateColumns: []string{"AppDate"},
	},
	{
		Path: "/markets/short-ratio", Key: []string{"Date", "S33"}, DateColumn: "Date",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1630,
		SettleDays: 5, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
	},
	{
		Path:       "/markets/short-sale-report",
		Key:        []string{"DiscDate", "CalcDate", "Code", "SSName", "FundName"},
		DateColumn: "DiscDate", Mode: ModeDate, DateParam: "disc_date", AvailableAt: t1730,
		SettleDays: 5, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
		ExtraDateColumns: []string{"CalcDate", "PrevRptDate"},
	},
	{
		Path: "/derivatives/bars/daily/options/225", Key: []string{"Date", "Code"}, DateColumn: "Date",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1630,
		SettleDays: 5, MinIntervalHours: 20, TradingDaysOnly: true, Bulk: true,
	},
	{
		Path: "/edinet/major-shareholders", Key: []string{"DocId"}, DateColumn: "SubDate",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1800,
		SettleDays: 1, MinIntervalHours: 20, TradingDaysOnly: false, Bulk: false,
		ExtraDateColumns: []string{"PerSt", "PerEn"},
	},
	{
		Path: "/edinet/cross-shareholdings", Key: []string{"DocId"}, DateColumn: "SubDate",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1800,
		SettleDays: 1, MinIntervalHours: 20, TradingDaysOnly: false, Bulk: false,
		ExtraDateColumns: []string{"PerSt", "PerEn"},
	},
	{
		Path: "/edinet/large-volume-shareholders", Key: []string{"DocId"}, DateColumn: "SubDate",
		Mode: ModeDate, DateParam: "date", AvailableAt: t1800,
		SettleDays: 1, MinIntervalHours: 20, TradingDaysOnly: false, Bulk: false,
	},
}

// LookupEndpoint は名前（equities_bars_daily）かパス（/equities/bars/daily）で端点を引く。
func LookupEndpoint(nameOrPath string) (Endpoint, error) {
	for _, ep := range StandardEndpoints {
		if ep.Name() == nameOrPath || ep.Path == nameOrPath {
			return ep, nil
		}
	}
	names := make([]string, 0, len(StandardEndpoints))
	for _, ep := range StandardEndpoints {
		names = append(names, ep.Name())
	}
	sort.Strings(names)
	return Endpoint{}, fmt.Errorf("未知の端点 %q（利用可能: %s）", nameOrPath, strings.Join(names, ", "))
}

// MustEndpoint は LookupEndpoint の panic 版。定数を引くときだけ使う。
func MustEndpoint(nameOrPath string) Endpoint {
	ep, err := LookupEndpoint(nameOrPath)
	if err != nil {
		panic(err)
	}
	return ep
}

// CalendarEndpoint は取引カレンダー。営業日の判定に使うので他より先に取る。
func CalendarEndpoint() Endpoint { return MustEndpoint("/markets/calendar") }
