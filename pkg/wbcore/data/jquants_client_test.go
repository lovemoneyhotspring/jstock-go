package data

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// newStubClient は試験用のサーバに向けたクライアント。送信間隔は 0（待たない）。
func newStubClient(t *testing.T, handler http.HandlerFunc) (*JQuantsClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewJQuantsClient("test-key")
	client.SetBaseURL(server.URL)
	client.SetRatePerMinute(0)
	return client, server
}

func TestGetAllFollowsPaginationKey(t *testing.T) {
	pages := 0
	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("API キーが送られていない")
		}
		switch r.URL.Query().Get("pagination_key") {
		case "":
			pages++
			fmt.Fprint(w, `{"data":[{"Date":"2025-01-06","Code":"1"}],"pagination_key":"k2"}`)
		case "k2":
			pages++
			fmt.Fprint(w, `{"data":[{"Date":"2025-01-07","Code":"2"}]}`)
		default:
			t.Errorf("知らない頁: %s", r.URL.Query().Get("pagination_key"))
		}
	})

	rows, err := client.GetAll("/equities/bars/daily", map[string]string{"date": "2025-01-06"})
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 {
		t.Errorf("頁数 = %d, want 2", pages)
	}
	if len(rows) != 2 {
		t.Fatalf("行数 = %d, want 2", len(rows))
	}
	if rows[1]["Code"] != "2" {
		t.Errorf("2 頁目が取れていない: %v", rows[1])
	}
}

func TestGetAllKeepsNumberFormatting(t *testing.T) {
	// 数値は表記を変えない（1.0 を 1 にしない）。json.Number で受ける
	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"Close":1.0,"Vol":1000000000000}]}`)
	})
	rows, err := client.GetAll("/equities/bars/daily", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rows[0]["Close"]) != "1.0" {
		t.Errorf("Close = %v, want 1.0", rows[0]["Close"])
	}
	if fmt.Sprint(rows[0]["Vol"]) != "1000000000000" {
		t.Errorf("Vol = %v（指数表記になっていないか）", rows[0]["Vol"])
	}
}

func TestGetAllPassesParams(t *testing.T) {
	var seen url.Values
	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		fmt.Fprint(w, `{"data":[]}`)
	})
	if _, err := client.GetAll("/markets/short-sale-report", map[string]string{"disc_date": "2025-01-06"}); err != nil {
		t.Fatal(err)
	}
	if seen.Get("disc_date") != "2025-01-06" {
		t.Errorf("引数が渡っていない: %v", seen)
	}
}

func TestGetRejects4xx(t *testing.T) {
	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"bad"}`)
	})
	// 4xx は再試行しない（引数の誤りは待っても直らない）
	if _, err := client.GetAll("/equities/bars/daily", nil); err == nil {
		t.Error("4xx でエラーにならない")
	}
}

func TestBulkListAndDownload(t *testing.T) {
	payload := []byte("Date,Code\n2025-01-06,1\n")
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			t.Error("署名付き URL に API キーを送ってはいけない")
		}
		w.Write(payload)
	}))
	defer files.Close()

	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bulk/list":
			if r.URL.Query().Get("endpoint") != "/equities/bars/daily" {
				t.Errorf("endpoint が渡っていない: %v", r.URL.Query())
			}
			fmt.Fprint(w, `{"data":[{"Key":"equities_bars_daily_202501.csv.gz","LastModified":"2025-02-01"}]}`)
		case "/bulk/get":
			fmt.Fprintf(w, `{"url":%q}`, files.URL+"/signed")
		default:
			t.Errorf("知らないパス: %s", r.URL.Path)
		}
	})

	items, err := client.BulkList("/equities/bars/daily")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["Key"] != "equities_bars_daily_202501.csv.gz" {
		t.Fatalf("BulkList = %v", items)
	}
	got, err := client.BulkDownload("equities_bars_daily_202501.csv.gz")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("中身が違う: %q", got)
	}
}

func TestBulkDownloadWithoutURL(t *testing.T) {
	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	})
	if _, err := client.BulkDownload("x.csv.gz"); err == nil {
		t.Error("URL が無ければエラーにすべき")
	}
}

// 保存済みの足が無い銘柄（新規上場・新規追加）は defaultSyncDays ぶん遡った
// 古い start で要求してしまう。プランの対応開始日より前で 400 になったとき、
// その日付に合わせて取り直せることを確認する（取り直さないと、次回同期でも
// 同じ古い start を送り続け、その銘柄だけ永久に同期できない）。
func TestFetchDailyBarsRetriesFromSubscriptionStart(t *testing.T) {
	var seenFrom []string
	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		seenFrom = append(seenFrom, from)
		if from == "19960908" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"message": "Your subscription covers the following dates: 2016-09-03 ~ . If you want more data, please check other plans:https://jpx-jquants.com/#dataset"}`)
			return
		}
		if from != "20160903" {
			t.Errorf("取り直した from が違う: %s", from)
		}
		fmt.Fprint(w, `{"data":[{"Date":"2016-09-05","Code":"1629","AdjC":1000}]}`)
	})

	bars, err := client.FetchDailyBars("1629.T", "1996-09-08", "2026-09-03")
	if err != nil {
		t.Fatalf("取り直しで成功するはず: %v", err)
	}
	if len(bars) != 1 || bars[0].Date != "2016-09-05" {
		t.Fatalf("bars = %v", bars)
	}
	if len(seenFrom) != 2 || seenFrom[0] != "19960908" || seenFrom[1] != "20160903" {
		t.Errorf("要求した from の履歴が違う: %v", seenFrom)
	}
}

// 対応開始日が要求済みの start と同じかそれより前なら、取り直しても結果は
// 変わらない（無限ループの元）ので、そのままエラーを返す。
func TestFetchDailyBarsDoesNotRetryWhenNoEarlierData(t *testing.T) {
	calls := 0
	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message": "Your subscription covers the following dates: 2016-09-03 ~ . ..."}`)
	})

	if _, err := client.FetchDailyBars("1629.T", "2020-01-01", "2026-09-03"); err == nil {
		t.Fatal("エラーになるはず")
	}
	if calls != 1 {
		t.Errorf("取り直すべきでないのに %d 回呼ばれた", calls)
	}
}
