package notify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDiscord は Discord の REST API の受け皿。
//
// GET  /channels/{id}          … チャンネルの種別
// POST /channels/{id}/threads  … スレッド作成（作った ID を返す）
// POST /channels/{id}/messages … メッセージ 1 通
type fakeDiscord struct {
	// kinds はチャンネル ID → 種別（未登録は 0 = テキスト）。
	kinds map[string]int
	// threads は作られたスレッド（親チャンネル → 送った内容）。
	threads []threadCall
	// messages は送られたメッセージ（宛先 → 本文）。
	messages []messageCall
	// auth は受け取った Authorization ヘッダ。
	auth []string
	// fail は「この宛先とメソッドはエラーを返す」指定。
	fail func(method, path string) (status, code int, ok bool)
	next int
}

type threadCall struct {
	Parent string
	Name   string
	Type   any
	// First はフォーラムで一緒に送られた最初のメッセージ（無ければ空）。
	First string
	ID    string
}

type messageCall struct {
	Channel string
	Content string
}

// start は試験用サーバーを立て、送り先の環境変数をそこに向ける。
// 開発機の .env が環境に載っていても本物の Discord に飛ばないよう、
// トークンとチャンネル ID は必ず上書きする。
func (f *fakeDiscord) start(t *testing.T) *fakeDiscord {
	t.Helper()
	if f.kinds == nil {
		f.kinds = map[string]int{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 || parts[0] != "channels" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		id := parts[1]
		if f.fail != nil {
			if status, code, ok := f.fail(r.Method, r.URL.Path); ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": "だめ"})
				return
			}
		}
		var body map[string]any
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && len(parts) == 2:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "type": f.kinds[id]})
		case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "threads":
			f.next++
			call := threadCall{
				Parent: id, ID: fmt.Sprintf("thread-%d", f.next),
				Type: body["type"],
			}
			call.Name, _ = body["name"].(string)
			if msg, ok := body["message"].(map[string]any); ok {
				call.First, _ = msg["content"].(string)
			}
			f.threads = append(f.threads, call)
			f.kinds[call.ID] = 11
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": call.ID})
		case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "messages":
			content, _ := body["content"].(string)
			f.messages = append(f.messages, messageCall{Channel: id, Content: content})
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	old := discordAPIBase
	discordAPIBase = srv.URL
	t.Cleanup(func() { discordAPIBase = old })

	t.Setenv(BotTokenEnvVar, "test-token")
	t.Setenv(AlertChannelEnvVar, "alert-ch")
	t.Setenv(ReportChannelEnvVar, "report-ch")
	t.Setenv(MentionEnvVar, "")
	// 控えは試験ごとの一時ディレクトリへ（本物の state/notify を汚さない）
	t.Setenv(archiveDirEnvVar, t.TempDir())
	return f
}

// contents は宛先ごとの本文。
func (f *fakeDiscord) contents(channel string) []string {
	var out []string
	for _, m := range f.messages {
		if m.Channel == channel {
			out = append(out, m.Content)
		}
	}
	return out
}

// テキストチャンネルでは、スレッドを作ってから本文をその中に送る。
// 長い本文はページに分かれ、すべて同じスレッドに入る。
func TestPostDocumentCreatesThreadInTextChannel(t *testing.T) {
	f := (&fakeDiscord{}).start(t)

	long := strings.Repeat("あ\n", 1500) // ChunkLimit 超えで複数ページ
	ok, err := PostDocument(long, "日次レポート 2026-09-04（金）")
	if err != nil || !ok {
		t.Fatalf("PostDocument failed: ok=%v err=%v", ok, err)
	}

	if len(f.threads) != 1 {
		t.Fatalf("スレッドの作成回数 = %d, want 1", len(f.threads))
	}
	thread := f.threads[0]
	if thread.Parent != "report-ch" {
		t.Errorf("スレッドの親 = %s, want report-ch", thread.Parent)
	}
	if thread.Name != "日次レポート 2026-09-04（金）" {
		t.Errorf("スレッド名 = %q", thread.Name)
	}
	if thread.Type != float64(11) {
		t.Errorf("スレッドの種別 = %v, want 11（公開スレッド）", thread.Type)
	}
	if thread.First != "" {
		t.Errorf("テキストチャンネルなのに作成時に本文を入れた: %q", thread.First)
	}

	pages := f.contents(thread.ID)
	if len(pages) < 2 {
		t.Fatalf("ページ数 = %d, want 2 以上", len(pages))
	}
	if !strings.HasPrefix(pages[0], "**日次レポート 2026-09-04（金）**") {
		t.Errorf("1 ページ目に見出しが無い: %.40q", pages[0])
	}
	if !strings.Contains(pages[0], fmt.Sprintf("_(1/%d)_", len(pages))) {
		t.Errorf("ページ番号が付いていない: %q", pages[0][len(pages[0])-20:])
	}
	if n := len(f.contents("report-ch")); n != 0 {
		t.Errorf("チャンネルに直接送ったメッセージが %d 件ある（全部スレッドに入るべき）", n)
	}
	for _, a := range f.auth {
		if a != "Bot test-token" {
			t.Fatalf("Authorization = %q, want Bot test-token", a)
		}
	}
}

// フォーラム/メディアチャンネルはスレッド作成に最初のメッセージが要る。
// 1 ページ目はそこで送られるので、二重に送らない。
func TestPostDocumentCreatesForumPost(t *testing.T) {
	f := (&fakeDiscord{kinds: map[string]int{"report-ch": channelForum}}).start(t)

	long := strings.Repeat("い\n", 1500)
	if ok, err := PostDocument(long, "日次レポート"); err != nil || !ok {
		t.Fatalf("PostDocument failed: ok=%v err=%v", ok, err)
	}

	if len(f.threads) != 1 || f.threads[0].First == "" {
		t.Fatalf("フォーラムの投稿に本文が入っていない: %+v", f.threads)
	}
	thread := f.threads[0]
	if thread.Type != nil {
		t.Errorf("フォーラムに type を送っている: %v", thread.Type)
	}
	if !strings.Contains(thread.First, "_(1/") {
		t.Errorf("作成時の本文が 1 ページ目でない: %.40q", thread.First)
	}
	pages := f.contents(thread.ID)
	for _, p := range pages {
		if strings.Contains(p, "_(1/") {
			t.Error("1 ページ目を二重に送っている")
		}
	}
}

// 送り先が既にスレッドなら、新しく作らずそこへ追記する。
func TestPostThreadAppendsToExistingThread(t *testing.T) {
	f := (&fakeDiscord{kinds: map[string]int{"thr-9": 11}}).start(t)

	id, err := PostThread("thr-9", "見出し", "本文")
	if err != nil {
		t.Fatal(err)
	}
	if id != "thr-9" {
		t.Errorf("返ったスレッド ID = %q, want thr-9", id)
	}
	if len(f.threads) != 0 {
		t.Errorf("スレッドの中でスレッドを作っている: %+v", f.threads)
	}
	if got := f.contents("thr-9"); len(got) != 1 || got[0] != "本文" {
		t.Errorf("スレッドへの追記 = %v", got)
	}
}

// PostThread が返したスレッド ID に PostMessage で続きを足せる。
func TestPostThreadReturnsIDForFollowUps(t *testing.T) {
	f := (&fakeDiscord{}).start(t)

	id, err := PostThread("report-ch", "スレッド送信テスト", "1 通目")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.threads) != 1 || id != f.threads[0].ID {
		t.Fatalf("返ったスレッド ID = %q, 作られたスレッド = %+v", id, f.threads)
	}
	if err := PostMessage(id, "2 通目"); err != nil {
		t.Fatal(err)
	}
	if got := f.contents(id); len(got) != 2 || got[0] != "1 通目" || got[1] != "2 通目" {
		t.Errorf("同じスレッドに 2 通入っていない: %v", got)
	}
}

// Alert は呼ぶたびに別のスレッドを作り、本文に [wbjp] を付けてアラートの
// チャンネルへ送る（レポートのチャンネルには送らない）。
func TestAlertCreatesThreadPerCall(t *testing.T) {
	f := (&fakeDiscord{}).start(t)

	if !Alert("障害A", "詳細A", nil) {
		t.Fatal("1 回目の Alert が失敗")
	}
	if !Alert("障害A", "詳細A", nil) {
		t.Fatal("2 回目の Alert が失敗")
	}
	if len(f.threads) != 2 {
		t.Fatalf("スレッドの作成回数 = %d, want 2", len(f.threads))
	}
	for i, thread := range f.threads {
		if thread.Parent != "alert-ch" {
			t.Errorf("%d 回目の親 = %s, want alert-ch", i+1, thread.Parent)
		}
		if thread.Name != "障害A" {
			t.Errorf("%d 回目のスレッド名 = %q", i+1, thread.Name)
		}
		body := f.contents(thread.ID)
		if len(body) != 1 || !strings.HasPrefix(body[0], "[wbjp] 障害A") {
			t.Errorf("%d 回目の本文 = %v", i+1, body)
		}
	}
	if f.threads[0].ID == f.threads[1].ID {
		t.Error("同じスレッドを使い回している")
	}
}

// 設定が欠けていれば送らない（例外にせず false）。
func TestAlertSkipsWhenNotConfigured(t *testing.T) {
	t.Setenv(BotTokenEnvVar, "")
	t.Setenv(AlertChannelEnvVar, "")
	t.Setenv(ReportChannelEnvVar, "")
	if Alert("障害", "詳細", nil) {
		t.Error("未設定なのに送ったことになっている")
	}
	if _, err := PostDocument("本文", "見出し"); err == nil {
		t.Error("未設定なのに PostDocument が成功している")
	}

	t.Setenv(BotTokenEnvVar, "tok")
	if _, err := PostDocument("本文", "見出し"); err == nil ||
		!strings.Contains(err.Error(), ReportChannelEnvVar) {
		t.Errorf("チャンネル未設定の理由が分からない: %v", err)
	}
}

// 日次レポートの送り先は WBJP_REPORT_CHANNEL_ID が優先、無ければアラートと同じ。
func TestReportChannelFallsBackToAlert(t *testing.T) {
	t.Setenv(AlertChannelEnvVar, "alert-ch")
	t.Setenv(ReportChannelEnvVar, "")
	if got := ReportChannelID(); got != "alert-ch" {
		t.Errorf("専用設定が無いときの送り先 = %q", got)
	}
	t.Setenv(ReportChannelEnvVar, "report-ch")
	if got := ReportChannelID(); got != "report-ch" {
		t.Errorf("専用設定があるときの送り先 = %q", got)
	}
}

// 見出しが無いときは本文の 1 行目（飾りを外したもの）がスレッド名になる。
func TestPostDocumentNamesThreadFromFirstLine(t *testing.T) {
	f := (&fakeDiscord{}).start(t)

	if ok, err := PostDocument("\n**日次レポート 2026-09-04（金）**\n- 本文", ""); err != nil || !ok {
		t.Fatal(err)
	}
	if len(f.threads) != 1 || f.threads[0].Name != "日次レポート 2026-09-04（金）" {
		t.Errorf("スレッド名 = %+v, want 本文の 1 行目", f.threads)
	}
	if got := firstLine("# 見出し\n本文"); got != "見出し" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("   \n\n"); got != "" {
		t.Errorf("空の本文の firstLine = %q", got)
	}
}

// Discord のエラーは状態コードと code を持ったまま上がる（原因が分かる）。
func TestPostDocumentSurfacesDiscordError(t *testing.T) {
	f := &fakeDiscord{fail: func(method, path string) (int, int, bool) {
		if strings.HasSuffix(path, "/threads") {
			return http.StatusForbidden, 50013, true
		}
		return 0, 0, false
	}}
	f.start(t)

	ok, err := PostDocument("本文", "見出し")
	if ok || err == nil {
		t.Fatal("スレッドを作れないのに成功している")
	}
	if !strings.Contains(err.Error(), "50013") || !strings.Contains(err.Error(), "403") {
		t.Errorf("エラーに状態コードと code が無い: %v", err)
	}
}

// 呼びかけ（メンション）は 1 通目の先頭にだけ付く。ページごとに付けると
// 1 投稿で何度も通知が飛ぶ。
func TestMentionOnFirstPageOnly(t *testing.T) {
	f := (&fakeDiscord{}).start(t)
	t.Setenv(MentionEnvVar, "837861747271794751")

	long := strings.Repeat("う\n", 1500)
	if ok, err := PostDocument(long, "日次レポート"); err != nil || !ok {
		t.Fatal(err)
	}
	pages := f.contents(f.threads[0].ID)
	if len(pages) < 2 {
		t.Fatalf("ページ数 = %d, want 2 以上", len(pages))
	}
	if !strings.HasPrefix(pages[0], "<@837861747271794751>\n") {
		t.Errorf("1 通目に呼びかけが無い: %.30q", pages[0])
	}
	for i, p := range pages[1:] {
		if strings.Contains(p, "<@") {
			t.Errorf("%d ページ目にも呼びかけが付いている", i+2)
		}
	}

	// Alert も同じ
	f.threads, f.messages = nil, nil
	if !Alert("障害", "詳細", nil) {
		t.Fatal("Alert が失敗")
	}
	body := f.contents(f.threads[0].ID)
	if len(body) != 1 || !strings.HasPrefix(body[0], "<@837861747271794751>\n[wbjp] 障害") {
		t.Errorf("Alert の本文 = %v", body)
	}
}

// 呼びかけは複数指定でき、<@...> の形で書いてあればそのまま使う。未設定なら付かない。
func TestMentionPrefixForms(t *testing.T) {
	t.Setenv(MentionEnvVar, "")
	if got := mentionPrefix(); got != "" {
		t.Errorf("未設定 = %q, want 空", got)
	}
	t.Setenv(MentionEnvVar, " 111 , <@222> ,, ")
	if got := mentionPrefix(); got != "<@111> <@222>\n" {
		t.Errorf("複数指定 = %q", got)
	}
}

// 送ったものは state/notify に控えが残る（送れなかったものも理由付きで）。
func TestArchiveKeepsSentAndFailed(t *testing.T) {
	f := (&fakeDiscord{}).start(t)

	if !Alert("障害A", "詳細A", nil) {
		t.Fatal("Alert が失敗")
	}
	if ok, err := PostDocument("レポート本文", "日次レポート"); err != nil || !ok {
		t.Fatal(err)
	}

	records, err := ReadArchive("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("控えの件数 = %d, want 2", len(records))
	}
	alert, report := records[0], records[1]
	if alert.Kind != KindAlert || alert.Title != "障害A" || !alert.OK || alert.ThreadID == "" {
		t.Errorf("異常通知の控え = %+v", alert)
	}
	if !strings.Contains(alert.Body, "詳細A") {
		t.Errorf("控えに本文が無い: %q", alert.Body)
	}
	if report.Kind != KindReport || report.ChannelID != "report-ch" || !report.OK {
		t.Errorf("レポートの控え = %+v", report)
	}
	if report.At.IsZero() {
		t.Error("控えに時刻が無い")
	}

	// 送れなかったものも残る
	f.fail = func(method, path string) (int, int, bool) { return http.StatusForbidden, 50013, true }
	if Alert("障害B", "詳細B", nil) {
		t.Fatal("送れないのに成功している")
	}
	records, _ = ReadArchive("", "")
	last := records[len(records)-1]
	if last.Title != "障害B" || last.OK || !strings.Contains(last.Error, "50013") {
		t.Errorf("失敗の控え = %+v", last)
	}
}

// 送り先が未設定でも控えは残る（設定を直したあとに何を送り損ねたか分かる）。
func TestArchiveKeepsSkipped(t *testing.T) {
	t.Setenv(BotTokenEnvVar, "")
	t.Setenv(AlertChannelEnvVar, "")
	t.Setenv(ReportChannelEnvVar, "")
	t.Setenv(archiveDirEnvVar, t.TempDir())

	if Alert("障害", "詳細", nil) {
		t.Fatal("未設定なのに送ったことになっている")
	}
	records, err := ReadArchive("", "")
	if err != nil || len(records) != 1 {
		t.Fatalf("控え = %v (err=%v)", records, err)
	}
	if records[0].OK || !strings.Contains(records[0].Error, "未設定") {
		t.Errorf("未設定の控え = %+v", records[0])
	}
}

// 保持日数を過ぎた控えは消える。日付として読めない名前は触らない。
func TestPruneArchiveRemovesOldOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -ArchiveRetainDays-1).Format(archiveDayLayout)
	keep := now.AddDate(0, 0, -ArchiveRetainDays+1).Format(archiveDayLayout)
	for _, name := range []string{old + ".jsonl", keep + ".jsonl", "メモ.jsonl", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pruneArchive(dir, now)

	for name, want := range map[string]bool{
		old + ".jsonl": false, keep + ".jsonl": true, "メモ.jsonl": true, "notes.txt": true,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Errorf("%s が残っている = %v, want %v", name, got, want)
		}
	}
}

// 期間で絞って読める。
func TestReadArchiveFiltersByDay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(archiveDirEnvVar, dir)
	notifyDir := filepath.Join(dir, "notify")
	if err := os.MkdirAll(notifyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2026-09-01", "2026-09-02", "2026-09-03"} {
		line := fmt.Sprintf(`{"at":"%sT00:00:00Z","kind":"alert","title":"%s"}`+"\n", day, day)
		// 壊れた行を混ぜても他が読めること
		if err := os.WriteFile(filepath.Join(notifyDir, day+".jsonl"), []byte("こわれた行\n"+line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadArchive("2026-09-02", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "2026-09-02" || got[1].Title != "2026-09-03" {
		t.Errorf("絞り込み = %+v", got)
	}
}

func TestThreadNameTruncatesAndFallsBack(t *testing.T) {
	if got := threadName(""); got != "wbjp" {
		t.Errorf("空文字のフォールバック = %q", got)
	}
	long := strings.Repeat("あ", 150)
	got := threadName(long)
	if n := len([]rune(got)); n != threadNameLimit {
		t.Errorf("%d 文字に切り詰められていない: %d 文字", threadNameLimit, n)
	}
}
