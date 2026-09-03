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
