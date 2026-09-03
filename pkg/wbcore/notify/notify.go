package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

const (
	WebhookEnvVar = "WBJP_ALERT_WEBHOOK_URL"
	UserAgent     = "wbjp/1.0 (+https://github.com/lovemoneyhotspring/jstock-go)"
)

// Alert は Slack または Discord の Webhook へ運用通知（アラート）を送信する。
func Alert(title, body string, logger *logging.Logger) bool {
	webhookURL := os.Getenv(WebhookEnvVar)
	if webhookURL == "" {
		if logger != nil {
			logger.Warn("notify.skipped", fmt.Sprintf("通知先未設定: %s (本文: %s)", title, body))
		}
		return false
	}

	text := fmt.Sprintf("[wbjp] %s", title)
	if body != "" {
		text = fmt.Sprintf("%s\n%s", text, body)
	}

	payload := map[string]string{
		"text":    text,
		"content": text,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(data))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if logger != nil {
			logger.Error("notify.failed", fmt.Sprintf("通知送信失敗: %v", err))
		}
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if logger != nil {
			logger.Error("notify.http_error", fmt.Sprintf("通知先がHTTP %d を返しました", resp.StatusCode))
		}
		return false
	}

	return true
}
