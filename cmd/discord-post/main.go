// discord-post は標準入力の文章を Discord の Incoming Webhook に流す
// （日次レポートの配達係）。
//
//	cat report.md | discord-post
//	discord-post --title "日次レポート" < report.md
//
// 送り先は環境変数 WBJP_ALERT_WEBHOOK_URL。未設定なら何もせず 2 で終了する
// （レポート本体はファイルに残っているので、配達の失敗で処理は止めない）。
//
// 終了コード: 0 = 送信成功 / 1 = 本文が空か送信に失敗 / 2 = 送り先が未設定。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"
)

func main() {
	title := flag.String("title", "", "本文の先頭に付ける見出し")
	flag.Parse()

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "標準入力を読めません: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(string(body)) == "" {
		fmt.Fprintln(os.Stderr, "本文が空。送らない")
		os.Exit(1)
	}
	if os.Getenv(notify.WebhookEnvVar) == "" {
		fmt.Fprintf(os.Stderr, "%s が未設定。送らない\n", notify.WebhookEnvVar)
		os.Exit(2)
	}

	ok, err := notify.PostDocument(string(body), *title)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(1)
	}
}
