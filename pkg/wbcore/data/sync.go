package data

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
)

// JQuantsDailyBarRaw は日足エンドポイントの応答 1 行。
//
// 株式は**調整済み**（Adj*）を使う。未調整のままバックテストすると、
// 分割日に巨大な偽の下落が現れる。指数は調整の概念が無く出来高も持たないので、
// 別の項目名（O/H/L/C）で返る。どちらの形も受けられるよう両方を持つ。
type JQuantsDailyBarRaw struct {
	Date string `json:"Date"`
	Code string `json:"Code"`

	AdjOpen   any `json:"AdjO"`
	AdjHigh   any `json:"AdjH"`
	AdjLow    any `json:"AdjL"`
	AdjClose  any `json:"AdjC"`
	AdjVolume any `json:"AdjVo"`

	IndexOpen  any `json:"O"`
	IndexHigh  any `json:"H"`
	IndexLow   any `json:"L"`
	IndexClose any `json:"C"`
}

// ISODate は Date を YYYY-MM-DD に揃える（API は YYYYMMDD でも返す）。
func (r JQuantsDailyBarRaw) ISODate() string {
	if len(r.Date) == 8 {
		return fmt.Sprintf("%s-%s-%s", r.Date[:4], r.Date[4:6], r.Date[6:])
	}
	return r.Date
}

// ToBar は応答の 1 行を正規スキーマの日足にする。
//
// 取引の無かった日は価格が null で返る。NormalizeBars が落とす。
func (r JQuantsDailyBarRaw) ToBar(symbol string, isIndex bool) domain.Bar {
	bar := domain.Bar{Symbol: symbol, Date: r.ISODate()}
	if isIndex {
		bar.Open = parseDec(r.IndexOpen)
		bar.High = parseDec(r.IndexHigh)
		bar.Low = parseDec(r.IndexLow)
		bar.Close = parseDec(r.IndexClose)
		bar.Volume = decimal.Zero
		return bar
	}
	bar.Open = parseDec(r.AdjOpen)
	bar.High = parseDec(r.AdjHigh)
	bar.Low = parseDec(r.AdjLow)
	bar.Close = parseDec(r.AdjClose)
	bar.Volume = parseDec(r.AdjVolume)
	return bar
}

func parseDec(v any) decimal.Decimal {
	if v == nil {
		return decimal.Zero
	}
	switch val := v.(type) {
	case float64:
		return decimal.NewFromFloat(val)
	case int64:
		return decimal.NewFromInt(val)
	case int:
		return decimal.NewFromInt(int64(val))
	case string:
		d, err := decimal.NewFromString(val)
		if err == nil {
			return d
		}
	}
	return decimal.Zero
}

// defaultSyncStart は保存済みが無い銘柄を初めて取るときの開始日（過去 5 年分）。
const defaultSyncStart = "2020-01-01"

// SyncSymbolBars は指定された銘柄の日足を適切なプロバイダから取得して BarStore に保存する。
//
// 保存済みがあれば最終日以降だけを取り、Upsert で重ねる（全部取り直さない）。
func SyncSymbolBars(
	symbol string,
	barStore *BarStore,
	jqClient *JQuantsClient,
	fredClient *FREDProvider,
	logger *logging.Logger,
) error {
	return SyncSymbolBarsSince(symbol, barStore, jqClient, fredClient, logger, defaultSyncStart, false)
}

// SyncSymbolBarsSince は取得の開始日を指定できる SyncSymbolBars。
//
// force が false なら、保存済みの最終日が since より後のときはそこから取り直す
// （当日の足は取引所側で後から訂正されうるので、最終日そのものも取り直して
// Upsert で上書きする）。毎回全期間を取り直すと、短い間隔で回る run が
// API のレート制限を食い潰す。force ならその抑制をやめて since から取る。
func SyncSymbolBarsSince(
	symbol string,
	barStore *BarStore,
	jqClient *JQuantsClient,
	fredClient *FREDProvider,
	logger *logging.Logger,
	since string,
	force bool,
) error {
	today := clock.NowUTC().Format("2006-01-02")
	startDate := since
	if startDate == "" {
		startDate = defaultSyncStart
	}
	if !force {
		if last, lerr := barStore.LastDate(symbol); lerr == nil && last > startDate {
			startDate = last
		}
	}

	var bars []domain.Bar
	var err error

	// 取得元は「J-Quants で引けるコードか」で決める。^TOPIX のような東証の
	// 指数は J-Quants の指数エンドポイント、^GSPC のように J-Quants に無い
	// ものは FRED。^ の有無だけで振ると、東証の指数まで FRED に行く。
	code, isIndex, codeErr := ToJQuantsCode(symbol)
	if codeErr != nil {
		// 米国指数/外国指数は FRED から取得
		if fredClient == nil {
			fredClient = NewFREDProvider(15 * time.Second)
		}

		logger.Info("sync.fred", fmt.Sprintf("%s の日足を FRED から取得中...", symbol))
		bars, err = fredClient.FetchBars(symbol, startDate, today)
		if err != nil {
			return fmt.Errorf("FRED からの取得失敗 (%s): %w", symbol, err)
		}
	} else {
		// 日本株・東証の指数は J-Quants から取得
		if jqClient == nil {
			return fmt.Errorf("J-Quants クライアントが初期化されていません（WBJP_JQUANTS_API_KEY が未設定）")
		}

		logger.Info("sync.jquants", fmt.Sprintf("%s の日足を J-Quants から取得中...", code))

		params := url.Values{}
		params.Set("code", code)
		params.Set("from", strings.ReplaceAll(startDate, "-", ""))
		params.Set("to", strings.ReplaceAll(today, "-", ""))

		resp, err := jqClient.Get(JQuantsDailyPath(isIndex), params)
		if err != nil {
			return fmt.Errorf("J-Quants からの取得失敗 (%s): %w", code, err)
		}

		for _, item := range resp.Data {
			var raw JQuantsDailyBarRaw
			if err := json.Unmarshal(item, &raw); err == nil {
				bars = append(bars, raw.ToBar(symbol, isIndex))
			}
		}
	}

	if len(bars) == 0 {
		logger.Warn("sync.empty", fmt.Sprintf("%s の足データが 0 件でした", symbol))
		return nil
	}

	total, err := barStore.Upsert(symbol, bars)
	if err != nil {
		return fmt.Errorf("%s の Parquet 保存失敗: %w", symbol, err)
	}

	logger.Info("sync.success", fmt.Sprintf("%s の日足 %d 件を保存しました（保存後 %d 本、最新: %s）",
		symbol, len(bars), total, bars[len(bars)-1].Date))
	return nil
}
