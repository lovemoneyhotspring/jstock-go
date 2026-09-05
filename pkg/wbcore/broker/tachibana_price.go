package broker

// 立花証券の定額コース手数料と、時価問合（CLMMfdsGetMarketPrice）。
//
// 手数料表は 1 日の現物約定代金の**合計**で段階が決まる。1 注文ごとではないので、
// 「この注文でいくら増えるか」は当日の既約定分を含めた差分で見る
// （MarginalFlatRateCommission）。デイトレの検証と発注前の見積りが両方これに依存する。

import (
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

// MarketPricesRaw は時価問合の応答を**そのまま**返す（1 要素 = 1 銘柄）。
//
// 板の列を増やすときに「その列名が実際に何を返すか」を確かめる口。解釈を挟まないので、
// 仕様書に無い列も、値が空（"*"）で返る列も、そのまま見える。columns が空なら
// MarketPriceColumns。1 リクエスト 120 銘柄までなので、それを超える分は分割して送る。
func (t *TachibanaBroker) MarketPricesRaw(symbols []string, columns string) ([]map[string]any, error) {
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

	found := make([]map[string]any, 0, len(wanted))
	batches := (len(wanted) + MarketPriceBatch - 1) / MarketPriceBatch
	started := time.Now()
	for start := 0; start < len(wanted); start += MarketPriceBatch {
		end := min(start+MarketPriceBatch, len(wanted))
		batch := wanted[start:end]
		index := start/MarketPriceBatch + 1
		// 失敗したら「何本目で、どの銘柄から」を残す。レート制限か銘柄コードの誤りかを
		// 後から切り分けるのに要る
		describe := func(err error) error {
			return &BrokerError{Message: fmt.Sprintf(
				"立花証券の時価取得に失敗しました（バッチ %d/%d、%d 銘柄、先頭 %s、開始から %dms）: %v",
				index, batches, len(batch), batch[0], time.Since(started).Milliseconds(), err)}
		}

		// 締め切りが過ぎていれば待たずに諦める（次の cron が取り直す）
		if t.expired() {
			return nil, describe(&ErrDeadline{CLMID: clmMarketPrice, Deadline: t.deadline})
		}
		// 上限に当たったら例外にせず待つ（「制限に当たったので取れませんでした」より良い）
		if _, err := priceLimiter().Acquire(); err != nil {
			return nil, &BrokerError{Message: fmt.Sprintf("時価問合の送信待ちに失敗しました: %v", err)}
		}
		res, err := t.postPriceRequest(clmMarketPrice, map[string]any{
			"sTargetIssueCode": strings.Join(batch, ","),
			"sTargetColumn":    columns,
		})
		if err != nil {
			return nil, describe(err)
		}
		rows, _ := res["aCLMMfdsMarketPrice"].([]any)
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			found = append(found, row)
		}
	}
	return found, nil
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
