package main

import "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"

// notifyAlert は運用通知。Webhook が未設定なら黙って何もしない。
func notifyAlert(title, body string) error {
	notify.Alert(title, body, logger)
	return nil
}
