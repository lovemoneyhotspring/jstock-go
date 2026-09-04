package data

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/shopspring/decimal"
)

// fakeArchive は保管庫の代役。日足とカレンダーを 1 枚ずつ持つだけで、
// 銘柄・列の絞り込みは行わない（試験では 1 銘柄しか置かない）。
type fakeArchive struct {
	bars    *archive.Frame
	cal     *archive.Frame
	upserts int
}

func (a *fakeArchive) Read(ep archive.Endpoint, start, end time.Time) (*archive.Frame, error) {
	if ep.Path == "/markets/calendar" {
		return a.cal, nil
	}
	return a.bars, nil
}

func (a *fakeArchive) ReadWhere(ep archive.Endpoint, opt archive.ReadOptions) (*archive.Frame, error) {
	return a.bars, nil
}

func (a *fakeArchive) Upsert(ep archive.Endpoint, f *archive.Frame) (int, error) {
	a.upserts++
	return f.Height(), nil
}

func archivedBars(t *testing.T, dates ...string) *archive.Frame {
	t.Helper()
	rows := make([]map[string]any, 0, len(dates))
	for _, d := range dates {
		rows = append(rows, map[string]any{
			"Date": d, "Code": "16290",
			"AdjO": "100", "AdjH": "100", "AdjL": "100", "AdjC": "100", "AdjVo": "1000",
		})
	}
	f, err := archive.RowsToFrame(rows, archive.MustEndpoint("/equities/bars/daily"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func tradingCalendar(t *testing.T, dates ...string) *archive.Frame {
	t.Helper()
	rows := make([]map[string]any, 0, len(dates))
	for _, d := range dates {
		rows = append(rows, map[string]any{"Date": d, "HolDiv": "1"})
	}
	f, err := archive.RowsToFrame(rows, archive.CalendarEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// subscriptionFloor は購読の対象になる最古の日（YYYYMMDD）。これより前を含む
// 要求は本物の API と同じく 400 で拒む。
const subscriptionFloor = "20160903"

// newSubscriptionStub は「購読範囲外の from を含む要求は 400」という J-Quants の
// 振る舞いを真似る。受け取った from を froms に積む。
func newSubscriptionStub(t *testing.T, froms *[]string, data []map[string]any) *JQuantsClient {
	t.Helper()
	client, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		*froms = append(*froms, from)
		if from < subscriptionFloor {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message": "Your subscription covers the following dates: 2016-09-03 ~ ."}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	return client
}

func TestJQuantsProviderFetchesOnlyMissingTailFromAPI(t *testing.T) {
	// 保管庫には 9/1・9/2 まで。直近営業日は 9/3 なので揃っていない。
	// 30 年ぶんの要求でも、API には保存済み最終日（9/2）から先だけを求める
	arch := &fakeArchive{
		bars: archivedBars(t, "2026-09-01", "2026-09-02"),
		cal:  tradingCalendar(t, "2026-09-01", "2026-09-02", "2026-09-03"),
	}
	var froms []string
	client := newSubscriptionStub(t, &froms, []map[string]any{
		{"Date": "2026-09-02", "Code": "16290", "AdjO": 105, "AdjH": 105, "AdjL": 105, "AdjC": 105, "AdjVo": 1},
		{"Date": "2026-09-03", "Code": "16290", "AdjO": 110, "AdjH": 110, "AdjL": 110, "AdjC": 110, "AdjVo": 1},
	})
	p := &jquantsProvider{client: client, arch: arch}

	got, err := p.FetchBars([]string{"1629.T"}, "1996-09-10", "2026-09-03")
	if err != nil {
		t.Fatalf("FetchBars: %v", err)
	}
	if len(froms) != 1 || froms[0] != "20260902" {
		t.Fatalf("API への from = %v, want [20260902]（保存済み最終日から先だけ）", froms)
	}
	bars := got["1629.T"]
	if len(bars) != 3 {
		t.Fatalf("本数 = %d, want 3（保管庫 2 本 + API の 9/3）: %v", len(bars), bars)
	}
	// 境界の 9/2 は API 側（訂正後）が勝つ
	if !bars[1].Close.Equal(decimal.NewFromInt(105)) {
		t.Errorf("9/2 の終値 = %s, want 105（API 側で上書き）", bars[1].Close)
	}
	if bars[2].Date != "2026-09-03" {
		t.Errorf("最終日 = %s, want 2026-09-03", bars[2].Date)
	}
	if arch.upserts != 1 {
		t.Errorf("取れたぶんを保管庫に書き戻していない: upserts = %d", arch.upserts)
	}
}

func TestJQuantsProviderSkipsAPIWhenArchiveComplete(t *testing.T) {
	arch := &fakeArchive{
		bars: archivedBars(t, "2026-09-01", "2026-09-02", "2026-09-03"),
		cal:  tradingCalendar(t, "2026-09-01", "2026-09-02", "2026-09-03"),
	}
	var froms []string
	p := &jquantsProvider{client: newSubscriptionStub(t, &froms, nil), arch: arch}

	got, err := p.FetchBars([]string{"1629.T"}, "1996-09-10", "2026-09-03")
	if err != nil {
		t.Fatalf("FetchBars: %v", err)
	}
	if len(froms) != 0 {
		t.Errorf("揃っているのに API を叩いた: %v", froms)
	}
	if len(got["1629.T"]) != 3 {
		t.Errorf("本数 = %d, want 3", len(got["1629.T"]))
	}
}

func TestJQuantsProviderFallsBackToFullRangeWhenArchiveEmpty(t *testing.T) {
	// 保管庫に何も無ければ従来どおり要求範囲をそのまま API に投げる。
	// 購読範囲外を含む要求はそのまま 400 として呼び手に返す（黙って縮めない）
	arch := &fakeArchive{
		bars: archivedBars(t),
		cal:  tradingCalendar(t, "2026-09-03"),
	}
	var froms []string
	p := &jquantsProvider{client: newSubscriptionStub(t, &froms, nil), arch: arch}

	_, err := p.FetchBars([]string{"1629.T"}, "1996-09-10", "2026-09-03")
	if err == nil || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("購読範囲外の要求は 400 のまま返すべき: %v", err)
	}
	if len(froms) != 1 || froms[0] != "19960910" {
		t.Errorf("API への from = %v, want [19960910]", froms)
	}
}
