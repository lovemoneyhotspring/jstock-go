// Package history は積立の判断履歴（accum run が「今日いくら出すか」を決めた記録）。
//
// 置き場は state/accum/history/。追記専用の Parquet ストアそのものは
// wbcore/history が持っており、ここはその上に積立固有の列だけを載せる薄い層。
//
// なぜ台帳では足りないか:
// 台帳（state/accum-<env>.db）には発注したものしか残らない。積立の判断は
// 「倍率 × 予算」で決まり、下落局面ほど多く買う設計なので、改善に使いたいのは
// むしろ「どの倍率で、いくらの株価のときに、いくら出したか」の並び。発注に
// 至らなかった日（時間帯の外・単元未満）も含めて残さないと、後から
// 「倍率の付け方は効いていたのか」を確かめられない。
package history

import (
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
	"time"
)

// Kind は判断を積む履歴の種類。
const Kind = "decision"

// DecisionColumns は判断履歴の列。
var DecisionColumns = []history.Column{
	{Name: "symbol", Type: history.TypeString},
	{Name: "market", Type: history.TypeString},
	// judged_on は判断に使った足の日付（day は実行日なので別に持つ）
	{Name: "judged_on", Type: history.TypeDate},
	// month はどの月の積立か
	{Name: "month", Type: history.TypeDate},
	// close は判断に使った終値
	{Name: "close", Type: history.TypeFloat64},
	// due は今日出す額（差額）
	{Name: "due", Type: history.TypeFloat64},
	// target / placed は今月の目標と、すでに発注済みの額
	{Name: "target", Type: history.TypeFloat64},
	{Name: "placed", Type: history.TypeFloat64},
	// multiplier は下落局面で増やすための倍率。評価はこれが効いたかを見る
	{Name: "multiplier", Type: history.TypeFloat64},
	{Name: "tactic", Type: history.TypeString},
	{Name: "reason", Type: history.TypeString},
}

// Decision はその実行で決まった 1 銘柄ぶんの投下。
//
// Python 版は accum.execute.Contribution をそのまま受けていたが、Go の
// execute はこの形の値を持たないので、履歴に残す項目だけをここで定義する。
type Decision struct {
	Symbol     string
	Market     string
	JudgedOn   string // YYYY-MM-DD
	Month      string // YYYY-MM-DD（月初）
	Close      decimal.Decimal
	Due        decimal.Decimal
	Target     decimal.Decimal
	Placed     decimal.Decimal
	Multiplier float64
	Tactic     string
	Reason     string
}

// StoreFor は積立の履歴ストア。
func StoreFor(s *settings.AppSettings) *history.Store {
	return history.NewStore(s.AccumHistoryDir())
}

// DecisionFrame はその実行で決まった投下を 1 行ずつにする。0 件でも形は保つ
// （「その日は投下が無かった」も記録のうち）。
func DecisionFrame(decisions []Decision) history.Frame {
	rows := make([]map[string]any, 0, len(decisions))
	for _, d := range decisions {
		rows = append(rows, map[string]any{
			"symbol":     d.Symbol,
			"market":     d.Market,
			"judged_on":  dayValue(d.JudgedOn),
			"month":      dayValue(d.Month),
			"close":      history.ToFloat(d.Close),
			"due":        history.ToFloat(d.Due),
			"target":     history.ToFloat(d.Target),
			"placed":     history.ToFloat(d.Placed),
			"multiplier": d.Multiplier,
			"tactic":     d.Tactic,
			"reason":     d.Reason,
		})
	}
	return history.NewFrame(DecisionColumns, rows)
}

// dayValue は "YYYY-MM-DD" を Parquet の日付にする。空や不正なら欠損。
func dayValue(day string) any {
	if day == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		return nil
	}
	return t
}
