// discord-post は標準入力の文章を Discord の Incoming Webhook に流す
// （日次レポートの配達係）。
//
//	cat report.md | discord-post
//	discord-post --title "日次レポート" < report.md
//	discord-post --sections < report.md              # "---" だけの行で切り、節ごとに 1 通ずつ
//	discord-post --sections --sep '<<<>>>' < doc.md  # 本文に "---" が出るとき
//
// 送り先は Discord の Bot（WBJP_DISCORD_BOT_TOKEN）と、チャンネル ID
// WBJP_REPORT_CHANNEL_ID（無ければ WBJP_ALERT_CHANNEL_ID）。未設定なら何もせず
// 2 で終了する（レポート本体はファイルに残っているので、配達の失敗で処理は止めない）。
// 本文はチャンネルに新しいスレッドを作ってその中に入れる。--sections を付けると
// "---" だけの行で節に分け、節ごとに 1 通ずつ送る（節を丸ごとコピーできる）。
// 付けないときは 1 通の上限（2000 字）で機械的に切るので、節の途中で切れることがある。
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
	sections := flag.Bool("sections", false, "区切り行で節に分け、節ごとに 1 通ずつ送る")
	sep := flag.String("sep", notify.SectionSeparator, "節の区切り行（--sections のとき）")
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
	if notify.BotToken() == "" || notify.ReportChannelID() == "" {
		fmt.Fprintf(os.Stderr, "%s と %s（無ければ %s）が要ります。送らない\n",
			notify.BotTokenEnvVar, notify.ReportChannelEnvVar, notify.AlertChannelEnvVar)
		os.Exit(2)
	}

	post := func() (bool, error) { return notify.PostDocument(string(body), *title) }
	if *sections {
		post = func() (bool, error) { return notify.PostSections(notify.SplitSectionsWith(string(body), *sep), *title) }
	}
	ok, err := post()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(1)
	}
}
