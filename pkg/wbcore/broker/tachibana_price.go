package broker

// 立花証券の定額コース手数料と、時価問合（CLMMfdsGetMarketPrice）。
//
// 手数料表は 1 日の現物約定代金の**合計**で段階が決まる。1 注文ごとではないので、
// 「この注文でいくら増えるか」は当日の既約定分を含めた差分で見る
// （MarginalFlatRateCommission）。デイトレの検証と発注前の見積りが両方これに依存する。

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/shopspring/decimal"
)

// FlatRateTier は定額コースの 1 段階（その日の約定代金合計の上限「以下」と、その手数料）。
type FlatRateTier struct {
	// Bound はこの段階に入る約定代金合計の上限（この値「以下」なら Fee）。
	Bound decimal.Decimal
	// Fee はその日の手数料（総額、円）。
	Fee decimal.Decimal
}

// FlatRateTable は立花証券 e 支店の定額コース（現物）の段階。
// 12 万円まで 0 円、20 万円まで 176 円、50 万円まで 253 円、100 万円まで 506 円、
// 以後 100 万円ごとに 253 円ずつ加算される。
var FlatRateTable = []FlatRateTier{
	{decimal.NewFromInt(120_000), decimal.NewFromInt(0)},
	{decimal.NewFromInt(200_000), decimal.NewFromInt(176)},
	{decimal.NewFromInt(500_000), decimal.NewFromInt(253)},
	{decimal.NewFromInt(1_000_000), decimal.NewFromInt(506)},
	{decimal.NewFromInt(2_000_000), decimal.NewFromInt(759)},
	{decimal.NewFromInt(3_000_000), decimal.NewFromInt(1_012)},
	{decimal.NewFromInt(4_000_000), decimal.NewFromInt(1_265)},
	{decimal.NewFromInt(5_000_000), decimal.NewFromInt(1_518)},
	{decimal.NewFromInt(6_000_000), decimal.NewFromInt(1_771)},
	{decimal.NewFromInt(7_000_000), decimal.NewFromInt(2_024)},
	{decimal.NewFromInt(8_000_000), decimal.NewFromInt(2_277)},
	{decimal.NewFromInt(9_000_000), decimal.NewFromInt(2_530)},
	{decimal.NewFromInt(10_000_000), decimal.NewFromInt(2_783)},
}

// FlatRateStepSize と FlatRateStepFee は最上段（1000 万円）を超えたときの刻み。
var (
	FlatRateStepSize = decimal.NewFromInt(1_000_000)
	FlatRateStepFee  = decimal.NewFromInt(253)
)

// FlatRateCommission は定額コースの、その日の現物約定代金合計に対する手数料（1 日分の総額）。
// 0 以下なら 0。
func FlatRateCommission(dayTotal decimal.Decimal) decimal.Decimal {
	if dayTotal.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	for _, tier := range FlatRateTable {
		if dayTotal.LessThanOrEqual(tier.Bound) {
			return tier.Fee
		}
	}
	top := FlatRateTable[len(FlatRateTable)-1]
	extra := dayTotal.Sub(top.Bound).Div(FlatRateStepSize)
	steps := extra.Ceil()
	return top.Fee.Add(FlatRateStepFee.Mul(steps))
}

// MarginalFlatRateCommission はこの注文で増える手数料。
// 定額コースは 1 日の合計で段階が決まるので、当日の既約定分を含めた差分を取る。
func MarginalFlatRateCommission(dayTotalBefore, amount decimal.Decimal) decimal.Decimal {
	return FlatRateCommission(dayTotalBefore.Add(amount)).Sub(FlatRateCommission(dayTotalBefore))
}

// priceLimiter は時価問合の送信上限（4 回 / 秒）。
//
// 母集団が数千銘柄あると 1 回の open で 120 銘柄ずつ数十リクエストを連射することになり、
// 上限に当たって寄付の判断が止まる。上限はブローカー側の口（sUrlPrice）ごとなので、
// プロセスに 1 つ持てば足りる。
var priceLimiter = sync.OnceValue(func() *RateLimiter {
	return NewRateLimiter(Limit{Calls: 4, PerSeconds: 1.0})
})

const (
	// MarketPriceBatch は時価問合 1 リクエストの銘柄数の上限。
	MarketPriceBatch = 120
	// MarketPriceColumns は取得する項目（始値・現在値・現在値時刻・前日終値）。
	MarketPriceColumns = "pDOP,pDPP,tDPP:T,pPRP"
)

// MarketPrice は 1 銘柄ぶんの時価。取れなかった値はゼロ。
type MarketPrice struct {
	Symbol string
	// Open は当日始値（寄付前は 0）。
	Open decimal.Decimal
	// Last は現在値。
	Last decimal.Decimal
	// PrevClose は前日終値。
	PrevClose decimal.Decimal
	// At は現在値の時刻（UTC）。読めなければ取得時刻。
	At time.Time
}

// MarketPrices は時価問合（CLMMfdsGetMarketPrice）。銘柄 → 時価。
//
// 寄付後は始値（pDOP）、寄り前は現在値（pDPP）が気配になる。どちらを使うかは
// 呼び出し側（daytrade/quotes）の判断なので、ここでは両方そのまま返す。
func (t *TachibanaBroker) MarketPrices(symbols []string) (map[string]MarketPrice, error) {
	rows, err := t.MarketPricesRaw(symbols, MarketPriceColumns)
	if err != nil {
		return nil, err
	}
	found := make(map[string]MarketPrice, len(rows))
	for _, row := range rows {
		symbol := strings.TrimSpace(fmt.Sprint(row["sIssueCode"]))
		if symbol == "" || symbol == "<nil>" {
			continue
		}
		found[symbol] = MarketPrice{
			Symbol:    symbol,
			Open:      priceDecimal(row["pDOP"]),
			Last:      priceDecimal(row["pDPP"]),
			PrevClose: priceDecimal(row["pPRP"]),
			At:        priceTime(row["tDPP:T"]),
		}
	}
	return found, nil
}

// PriceBatchFailure は時価問合で取れなかった 1 バッチ。
type PriceBatchFailure struct {
	// Index は何本目か（1 始まり）、Batches は全体の本数。
	Index, Batches int
	// Symbols はそのバッチの銘柄。First は先頭（ログで銘柄コードの誤りを切り分ける）。
	Symbols []string
	Err     error
}

func (f PriceBatchFailure) Error() string {
	first := ""
	if len(f.Symbols) > 0 {
		first = f.Symbols[0]
	}
	return fmt.Sprintf("バッチ %d/%d（%d 銘柄、先頭 %s）: %v", f.Index, f.Batches, len(f.Symbols), first, f.Err)
}

// MarketPricesRaw は時価問合の応答を**そのまま**返す（1 要素 = 1 銘柄）。
//
// 板の列を増やすときに「その列名が実際に何を返すか」を確かめる口。解釈を挟まないので、
// 仕様書に無い列も、値が空（"*"）で返る列も、そのまま見える。columns が空なら
// MarketPriceColumns。1 リクエスト 120 銘柄までなので、それを超える分は分割して送る。
//
// 全バッチが揃わなければ失敗（発注の判断に欠けた気配を使わせない）。取れたぶんだけでも
// 欲しい記録（daytrade snap）は MarketPricesRawPartial を使う。
func (t *TachibanaBroker) MarketPricesRaw(symbols []string, columns string) ([]map[string]any, error) {
	rows, failed := t.MarketPricesRawPartial(symbols, columns)
	if len(failed) > 0 {
		return nil, &BrokerError{Message: fmt.Sprintf(
			"立花証券の時価取得に失敗しました（%d/%d バッチ。%v）", len(failed), failed[0].Batches, failed[0])}
	}
	return rows, nil
}

// MarketPricesRawPartial は取れたバッチの行と、取れなかったバッチを返す。
//
// 1 周目で失敗したバッチは、締め切りに掛からなければ**そのバッチだけ**もう 1 周取り直す
// （1 本の通信エラーの送り直しは postTo が 1 回だけやるので、ここはその上の段）。
// 31 バッチのうち 1 本が落ちただけで 30 本ぶんの板を捨てるのは、遡れない記録では痛い。
func (t *TachibanaBroker) MarketPricesRawPartial(symbols []string, columns string) ([]map[string]any, []PriceBatchFailure) {
	if strings.TrimSpace(columns) == "" {
		columns = MarketPriceColumns
	}
	wanted := make([]string, 0, len(symbols))
	seen := map[string]struct{}{}
	for _, s := range symbols {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		wanted = append(wanted, s)
	}

	batches := (len(wanted) + MarketPriceBatch - 1) / MarketPriceBatch
	started := time.Now()
	var pending []PriceBatchFailure
	for start := 0; start < len(wanted); start += MarketPriceBatch {
		end := min(start+MarketPriceBatch, len(wanted))
		pending = append(pending, PriceBatchFailure{Index: start/MarketPriceBatch + 1, Batches: batches, Symbols: wanted[start:end]})
	}

	found := make([]map[string]any, 0, len(wanted))
	for pass := 1; pass <= 2 && len(pending) > 0; pass++ {
		if pass == 2 {
			if t.expired() {
				break
			}
			t.logWarn("broker.price_retry", "取れなかったバッチだけ取り直す", map[string]any{
				"failed": len(pending), "batches": batches, "elapsed_ms": time.Since(started).Milliseconds(),
			})
		}
		var failed []PriceBatchFailure
		for _, b := range pending {
			rows, err := t.marketPriceBatch(b.Symbols, columns)
			if err != nil {
				b.Err = fmt.Errorf("開始から %dms: %w", time.Since(started).Milliseconds(), err)
				failed = append(failed, b)
				// 締め切りを過ぎたら残りは送っても無駄（全部 ErrDeadline になる）
				var deadline *ErrDeadline
				if errors.As(err, &deadline) {
					for _, rest := range pending[indexOfBatch(pending, b.Index)+1:] {
						rest.Err = err
						failed = append(failed, rest)
					}
					break
				}
				continue
			}
			found = append(found, rows...)
		}
		pending = failed
	}
	return found, pending
}

func indexOfBatch(batches []PriceBatchFailure, index int) int {
	for i, b := range batches {
		if b.Index == index {
			return i
		}
	}
	return len(batches) - 1
}

// marketPriceBatch は 1 リクエストぶん（120 銘柄まで）の時価問合。
func (t *TachibanaBroker) marketPriceBatch(batch []string, columns string) ([]map[string]any, error) {
	// 締め切りが過ぎていれば待たずに諦める（次の cron が取り直す）
	if t.expired() {
		return nil, &ErrDeadline{CLMID: clmMarketPrice, Deadline: t.deadline}
	}
	// 上限に当たったら例外にせず待つ（「制限に当たったので取れませんでした」より良い）
	if _, err := priceLimiter().Acquire(); err != nil {
		return nil, fmt.Errorf("時価問合の送信待ちに失敗しました: %w", err)
	}
	res, err := t.postPriceRequest(clmMarketPrice, map[string]any{
		"sTargetIssueCode": strings.Join(batch, ","),
		"sTargetColumn":    columns,
	})
	if err != nil {
		return nil, err
	}
	raw, _ := res["aCLMMfdsMarketPrice"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if row, ok := item.(map[string]any); ok {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// priceDecimal は時価問合の値を Decimal にする。空・"*"（値無し）はゼロ。
func priceDecimal(value any) decimal.Decimal {
	if value == nil {
		return decimal.Zero
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "*" || text == "<nil>" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// priceTime は tDPP:T（現在値時刻、JST の時刻のみ）を UTC の時刻にする。
// 日付は今日（JST）を当てる。形式が読めなければ現在時刻。
func priceTime(value any) time.Time {
	now := clock.NowUTC()
	if value == nil {
		return now
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" {
		return now
	}
	today := clock.ToZone(now, clock.Tokyo)
	for _, layout := range []string{"15:04:05", "150405", "15:04", "1504"} {
		parsed, err := time.Parse(layout, text)
		if err != nil {
			continue
		}
		at := time.Date(today.Year(), today.Month(), today.Day(),
			parsed.Hour(), parsed.Minute(), parsed.Second(), 0, clock.Tokyo)
		return at.UTC()
	}
	return now
}
