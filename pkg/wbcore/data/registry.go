package data

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/registry"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

// 市場データ取得元の登録簿。設定の data_provider からプロバイダを引く。
//
// 取得元を足すときは MarketDataProvider を満たす型を書き、ここに登録する。

// ProviderParams は取得元を組み立てるのに必要な環境。
type ProviderParams struct {
	Env      settings.Environment
	Market   domain.Market
	Settings *settings.AppSettings
}

// providerFactory は環境と市場から取得元を組み立てる。
type providerFactory func(params ProviderParams) (MarketDataProvider, error)

// providers は名前 → 組み立て。汎用の registry.Registry を、この用途の
// 引数（ProviderParams）に合わせて包んでいる。
var providers = registry.New[providerFactory]("データソース")

func init() {
	providers.MustRegister(ProviderJQuants, "J-Quants（日本株の日足。公式）", func(map[string]any) (providerFactory, error) {
		return connectJQuants, nil
	})
	providers.MustRegister(ProviderFred, "FRED（米国の指数。判断材料）", func(map[string]any) (providerFactory, error) {
		return connectFred, nil
	})
}

// 登録済みの取得元の名前。
const (
	ProviderJQuants = "jquants"
	ProviderFred    = "fred"
)

// Available は登録済みの取得元の名前（辞書順）。
func Available() []string { return providers.Available() }

// Describe は取得元の名前と 1 行説明。
func Describe() []registry.Described { return providers.Describe() }

// Connect は名前で選んだ取得元を組み立てる。
//
// 未知の名前には候補を添える。その取得元が市場に対応していない場合は
// MarketDataError を返す。
func Connect(name string, params ProviderParams) (MarketDataProvider, error) {
	factory, err := providers.Create(name, nil)
	if err != nil {
		return nil, err
	}
	return factory(params)
}

// defaultProviders は設定で取得元を省略したときの既定。
// 日本株は公式の J-Quants。米国は売買しないので、判断材料に使う指数
// （S&P500 / VIX など）を FRED から取る。
var defaultProviders = map[domain.Market]string{
	domain.MarketJP: ProviderJQuants,
	domain.MarketUS: ProviderFred,
}

// DefaultProvider は市場ごとの既定の取得元の名前。
func DefaultProvider(market domain.Market) string {
	if name, ok := defaultProviders[market]; ok {
		return name
	}
	return ProviderJQuants
}

// jquantsProvider は既存の JQuantsClient を MarketDataProvider に合わせる薄い層。
//
// arch があれば API より先に読む。cron（jquants sync）が毎日蓄積している
// 端点をもう一度 API から取り直すと、レート制限を無駄に消費する。
type jquantsProvider struct {
	client *JQuantsClient
	arch   BarArchive
}

func (p *jquantsProvider) Name() string { return ProviderJQuants }

func (p *jquantsProvider) FetchBars(symbols []string, start, end string) (map[string][]domain.Bar, error) {
	result := map[string][]domain.Bar{}
	for _, symbol := range symbols {
		code, isIndex, err := ToJQuantsCode(symbol)
		if err != nil {
			// 米国の指数（^GSPC 等）が混ざっていても他の銘柄は取る。
			// 取れなかった銘柄はキーごと省く（抽象の約束）
			fmt.Fprintf(os.Stderr, "[warn] %s は J-Quants で取れません: %v\n", symbol, err)
			continue
		}
		bars, ok := archiveDailyBarsFor(p.arch, symbol, code, isIndex, start, end)
		if !ok {
			var err error
			bars, err = fetchJQuantsDaily(p.client, p.arch, symbol, code, isIndex, start, end)
			if err != nil {
				return nil, NewMarketDataError(p.Name(), symbol+" の日足を取得できません", err)
			}
		}
		normalized, err := NormalizeBars(bars)
		if err != nil {
			return nil, NewMarketDataError(p.Name(), symbol+" の足が不正です", err)
		}
		if len(normalized) > 0 {
			result[symbol] = FilterBars(normalized, start, end)
		}
	}
	return result, nil
}

func connectJQuants(params ProviderParams) (MarketDataProvider, error) {
	if params.Market != domain.MarketJP {
		return nil, NewMarketDataError(ProviderJQuants, "日本株以外の市場には対応していません", nil)
	}
	var dotenv map[string]string
	if params.Settings != nil {
		dotenv = params.Settings.DotenvMap
	}
	apiKey, err := credentials.LoadAPIKey(JQuantsAPIKeyVar, dotenv)
	if err != nil {
		return nil, NewMarketDataError(ProviderJQuants, "API キーを解決できません", err)
	}
	var arch BarArchive
	if params.Settings != nil {
		arch = archive.NewArchive(params.Settings.JQuantsArchiveDir())
	}
	return &jquantsProvider{client: NewJQuantsClient(apiKey), arch: arch}, nil
}

// fredProvider は既存の FREDProvider を MarketDataProvider に合わせる薄い層。
type fredProvider struct {
	client *FREDProvider
}

func (p *fredProvider) Name() string { return ProviderFred }

func (p *fredProvider) FetchBars(symbols []string, start, end string) (map[string][]domain.Bar, error) {
	result := map[string][]domain.Bar{}
	for _, symbol := range symbols {
		bars, err := p.client.FetchBars(symbol, start, end)
		if err != nil {
			return nil, NewMarketDataError(p.Name(), symbol+" の系列を取得できません", err)
		}
		normalized, err := NormalizeBars(bars)
		if err != nil {
			return nil, NewMarketDataError(p.Name(), symbol+" の足が不正です", err)
		}
		if len(normalized) > 0 {
			result[symbol] = FilterBars(normalized, start, end)
		}
	}
	return result, nil
}

func connectFred(params ProviderParams) (MarketDataProvider, error) {
	if params.Market != domain.MarketUS {
		return nil, NewMarketDataError(ProviderFred, "米国の指数以外には対応していません", nil)
	}
	return &fredProvider{client: NewFREDProvider(0)}, nil
}

// fetchJQuantsDaily は 1 銘柄の日足を J-Quants から取る。
//
// 既存の JQuantsClient は汎用の HTTP 層だけを持つので、日足の問い合わせ方
// （エンドポイントと日付の書式）はここで組み立てる。code と isIndex は
// ToJQuantsCode で解決済みのものを受け取る——指数と株式でエンドポイントが違う。
func fetchJQuantsDaily(client *JQuantsClient, arch BarArchive, symbol, code string, isIndex bool, start, end string) ([]domain.Bar, error) {
	params := url.Values{}
	params.Set("code", code)
	if start != "" {
		params.Set("from", strings.ReplaceAll(start, "-", ""))
	}
	if end != "" {
		params.Set("to", strings.ReplaceAll(end, "-", ""))
	}

	resp, err := client.Get(JQuantsDailyPath(isIndex), params)
	if err != nil {
		return nil, err
	}

	// 取れたぶんは保管庫に書き戻す。次回は API を叩かずに済む
	storeToArchive(arch, isIndex, resp.Data)

	bars := make([]domain.Bar, 0, len(resp.Data))
	for _, item := range resp.Data {
		var raw JQuantsDailyBarRaw
		if err := json.Unmarshal(item, &raw); err != nil {
			continue
		}
		bars = append(bars, raw.ToBar(symbol, isIndex))
	}
	return bars, nil
}
