package broker

// 立花証券の定額コース手数料と、時価問合（CLMMfdsGetMarketPrice）。
//
// 手数料表は 1 日の現物約定代金の**合計**で段階が決まる。1 注文ごとではないので、
// 「この注文でいくら増えるか」は当日の既約定分を含めた差分で見る
// （MarginalFlatRateCommission）。デイトレの検証と発注前の見積りが両方これに依存する。

import (
	"errors"
	"fmt"
	"os"
	"strconv"
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

// priceLimiter は時価問合の送信上限（8 回 / 秒。2026-09-21 に 4 から上げた——8 本を 0.35 秒で
// 送る形を 12 回続けても弾かれなかった。TestPriceParallelProbe）。
//
// 母集団が数千銘柄あると 1 回の open で 120 銘柄ずつ数十リクエストを連射することになり、
// 上限に当たって寄付の判断が止まる。上限はブローカー側の口（sUrlPrice）ごとなので、
// プロセスに 1 つ持てば足りる。
var priceLimiter = sync.OnceValue(func() *RateLimiter {
	return NewRateLimiter(Limit{Calls: 8, PerSeconds: 1.0})
})

// marketPriceStagger は時価問合のバッチを、応答を待たずに番号順にずらして送るときの間隔。
//
// 立花は**届いた順に** p_no が増えていることを検査する。8 本を同時に送ると、先に着いた 1 本だけが通り、
// 残りは p_errno=6「p_no <= 前要求.p_no」で弾かれる（2026-09-21 の実機。TestPriceParallelProbe）。
// 番号順に間隔を空けて送れば通る——960 銘柄 × 8 本が、間隔 50 / 30 / 20 / 10 ms のどれでも 12 回とも
// 8/8、0.35〜0.58 秒（直列は本番の実測で 1.48 秒）。測ったのは連休の深夜で、9:00 は回線が混むので
// 通った下限（10 ms）でなく 50 ms を採る。弾かれたバッチは 2 周目が直列で取り直す。
//
// TACHIBANA_PRICE_STAGGER_MS で上書きできる。0 なら従来の直列（bin を作り直さずに cron の 1 行で戻せる）。
func marketPriceStagger() (stagger time.Duration, invalid string) {
	if raw := strings.TrimSpace(os.Getenv("TACHIBANA_PRICE_STAGGER_MS")); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 0 || ms > 1000 {
			// 直列に戻したつもりの書き間違い（=O、=0ms）を黙って既定にしない。呼び出し側が警告を出す
			return 50 * time.Millisecond, raw
		}
		return time.Duration(ms) * time.Millisecond, ""
	}
	return 50 * time.Millisecond, ""
}

// requestLimiter は発注・照会の口（sUrlRequest）のうち**照会**の送信上限（2 回 / 秒）。
//
// 引けの手仕舞いは注文ごとに単品照会（CLMOrderListDetail）を送るので、銘柄数ぶん連射
// することになる。移植元の Python 実装は残高 2 回/秒・発注 2 回/秒・注文照会 4 回/秒に
// 分けていた。ここは照会をまとめて最も厳しい 2 回/秒に揃える（上限に当たったら待つ）。
var requestLimiter = sync.OnceValue(func() *RateLimiter {
	return NewRateLimiter(Limit{Calls: 2, PerSeconds: 1.0})
})

// orderLimiter は同じ口の**注文**（新規・訂正・取消）の送信上限（2 回 / 秒）。
//
// 2026-09-19 まで照会と 1 つの枠だった。寄付の open は注文の直前に余力（CLMZanKaiSummary）を
// 聞くので、その 1 回が注文の枠を食い、2 本目の注文が約 0.4 秒待たされていた（電文そのものは
// 平均 89 ms。2026-09-15〜18 の実測）。移植元と同じく注文は別枠にする。注文そのものの上限は
// 上げない——実機の上限を確かめていない。口への送信は合計で最大 4 回/秒になる（移植元は 8 回/秒）。
var orderLimiter = sync.OnceValue(func() *RateLimiter {
	return NewRateLimiter(Limit{Calls: 2, PerSeconds: 1.0})
})

// requestLimiterFor は発注・照会の口へ送る電文が使う枠。
func requestLimiterFor(clmID string) *RateLimiter {
	switch clmID {
	case clmNewOrder, clmCorrectOrder, clmCancelOrder:
		return orderLimiter()
	}
	return requestLimiter()
}

const (
	// MarketPriceBatch は時価問合 1 リクエストの銘柄数の上限。
	MarketPriceBatch = 120
	// MarketPriceColumns は取得する項目（始値・現在値・現在値時刻・前日終値・最良気配）。
	//
	// 気配（pQBP / pQAP）を取るのは、**寄り前と未寄付の銘柄は始値も現在値も空**で、
	// 値段が気配にしか無いため（2026-09-11 に実機で確認。docs/OPENING_DATA.md
	// 「実機で確かめること」3）。これを取らないと、寄りが遅れる銘柄＝利益源が
	// 「値段が無い」として候補から消える。
	MarketPriceColumns = "pDOP,pDPP,tDPP:T,pPRP,pQBP,pQAP"
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
	// Bid / Ask は最良買気配値・最良売気配値（無ければゼロ）。寄り前と未寄付の銘柄は
	// ここにしか値段が無い。
	Bid, Ask decimal.Decimal
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
			Bid:       priceDecimal(row["pQBP"]),
			Ask:       priceDecimal(row["pQAP"]),
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
	rows, _, failed := t.MarketPricesRawPartialAt(symbols, columns)
	return rows, failed
}

// MarketPricesRawPartialAt は MarketPricesRawPartial に、行ごとの受信時刻（UTC）を添えて返す。
// received[i] は rows[i] を含むバッチの応答を受け取った時刻。応答の行そのものには手を入れない
// （発注経路の MarketPricesRaw と同じ行を返すため）。1 回の記録が数秒にまたがるので、
// 寄り直前の気配が「何秒目の値か」を後から分けるのに使う（daytrade snap）。
func (t *TachibanaBroker) MarketPricesRawPartialAt(symbols []string, columns string) ([]map[string]any, []time.Time, []PriceBatchFailure) {
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
	received := make([]time.Time, 0, len(wanted))
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
		stagger, invalid := marketPriceStagger()
		if pass == 1 && invalid != "" {
			t.logWarn("broker.price_stagger_invalid", "TACHIBANA_PRICE_STAGGER_MS を読めないので既定の間隔で送る（直列に戻すなら 0）", map[string]any{
				"value": invalid, "stagger_ms": stagger.Milliseconds(),
			})
		}
		if pass == 1 && stagger > 0 && len(pending) > 1 {
			// 1 周目は応答を待たずにずらして送る。取り直し（2 周目）は下の直列——弾かれた理由が
			// 順番なら直列で確実に通り、失効なら postTo がログインし直す
			for _, r := range t.marketPricePipelined(pending, columns, stagger, started) {
				if r.err != nil {
					r.batch.Err = r.err
					failed = append(failed, r.batch)
					continue
				}
				found = append(found, r.rows...)
				for range r.rows {
					received = append(received, r.at)
				}
			}
			// どの形で送ったかを毎回残す（戻したつもりで戻っていない、を朝のログで見分ける）
			t.logInfo("broker.price_pipelined", "時価問合をずらして送った", map[string]any{
				"stagger_ms": stagger.Milliseconds(), "batches": batches, "failed": len(failed),
				"elapsed_ms": time.Since(started).Milliseconds(),
			})
			pending = failed
			continue
		}
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
			at := clock.NowUTC()
			found = append(found, rows...)
			for range rows {
				received = append(received, at)
			}
		}
		pending = failed
	}
	return found, received, pending
}

// pipelinedBatch は marketPricePipelined の 1 バッチぶんの結果。
type pipelinedBatch struct {
	batch PriceBatchFailure
	rows  []map[string]any
	at    time.Time
	err   error
}

// marketPricePipelined はバッチを番号順に stagger ずつずらして送り、応答はまとめて待つ（marketPriceStagger）。
//
// 採番は postTo と同じく flock の中で「読む → 進める → 書く」。**送信を始めてから stagger のあいだロックを
// 握ったままにする**——放すと、並走する別プロセス（snap / wbjp）が大きい番号を先に届かせて、こちらの
// 送りかけが弾かれる。応答の p_errno が 0 でなければそのバッチは失敗として返す（呼び出し側が直列で取り直す）。
// 失効（6・-1・-62 以外）ならセッションを捨てる——取り直しの postTo がログインし直す。
func (t *TachibanaBroker) marketPricePipelined(pending []PriceBatchFailure, columns string, stagger time.Duration, started time.Time) []pipelinedBatch {
	out := make([]pipelinedBatch, len(pending))
	var wg sync.WaitGroup
	for i, b := range pending {
		out[i].batch = b
		fail := func(err error) {
			out[i].err = fmt.Errorf("開始から %dms: %w", time.Since(started).Milliseconds(), err)
		}
		if t.expired() {
			fail(&ErrDeadline{CLMID: clmMarketPrice, Deadline: t.deadline})
			continue
		}
		if _, err := priceLimiter().Acquire(); err != nil {
			fail(fmt.Errorf("時価問合の送信待ちに失敗しました: %w", err))
			continue
		}
		err := func() error {
			t.mu.Lock()
			defer t.mu.Unlock()
			path := t.sessionFilePath()
			unlock, err := lockSession(path)
			if err != nil {
				return err
			}
			defer unlock()
			if err := t.ensureSessionLocked(path); err != nil {
				return err
			}
			pNo := t.session.PNo
			t.session.PNo++
			if err := writeSessionFile(path, t.session); err != nil {
				return fmt.Errorf("セッションを保存できません: %w", err)
			}
			endpoint, timeout := t.endpointOf(interfacePrice)
			wg.Add(1)
			go func() {
				defer wg.Done()
				// この goroutine の panic は cli.Guarded の網に掛からず、通知もなくプロセスが落ちる。
				// バッチの失敗に落とす——気配が揃わないので発注に進まず、通常のエラーとして通知される
				defer func() {
					if p := recover(); p != nil {
						fail(fmt.Errorf("時価問合のバッチで panic: %v", p))
					}
				}()
				res, err := t.sendTo(endpoint, timeout, interfacePrice, pNo, clmMarketPrice, map[string]any{
					"sTargetIssueCode": strings.Join(b.Symbols, ","),
					"sTargetColumn":    columns,
				})
				out[i].at = clock.NowUTC()
				if err != nil {
					fail(err)
					return
				}
				if errno := strings.TrimSpace(text(res["p_errno"])); errno != "" && errno != "0" {
					fail(&ErrPlatform{CLMID: clmMarketPrice, Errno: errno, Text: text(res["p_err"])})
					return
				}
				if err := checkResultOptional(res, clmMarketPrice); err != nil {
					fail(err)
					return
				}
				rows, err := rowsOf(res, marketPriceKey, clmMarketPrice)
				if err != nil {
					fail(err)
					return
				}
				out[i].rows = rows
			}()
			time.Sleep(stagger)
			return nil
		}()
		if err != nil {
			fail(err)
		}
	}
	wg.Wait()

	// 失効らしい応答があればセッションを捨てる（postTo の「それ以外」と同じ扱い）
	for _, r := range out {
		var platform *ErrPlatform
		if errors.As(r.err, &platform) {
			switch platform.Errno {
			case pErrnoPNoOrder, pErrnoArgument, pErrnoOutsideHours:
			default:
				t.mu.Lock()
				path := t.sessionFilePath()
				if unlock, err := lockSession(path); err == nil {
					t.invalidateSessionLocked(path)
					unlock()
				}
				t.mu.Unlock()
				return out
			}
		}
	}
	return out
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
	// 配列のキーが無い応答を「0 行」と読むと、全銘柄が「気配なし」として候補から黙って消える。
	// 形が違えば止める（rowsOf）。sResultCode はこの電文に無いことがあるので、あれば見る
	if err := checkResultOptional(res, clmMarketPrice); err != nil {
		return nil, err
	}
	return rowsOf(res, marketPriceKey, clmMarketPrice)
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
