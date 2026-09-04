// Package notify は Discord への運用通知と日次レポートの配達。
//
// 送るのは **Bot の REST API**（Webhook は使わない）。Webhook は通常のテキスト
// チャンネルでスレッドを作れず、投稿がチャンネルに平積みになるため。Bot なら
// 「スレッドを作る → その中に本文を送る」ができ、1 通知 1 スレッドで並ぶ。
//
// 設定は 3 つ。Bot のトークンと、送り先チャンネルの ID 2 つ:
//
//	WBJP_DISCORD_BOT_TOKEN … Bot のトークン
//	WBJP_ALERT_CHANNEL_ID  … 異常（動作に問題があったとき）の報告先
//	WBJP_REPORT_CHANNEL_ID … 日次レポートの送り先（無ければアラートと同じ）
//
// Bot はそのサーバーに「チャンネルを見る」「メッセージを送信」「公開スレッドの作成」
// 「スレッドでメッセージを送信」の権限で入っていること。
package notify

import (
	"fmt"
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

const (
	// BotTokenEnvVar は Discord の Bot トークン。
	BotTokenEnvVar = "WBJP_DISCORD_BOT_TOKEN"
	// AlertChannelEnvVar は運用通知（アラート）を出すチャンネルの ID。
	AlertChannelEnvVar = "WBJP_ALERT_CHANNEL_ID"
	// ReportChannelEnvVar は日次レポートを出すチャンネルの ID。
	// 未設定なら AlertChannelEnvVar に流す（同じ場所にまとめる）。
	ReportChannelEnvVar = "WBJP_REPORT_CHANNEL_ID"

	UserAgent = "wbjp/1.0 (+https://github.com/lovemoneyhotspring/jstock-go)"
)

// BotToken は Discord の Bot トークン（未設定なら空）。
func BotToken() string { return os.Getenv(BotTokenEnvVar) }

// AlertChannelID は異常通知の送り先チャンネル。
func AlertChannelID() string { return os.Getenv(AlertChannelEnvVar) }

// ReportChannelID は日次レポートの送り先。専用の設定が無ければアラートと同じ。
func ReportChannelID() string {
	if id := os.Getenv(ReportChannelEnvVar); id != "" {
		return id
	}
	return AlertChannelID()
}

// missingConfig は送り先が揃っていないときの理由（揃っていれば空）。
func missingConfig(channelID string, channelEnv string) string {
	switch {
	case BotToken() == "" && channelID == "":
		return fmt.Sprintf("%s と %s が未設定", BotTokenEnvVar, channelEnv)
	case BotToken() == "":
		return BotTokenEnvVar + " が未設定"
	case channelID == "":
		return channelEnv + " が未設定"
	}
	return ""
}

// Alert は運用通知（アラート）を Discord に送る。
//
// 呼ぶたびに新しいスレッドを作り、その中に本文を入れる。固定のスレッドを
// 使い回さないので、通知ごとに読み分けられる。
func Alert(title, body string, logger *logging.Logger) bool {
	if reason := missingConfig(AlertChannelID(), AlertChannelEnvVar); reason != "" {
		if logger != nil {
			logger.Warn("notify.skipped", fmt.Sprintf("通知先未設定（%s）: %s (本文: %s)", reason, title, body))
		}
		return false
	}

	text := fmt.Sprintf("[wbjp] %s", title)
	if body != "" {
		text = fmt.Sprintf("%s\n%s", text, body)
	}

	if _, err := PostThread(AlertChannelID(), title, text); err != nil {
		if logger != nil {
			logger.Error("notify.failed", fmt.Sprintf("通知送信失敗: %v", err))
		}
		return false
	}
	return true
}
