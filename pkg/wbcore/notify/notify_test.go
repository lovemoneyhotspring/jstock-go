package notify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	if err := PostThread("thr-9", "見出し", "本文"); err != nil {
		t.Fatal(err)
	}
	if len(f.threads) != 0 {
		t.Errorf("スレッドの中でスレッドを作っている: %+v", f.threads)
	}
	if got := f.contents("thr-9"); len(got) != 1 || got[0] != "本文" {
		t.Errorf("スレッドへの追記 = %v", got)
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
