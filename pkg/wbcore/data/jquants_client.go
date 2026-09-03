package data

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

const (
	JQuantsBaseURL = "https://api.jquants.com/v2"
	// DefaultRatePerMin は既定の送信上限（回/分）。Standard プランの 120 より
	// 少し下げ、他のプロセス（accum sync と jquants sync の同時実行）の分を残す。
	DefaultRatePerMin = 100
	// JQuantsAPIKeyVar は API キーを置く環境変数（.env でも同じ名前）。
	JQuantsAPIKeyVar = "WBJP_JQUANTS_API_KEY"
	// maxPages は頁送りの上限。壊れた pagination_key で無限ループしないための保険。
	maxPages = 10000
	// downloadRetries は署名付き URL からのダウンロードの再試行回数。
	downloadRetries = 3
)

type JQuantsClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	mu         sync.Mutex
	lastReq    time.Time
	interval   time.Duration
}

func NewJQuantsClient(apiKey string) *JQuantsClient {
	interval := time.Minute / time.Duration(DefaultRatePerMin)
	return &JQuantsClient{
		apiKey:     apiKey,
		baseURL:    JQuantsBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		interval:   interval,
	}
}

func (c *JQuantsClient) throttle() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(c.lastReq)
	if elapsed < c.interval {
		time.Sleep(c.interval - elapsed)
	}
	c.lastReq = time.Now()
}

type JQuantsResponse struct {
	Data          []json.RawMessage `json:"data"`
	PaginationKey string            `json:"pagination_key"`
	// URL は /bulk/get が返す署名付き URL（5 分有効）。
	URL string `json:"url"`
}

// Get は J-Quants API に GET リクエストを送信し、レートリミット・429リトライを処理する。
func (c *JQuantsClient) Get(endpoint string, params url.Values) (*JQuantsResponse, error) {
	reqURL := fmt.Sprintf("%s%s", c.baseURL, endpoint)
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}

	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		c.throttle()

		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			waitSec := 60
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if s, err := strconv.Atoi(ra); err == nil {
					waitSec = s
				}
			}
			time.Sleep(time.Duration(waitSec) * time.Second)
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("J-Quants API error (status %d): %s", resp.StatusCode, string(body))
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}

		var res JQuantsResponse
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("failed to parse J-Quants JSON: %w", err)
		}

		return &res, nil
	}

	return nil, fmt.Errorf("J-Quants request exceeded maximum retries: %s", endpoint)
}

// SetBaseURL は問い合わせ先を差し替える（試験のスタブサーバ用）。
func (c *JQuantsClient) SetBaseURL(url string) { c.baseURL = strings.TrimRight(url, "/") }

// SetRatePerMinute は送信上限（回/分）を変える。0 で無効（試験で待たないため）。
func (c *JQuantsClient) SetRatePerMinute(rate float64) {
	if rate <= 0 {
		c.interval = 0
		return
	}
	c.interval = time.Duration(float64(time.Minute) / rate)
}

// NewJQuantsClientFromEnv は環境変数 / .env の API キーでクライアントを組み立てる。
func NewJQuantsClientFromEnv(dotenv map[string]string) (*JQuantsClient, error) {
	apiKey, err := credentials.LoadAPIKey(JQuantsAPIKeyVar, dotenv)
	if err != nil {
		return nil, fmt.Errorf("J-Quants の API キーがありません。%s を環境変数か .env に設定してください", JQuantsAPIKeyVar)
	}
	return NewJQuantsClient(apiKey), nil
}

// GetAll は pagination_key を辿って data の全行を集める。
//
// 応答は「列名 → 値」の JSON オブジェクトだが、値の型は端点ごとに揺れる
// （数値が文字列で返ることがある）ので、any のまま返して呼び出し側に任せる。
func (c *JQuantsClient) GetAll(path string, params map[string]string) ([]map[string]any, error) {
	query := url.Values{}
	for k, v := range params {
		query.Set(k, v)
	}
	var rows []map[string]any
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("J-Quants の頁送りが終わりません（%s、%d 頁）", path, page)
		}
		resp, err := c.Get(path, query)
		if err != nil {
			return nil, err
		}
		for _, raw := range resp.Data {
			var row map[string]any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber() // 数値の表記を変えない（1.0 を 1 にしない）
			if err := decoder.Decode(&row); err != nil {
				return nil, fmt.Errorf("J-Quants の行を解釈できません（%s）: %w", path, err)
			}
			rows = append(rows, row)
		}
		if resp.PaginationKey == "" {
			return rows, nil
		}
		query.Set("pagination_key", resp.PaginationKey)
	}
}

// BulkList は一括ダウンロードできるファイルの一覧（Key / Size / LastModified）。
func (c *JQuantsClient) BulkList(endpoint string) ([]map[string]any, error) {
	return c.GetAll("/bulk/list", map[string]string{"endpoint": endpoint})
}

// BulkDownload は /bulk/get で署名付き URL を貰い、csv.gz の中身をそのまま返す。
func (c *JQuantsClient) BulkDownload(key string) ([]byte, error) {
	resp, err := c.Get("/bulk/get", url.Values{"key": []string{key}})
	if err != nil {
		return nil, err
	}
	if resp.URL == "" {
		return nil, fmt.Errorf("一括ダウンロードの URL が返りませんでした: key=%s", key)
	}
	return c.download(resp.URL)
}

// download は署名付き URL（別ホスト）から取る。API キーは送らず、
// レート制限にも数えない。
func (c *JQuantsClient) download(signedURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < downloadRetries; attempt++ {
		req, err := http.NewRequest(http.MethodGet, signedURL, nil)
		if err != nil {
			return nil, fmt.Errorf("ダウンロード要求を作れません: %w", err)
		}
		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("一括ファイルの取得に失敗しました: HTTP %d", resp.StatusCode)
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("一括ファイルの取得を拒否されました: HTTP %d %s", resp.StatusCode, truncate(string(body), 200))
		}
		payload, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return payload, nil
	}
	return nil, fmt.Errorf("一括ファイルのダウンロードに失敗しました: %w", lastErr)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// loadJQuantsAPIKey は環境変数 / .env / Keychain から API キーを解決する。
func loadJQuantsAPIKey(dotenv map[string]string) (string, error) {
	return credentials.LoadAPIKey(JQuantsAPIKeyVar, dotenv)
}

// FetchDailyBars は 1 銘柄の日足を取る。symbol は "7203.T" でも "72030" でもよい。
// start / end は "YYYY-MM-DD"（API には区切り無しで渡す）。
func (c *JQuantsClient) FetchDailyBars(symbol, start, end string) ([]domain.Bar, error) {
	code := strings.TrimSuffix(symbol, ".T")
	params := url.Values{}
	params.Set("code", code)
	if start != "" {
		params.Set("from", strings.ReplaceAll(start, "-", ""))
	}
	if end != "" {
		params.Set("to", strings.ReplaceAll(end, "-", ""))
	}
	var bars []domain.Bar
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("J-Quants の頁送りが終わりません（%s）", code)
		}
		resp, err := c.Get("/equities/bars/daily", params)
		if err != nil {
			return nil, err
		}
		for _, item := range resp.Data {
			var raw JQuantsDailyBarRaw
			if err := json.Unmarshal(item, &raw); err != nil {
				continue // 1 行の形が違っても他の足は活かす
			}
			date := raw.Date
			if len(date) == 8 {
				date = fmt.Sprintf("%s-%s-%s", date[:4], date[4:6], date[6:])
			}
			bars = append(bars, domain.Bar{
				Date:   date,
				Open:   parseDec(raw.Open),
				High:   parseDec(raw.High),
				Low:    parseDec(raw.Low),
				Close:  parseDec(raw.Close),
				Volume: parseDec(raw.Volume),
				Symbol: symbol,
			})
		}
		if resp.PaginationKey == "" {
			return bars, nil
		}
		params.Set("pagination_key", resp.PaginationKey)
	}
}
