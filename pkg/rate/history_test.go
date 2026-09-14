package rate

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestMonthRange(t *testing.T) {
	cases := []struct {
		from, to string
		want     []string
		fail     bool
	}{
		{"202511", "202602", []string{"202511", "202512", "202601", "202602"}, false}, // 年またぎ
		{"202509", "202509", []string{"202509"}, false},                               // 同じ月
		{"202602", "202511", nil, true},                                               // 逆順
		{"2025-01", "202602", nil, true},
		{"202513", "202602", nil, true},
		{"20251", "202602", nil, true},
		{"202501", "", nil, true},
	}
	for _, c := range cases {
		got, err := MonthRange(c.from, c.to)
		if (err != nil) != c.fail {
			t.Errorf("MonthRange(%q, %q) err = %v", c.from, c.to, err)
			continue
		}
		if !c.fail && !reflect.DeepEqual(got, c.want) {
			t.Errorf("MonthRange(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

type stubMonths struct {
	pages map[string]string
	errs  map[string]error
	calls []string
}

func (s *stubMonths) fetch(_ context.Context, ym string) (string, error) {
	s.calls = append(s.calls, ym)
	if err := s.errs[ym]; err != nil {
		return "", err
	}
	return s.pages[ym], nil
}

// 取得に失敗したら止める。それまでに済んだ月は残り、再実行で続きから進む。
func TestImportMonthsStopsOnFetchErrorKeepingDoneMonths(t *testing.T) {
	store := openRateStore(t)
	ctx := context.Background()
	src := &stubMonths{
		pages: map[string]string{"200501": tradersSample, "200503": tradersSample},
		errs:  map[string]error{"200502": ErrServerError},
	}
	months := []string{"200501", "200502", "200503"}
	res, err := ImportMonths(ctx, store, months, false, 0, src.fetch, nil)
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("err = %v", err)
	}
	if res.Fetched != 1 || res.Rows != 3 || !reflect.DeepEqual(src.calls, []string{"200501", "200502"}) {
		t.Fatalf("res = %+v calls = %v", res, src.calls)
	}
	done, _ := store.ImportedMonths(ctx)
	if !done["200501"] || done["200502"] || done["200503"] {
		t.Fatalf("取り込み済み = %v", done)
	}

	// 直ったら続きから（済んだ月は飛ばす）
	delete(src.errs, "200502")
	src.pages["200502"] = tradersSample
	src.calls = nil
	res, err = ImportMonths(ctx, store, months, false, 0, src.fetch, nil)
	if err != nil || res.Skipped != 1 || res.Fetched != 2 || !reflect.DeepEqual(src.calls, []string{"200502", "200503"}) {
		t.Fatalf("再実行 res = %+v calls = %v err = %v", res, src.calls, err)
	}
}

// 行を読めない月は止めずに次へ進み、取り込み済みにはしない（次回また取りに行く）。
func TestImportMonthsContinuesOnParseFailure(t *testing.T) {
	store := openRateStore(t)
	ctx := context.Background()
	src := &stubMonths{pages: map[string]string{"200501": "<html>該当なし</html>", "200502": tradersSample}}
	var results []MonthResult
	res, err := ImportMonths(ctx, store, []string{"200501", "200502"}, false, 0, src.fetch,
		func(r MonthResult) { results = append(results, r) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 2 || res.Rows != 3 || len(results) != 2 || results[0].ParseErr == nil || results[1].Added != 3 {
		t.Fatalf("res = %+v results = %+v", res, results)
	}
	done, _ := store.ImportedMonths(ctx)
	if done["200501"] || !done["200502"] {
		t.Fatalf("取り込み済み = %v", done)
	}
	// force なら済んだ月も取り直す
	src.calls = nil
	if _, err := ImportMonths(ctx, store, []string{"200502"}, true, 0, src.fetch, nil); err != nil || len(src.calls) != 1 {
		t.Fatalf("force で取り直さない: %v %v", src.calls, err)
	}
}
