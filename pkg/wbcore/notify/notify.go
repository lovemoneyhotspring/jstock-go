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
	"strings"

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
	// MentionEnvVar は投稿のたびに呼びかける相手のユーザー ID（カンマ区切りで複数可）。
	// 異常も日次レポートも見落とさないため、**すべての投稿**の先頭に付く。
	MentionEnvVar = "WBJP_MENTION_USER_ID"

	UserAgent = "wbjp/1.0 (+https://github.com/lovemoneyhotspring/jstock-go)"
)

// mentionPrefix は本文の先頭に付ける呼びかけ（未設定なら空）。
//
// Discord は本文の "@名前" では通知を飛ばさない。ユーザー ID を <@ID> の形で
// 書いたときだけ相手に届く。設定に <@...> の形で書いてあればそのまま使う。
func mentionPrefix() string {
	raw := strings.TrimSpace(os.Getenv(MentionEnvVar))
	if raw == "" {
		return ""
	}
	var parts []string
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !strings.HasPrefix(id, "<@") {
			id = "<@" + id + ">"
		}
		parts = append(parts, id)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + "\n"
}

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
// 使い回さないので、通知ごとに読み分けられる。送っても送れなくても
// state/notify に控えを残す（30 日）。
func Alert(title, body string, logger *logging.Logger) bool {
	text := fmt.Sprintf("[wbjp] %s", title)
	if body != "" {
		text = fmt.Sprintf("%s\n%s", text, body)
	}
	rec := Record{Kind: KindAlert, Title: title, Body: text, ChannelID: AlertChannelID()}

	if reason := missingConfig(AlertChannelID(), AlertChannelEnvVar); reason != "" {
		if logger != nil {
			logger.Warn("notify.skipped", fmt.Sprintf("通知先未設定（%s）: %s (本文: %s)", reason, title, body))
		}
		rec.Error = "通知先未設定（" + reason + "）"
		archive(rec)
		return false
	}

	threadID, err := PostThread(AlertChannelID(), title, text)
	rec.ThreadID = threadID
	if err != nil {
		if logger != nil {
			logger.Error("notify.failed", fmt.Sprintf("通知送信失敗: %v", err))
		}
		rec.Error = err.Error()
		archive(rec)
		return false
	}
	rec.OK = true
	archive(rec)
	return true
}
