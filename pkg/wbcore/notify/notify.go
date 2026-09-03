package notify

import (
	"fmt"
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

const (
	WebhookEnvVar = "WBJP_ALERT_WEBHOOK_URL"
	UserAgent     = "wbjp/1.0 (+https://github.com/lovemoneyhotspring/jstock-go)"
)

// Alert は Discord の Webhook へ運用通知（アラート）を送信する。
//
// 送り先はフォーラムチャンネルの Webhook を想定していて、呼ぶたびに
// thread_name で新しい投稿（スレッド）を作る。固定のスレッド ID を
// 使い回すのではなく、通知のたびに別スレッドになる。
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

	if _, err := postMessage(webhookURL, text, threadName(title), "", true); err != nil {
		if logger != nil {
			logger.Error("notify.failed", fmt.Sprintf("通知送信失敗: %v", err))
		}
		return false
	}
	return true
}
