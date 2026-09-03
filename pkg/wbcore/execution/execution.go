// Package execution は実行品質——「そう判断した値」と「実際に約定した値」の差を残す。
//
// なぜ台帳とは別に要るか:
// 台帳（state/*.db）は「いま何を持っていて、いくら発注済みか」を答えるためのもので、
// 運用に必要な最新の状態だけを持つ。約定額で発注額を上書きするので、事後には
// 「判断時にいくらのつもりだったか」を復元できない。改善に使いたいのはまさにその
// 差分——想定より不利に約定していないか、どの理由でどれだけ見送っているか——なので、
// 上書きされない追記専用の表を別に持つ。
//
// 形:
// 1 回の発注は必ず 2 つの時点に分かれる。発注した瞬間に約定価格は分からず、
// 約定は次回以降の照会で判明する。そこで 1 行を後から書き換えるのではなく、行を足す。
//
//	intent  発注した（または dry-run で出さなかった）時点。想定価格・想定手数料
//	fill    約定を照会して確定した時点。約定価格・約定数量
//	skip    発注しなかった。Reason に理由コード
//
// 突き合わせの鍵は client_order_id。intent と fill を突き合わせればスリッページが出る。
//
// 金額の型:
// ログでは金額を文字列にしているが、ここは集計のための表なので float64 にする。
// 正確な残高の記録は台帳が持っており、こちらは「平均して何 bp 負けているか」を
// 出すためのもの。
package execution

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
)

// Kind は実行品質の履歴の種類（history.Store のディレクトリ名）。
const Kind = "execution"

// ReasonCode は発注しなかった／できなかった理由。
//
// これまで理由は自由文だけだった。文言は変わるので、集計はこのコードで行う。
// 人が読むための説明は note 列に自由文のまま残す。
type ReasonCode string

const (
	// ReasonPlaced は出した。
	ReasonPlaced ReasonCode = "placed"
	// ReasonDryRun は --live が無い（判断と記録だけ）。
	ReasonDryRun ReasonCode = "dry_run"
	// ReasonKillSwitch はキルスイッチ。
	ReasonKillSwitch ReasonCode = "kill_switch"
	// ReasonWindowClosed は発注してよい時間帯の外。
	ReasonWindowClosed ReasonCode = "window_closed"
	// ReasonHoliday は休日。
	ReasonHoliday ReasonCode = "holiday"
	// ReasonLotTooSmall は予算が 1 単元に届かない。
	ReasonLotTooSmall ReasonCode = "lot_too_small"
	// ReasonInsufficientFunds は買付余力が足りない。
	ReasonInsufficientFunds ReasonCode = "insufficient_funds"
	// ReasonIdempotent は同じ注文を送信済み（冪等）。
	ReasonIdempotent ReasonCode = "idempotent"
	// ReasonRiskRejected はリスク判定で弾かれた。
	ReasonRiskRejected ReasonCode = "risk_rejected"
	// ReasonNotEligible は銘柄が対象外（allowlist・貸借銘柄でない 等）。
	ReasonNotEligible ReasonCode = "not_eligible"
	// ReasonNoQuote は気配が取れない・古い。
	ReasonNoQuote ReasonCode = "no_quote"
	// ReasonBrokerError はブローカーがエラーを返した。
	ReasonBrokerError ReasonCode = "broker_error"
	// ReasonUnconfirmed は送ったが応答が無く、届いたか分からない。
	ReasonUnconfirmed ReasonCode = "unconfirmed"
	// ReasonFilled は約定した（fill の行）。
	ReasonFilled ReasonCode = "filled"
	// ReasonExpired は未約定のまま失効した。
	ReasonExpired ReasonCode = "expired"
)

func (r ReasonCode) String() string { return string(r) }

// イベント種別（event 列）。
const (
	EventIntent = "intent"
	EventFill   = "fill"
	EventSkip   = "skip"
)

// Schema は追記する表の形。列は増やしてよい（読み出しは union_by_name で前方互換）。
var Schema = []history.Column{
	{Name: "event", Type: history.TypeString},
	{Name: "app", Type: history.TypeString},
	{Name: "symbol", Type: history.TypeString},
	{Name: "side", Type: history.TypeString},
	// 現物 / 信用新規 / 信用返済。アプリによっては無い
	{Name: "trade", Type: history.TypeString},
	{Name: "client_order_id", Type: history.TypeString},
	{Name: "broker_order_id", Type: history.TypeString},
	{Name: "live", Type: history.TypeBool},
	// 判断した時点の想定
	{Name: "quantity", Type: history.TypeInt64},
	{Name: "intent_price", Type: history.TypeFloat64},
	{Name: "intent_amount", Type: history.TypeFloat64},
	{Name: "intent_fee", Type: history.TypeFloat64},
	// 実際
	{Name: "fill_quantity", Type: history.TypeInt64},
	{Name: "fill_price", Type: history.TypeFloat64},
	{Name: "fill_fee", Type: history.TypeFloat64},
	// 有利ならプラス、不利ならマイナス（SlippageBP）
	{Name: "slippage_bp", Type: history.TypeFloat64},
	{Name: "reason", Type: history.TypeString},
	{Name: "note", Type: history.TypeString},
}

// Spec は 1 行ぶんの入力。
//
// 数量・価格は any にしてある。Decimal でも文字列でも int でも渡せるようにし、
// 「値が無い」（nil）と「0」を区別するため——ポインタだらけの呼び出し側にしない。
type Spec struct {
	Event  string
	App    string
	Symbol string
	Side   string
	Reason ReasonCode

	Trade         string
	ClientOrderID string
	BrokerOrderID string
	Live          bool
	Note          string

	Quantity     any
	IntentPrice  any
	IntentAmount any
	IntentFee    any
	FillQuantity any
	FillPrice    any
	FillFee      any
}

// SlippageBP は想定と約定の差を bp で返す。有利ならプラス、不利ならマイナス。
//
// 買いは安く買えたら有利、売りは高く売れたら有利——向きが逆なので、
// 符号を揃えておかないと平均が意味を持たなくなる。
// 想定価格が無い・0 のとき、約定価格が無いときは nil。
func SlippageBP(side string, intentPrice, fillPrice any) any {
	intent := history.ToFloat(intentPrice)
	fill := history.ToFloat(fillPrice)
	if intent == nil || intent.(float64) == 0 || fill == nil {
		return nil
	}
	i, f := intent.(float64), fill.(float64)
	diff := f - i
	if strings.EqualFold(side, "BUY") {
		diff = i - f
	}
	return diff / i * 10_000
}

// Row は 1 行を作る。列は Schema に揃える。
func Row(spec Spec) map[string]any {
	return map[string]any{
		"event":  spec.Event,
		"app":    spec.App,
		"symbol": spec.Symbol,
		// Side / TradeType の型で渡ることがあるので、必ず素の大文字の str にする
		"side":            strings.ToUpper(spec.Side),
		"trade":           nilIfEmpty(spec.Trade),
		"client_order_id": nilIfEmpty(spec.ClientOrderID),
		"broker_order_id": nilIfEmpty(spec.BrokerOrderID),
		"live":            spec.Live,
		"quantity":        history.ToInt(spec.Quantity),
		"intent_price":    history.ToFloat(spec.IntentPrice),
		"intent_amount":   history.ToFloat(spec.IntentAmount),
		"intent_fee":      history.ToFloat(spec.IntentFee),
		"fill_quantity":   history.ToInt(spec.FillQuantity),
		"fill_price":      history.ToFloat(spec.FillPrice),
		"fill_fee":        history.ToFloat(spec.FillFee),
		"slippage_bp":     SlippageBP(spec.Side, spec.IntentPrice, spec.FillPrice),
		"reason":          string(spec.Reason),
		"note":            nilIfEmpty(spec.Note),
	}
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// Frame は行の並びを Schema の表にする。0 行でも形は保つ。
func Frame(rows []map[string]any) history.Frame {
	normalized := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := make(map[string]any, len(Schema))
		for _, column := range Schema {
			item[column.Name] = row[column.Name]
		}
		normalized = append(normalized, item)
	}
	return history.NewFrame(Schema, normalized)
}

// Record はまとめて 1 ファイルとして足す。行が無ければ何もしない。
//
// 0 行を書かないのは、plan などと違って「発注の機会そのものが無かった」実行が
// 大半だから。毎回書くと空ファイルばかりが増える。
func Record(store *history.Store, rows []map[string]any, day time.Time) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := store.Append(Kind, Frame(rows), day, history.AppendOptions{})
	return err
}

// ---------------------------------------------------------------------------
// 実行中の貯め込み
//
// Python 版は ContextVar だったが、Go の CLI では 1 プロセス 1 実行なので
// パッケージ変数で持つ。goroutine から呼ばれても壊れないように mutex で守る。
// ---------------------------------------------------------------------------

var (
	bufferMu sync.Mutex
	buffer   []map[string]any
)

// Collect は 1 行を貯める。書き出しは実行の終わりに Flush でまとめて行う。
//
// 発注のたびに Parquet を書くと、1 回の実行で十数個のファイルができてしまう。
func Collect(spec Spec) {
	bufferMu.Lock()
	defer bufferMu.Unlock()
	buffer = append(buffer, Row(spec))
}

// Flush は貯めた行を書き出して空にする。記録が失敗しても実行は落とさない
// （実行品質の記録は付帯物で、売買そのものより優先しない）。返り値は書き出しの
// エラーで、呼び出し側はログに落とすだけでよい。
func Flush(store *history.Store, day time.Time) error {
	bufferMu.Lock()
	rows := buffer
	buffer = nil
	bufferMu.Unlock()

	if len(rows) == 0 {
		return nil
	}
	if err := Record(store, rows, day); err != nil {
		return fmt.Errorf("実行品質の記録に失敗しました（売買には影響しません）: %w", err)
	}
	return nil
}

// Pending はまだ書き出していない行（テスト用）。
func Pending() []map[string]any {
	bufferMu.Lock()
	defer bufferMu.Unlock()
	out := make([]map[string]any, len(buffer))
	copy(out, buffer)
	return out
}

// Reset は貯めた行を捨てる（テスト用）。
func Reset() {
	bufferMu.Lock()
	defer bufferMu.Unlock()
	buffer = nil
}

// SummarySchema は Summarize が返す表の形。
var SummarySchema = []history.Column{
	{Name: "app", Type: history.TypeString},
	{Name: "reason", Type: history.TypeString},
	{Name: "count", Type: history.TypeInt64},
	{Name: "avg_slippage_bp", Type: history.TypeFloat64},
	{Name: "amount", Type: history.TypeFloat64},
}

// Summarize は実行品質の要約。理由コードごとの件数と、約定したものの平均スリッページ。
func Summarize(executions history.Frame) history.Frame {
	type key struct{ app, reason string }
	type agg struct {
		count        int64
		slippageSum  float64
		slippageRows int64
		amount       float64
	}
	groups := map[key]*agg{}
	for _, row := range executions.Rows {
		k := key{app: text(row["app"]), reason: text(row["reason"])}
		item := groups[k]
		if item == nil {
			item = &agg{}
			groups[k] = item
		}
		item.count++
		if v, ok := row["slippage_bp"].(float64); ok {
			item.slippageSum += v
			item.slippageRows++
		}
		if v, ok := row["intent_amount"].(float64); ok {
			item.amount += v
		}
	}

	rows := make([]map[string]any, 0, len(groups))
	for k, item := range groups {
		var avg any
		if item.slippageRows > 0 {
			avg = item.slippageSum / float64(item.slippageRows)
		}
		rows = append(rows, map[string]any{
			"app":             k.app,
			"reason":          k.reason,
			"count":           item.count,
			"avg_slippage_bp": avg,
			"amount":          item.amount,
		})
	}
	// アプリ名の昇順、件数の降順（多い理由から目に入るように）
	sort.Slice(rows, func(i, j int) bool {
		ai, aj := rows[i]["app"].(string), rows[j]["app"].(string)
		if ai != aj {
			return ai < aj
		}
		return rows[i]["count"].(int64) > rows[j]["count"].(int64)
	})
	return history.NewFrame(SummarySchema, rows)
}

func text(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}
