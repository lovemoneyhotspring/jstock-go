// Package history はスイング売買のスクリーニング履歴（wbjp screen の結果）を積む。
//
// 置き場は state/wbjp/history/screen/（wbcore/history.Store）。1 回の screen が 1 ファイル。
// 合成後の意見がある銘柄は**閾値未満も含めて全部**残し、passed（閾値以上）と
// adopted（上位 max_positions 件＝採用候補）の印を付ける。
//
// 全部残すのは事後検証のため——採用した銘柄だけを残すと、
// 「選ばなかった銘柄はどうなったか」と比べられなくなる（pkg/wbjp/evaluate）。
package history

import (
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

// Kind は screen の履歴の種類名（ディレクトリ名）。
const Kind = "screen"

// MetaKeys は戦略が meta に残す数値のうち履歴に持つもの（無ければ null）。
var MetaKeys = []string{
	"dryup_ratio",
	"drawdown",
	"atr_ratio",
	"dollar_volume",
	"dryup",
	"rs",
	"trend",
	"liquid",
}

// ScreenColumns は screen 履歴の列。
//
// wbcore/history は Parquet に書ける型しか持たないので、Python 版の Int32 は
// Int64 に寄せている（DuckDB で読むときに型が揺れないようにするため）。
var ScreenColumns = screenColumns()

func screenColumns() []corehistory.Column {
	columns := []corehistory.Column{
		{Name: "rank", Type: corehistory.TypeInt64},
		{Name: "symbol", Type: corehistory.TypeString},
		{Name: "score", Type: corehistory.TypeFloat64},
		{Name: "passed", Type: corehistory.TypeBool},
		{Name: "adopted", Type: corehistory.TypeBool},
		{Name: "close", Type: corehistory.TypeFloat64},
		{Name: "reason", Type: corehistory.TypeString},
	}
	for _, key := range MetaKeys {
		columns = append(columns, corehistory.Column{Name: key, Type: corehistory.TypeFloat64})
	}
	return append(columns,
		corehistory.Column{Name: "entry_threshold", Type: corehistory.TypeFloat64},
		corehistory.Column{Name: "max_positions", Type: corehistory.TypeInt64},
		corehistory.Column{Name: "combiner", Type: corehistory.TypeString},
	)
}

// StoreFor は設定から wbjp の履歴ストアを作る。
func StoreFor(s *settings.AppSettings) *corehistory.Store {
	return corehistory.NewStore(s.WbjpHistoryDir())
}

// ScreenOptions は ScreenFrame の付随情報。
type ScreenOptions struct {
	// Threshold は買い建て候補とみなす合成スコアの下限。
	Threshold float64
	// MaxPositions は 1 回に採用する銘柄数の上限。
	MaxPositions int
	// Combiner は使った合成方式の名前。
	Combiner string
	// Meta は銘柄ごとの戦略の内訳（MetaKeys の値を拾う）。
	Meta map[string]map[string]any
	// Reasons は銘柄ごとの表示用の理由（無ければ CombinedSignal の Reason）。
	Reasons map[string]string
	// Close は銘柄の終値を引く。Meta に close があればそちらを優先する。
	Close func(symbol string) decimal.Decimal
}

// ScreenFrame は合成後の全銘柄をスコア順に並べ、閾値と採用枠の印を付ける。
//
// 並びは「スコアの高い順、同点なら銘柄コード順」。同点の順位が実行ごとに
// 入れ替わると、後から履歴を突き合わせたときに差分が読めなくなるため。
func ScreenFrame(combined []domain.CombinedSignal, opts ScreenOptions) corehistory.Frame {
	ordered := make([]domain.CombinedSignal, len(combined))
	copy(ordered, combined)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Direction != ordered[j].Direction {
			return ordered[i].Direction > ordered[j].Direction
		}
		return ordered[i].Symbol < ordered[j].Symbol
	})

	rows := make([]map[string]any, 0, len(ordered))
	passedSoFar := 0
	for i, item := range ordered {
		meta := opts.Meta[item.Symbol]
		passed := item.Direction >= opts.Threshold
		if passed {
			passedSoFar++
		}

		reason := item.Reason
		if text, ok := opts.Reasons[item.Symbol]; ok && text != "" {
			reason = text
		}

		row := map[string]any{
			"rank":            int64(i + 1),
			"symbol":          item.Symbol,
			"score":           item.Direction,
			"passed":          passed,
			"adopted":         passed && passedSoFar <= opts.MaxPositions,
			"close":           closeOf(meta, opts.Close, item.Symbol),
			"reason":          reason,
			"entry_threshold": opts.Threshold,
			"max_positions":   int64(opts.MaxPositions),
			"combiner":        opts.Combiner,
		}
		for _, key := range MetaKeys {
			row[key] = corehistory.ToFloat(meta[key])
		}
		rows = append(rows, row)
	}
	return corehistory.NewFrame(ScreenColumns, rows)
}

// closeOf は meta の close を優先し、無ければ引き当て関数に聞く。
func closeOf(meta map[string]any, lookup func(string) decimal.Decimal, symbol string) any {
	if value := corehistory.ToFloat(meta["close"]); value != nil {
		return value
	}
	if lookup == nil {
		return nil
	}
	return corehistory.ToFloat(lookup(symbol))
}
