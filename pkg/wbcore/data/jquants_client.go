package data

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	JQuantsBaseURL = "https://api.jquants.com/v2"
	DefaultRatePerMin = 100
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
