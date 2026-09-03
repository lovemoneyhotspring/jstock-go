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

type JQuantsDailyBarRaw struct {
	Date   string `json:"Date"`
	Code   string `json:"Code"`
	Open   any    `json:"Open"`
	High   any    `json:"High"`
	Low    any    `json:"Low"`
	Close  any    `json:"Close"`
	Volume any    `json:"Volume"`
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

// SyncSymbolBars は指定された銘柄の日足を適切なプロバイダから取得して BarStore に保存する。
func SyncSymbolBars(
	symbol string,
	barStore *BarStore,
	jqClient *JQuantsClient,
	fredClient *FREDProvider,
	logger *logging.Logger,
) error {
	today := clock.NowUTC().Format("2006-01-02")
	startDate := "2020-01-01" // 過去5年分

	var bars []domain.Bar
	var err error

	if strings.HasPrefix(symbol, "^") {
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
		// 日本株は J-Quants から取得
		if jqClient == nil {
			return fmt.Errorf("J-Quants クライアントが初期化されていません（WBJP_JQUANTS_API_KEY が未設定）")
		}

		code := strings.TrimSuffix(symbol, ".T")
		logger.Info("sync.jquants", fmt.Sprintf("%s の日足を J-Quants から取得中...", code))

		params := url.Values{}
		params.Set("code", code)
		params.Set("from", strings.ReplaceAll(startDate, "-", ""))
		params.Set("to", strings.ReplaceAll(today, "-", ""))

		resp, err := jqClient.Get("/equities/bars/daily", params)
		if err != nil {
			return fmt.Errorf("J-Quants からの取得失敗 (%s): %w", code, err)
		}

		for _, item := range resp.Data {
			var raw JQuantsDailyBarRaw
			if err := json.Unmarshal(item, &raw); err == nil {
				dStr := raw.Date
				if len(dStr) == 8 {
					dStr = fmt.Sprintf("%s-%s-%s", dStr[:4], dStr[4:6], dStr[6:])
				}
				bars = append(bars, domain.Bar{
					Date:   dStr,
					Open:   parseDec(raw.Open),
					High:   parseDec(raw.High),
					Low:    parseDec(raw.Low),
					Close:  parseDec(raw.Close),
					Volume: parseDec(raw.Volume),
					Symbol: symbol,
				})
			}
		}
	}

	if len(bars) == 0 {
		logger.Warn("sync.empty", fmt.Sprintf("%s の足データが 0 件でした", symbol))
		return nil
	}

	if err := barStore.Write(symbol, bars); err != nil {
		return fmt.Errorf("%s の Parquet 保存失敗: %w", symbol, err)
	}

	logger.Info("sync.success", fmt.Sprintf("%s の日足 %d 件を保存しました (最新: %s)", symbol, len(bars), bars[len(bars)-1].Date))
	return nil
}
