package news

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "news.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func items() []broker.NewsItem {
	return []broker.NewsItem{
		{ID: "1", Time: "0900", Genres: []string{"62199", "3001"}, Codes: []string{"7203", "9432"},
			Headline: "公開買付けの開始に関するお知らせ", Body: "１株あたり1550円"},
		{ID: "2", Time: "1530", Genres: []string{"6526"}, Codes: []string{"7203"},
			Headline: "業績予想の修正", Body: "上方修正"},
	}
}

func TestSaveAndCoverage(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 14, 6, 30, 0, 0, time.UTC)

	n, err := s.Save(ctx, "2026-09-11", items(), at)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("初出の件数 = %d, want 2", n)
	}
	days, count, first, last, err := s.Coverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if days != 1 || count != 2 || first != "2026-09-11" || last != "2026-09-11" {
		t.Fatalf("溜まり具合 = %d 日 %d 件 %s〜%s", days, count, first, last)
	}
}

// 同じ日を取り直しても増えない。本文の訂正は後勝ちで反映し、初出時刻は動かさない。
func TestSaveIsIdempotent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	first := time.Date(2026, 9, 14, 6, 30, 0, 0, time.UTC)
	if _, err := s.Save(ctx, "2026-09-11", items(), first); err != nil {
		t.Fatal(err)
	}

	fixed := items()
	fixed[0].Body = "（訂正）１株あたり1600円"
	later := first.Add(3 * time.Hour)
	n, err := s.Save(ctx, "2026-09-11", fixed, later)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("2 回目の初出件数 = %d, want 0", n)
	}

	var body, savedAt string
	if err := s.DB().QueryRow(
		`SELECT body, saved_at FROM news WHERE feed_date = '2026-09-11' AND news_id = '1'`).
		Scan(&body, &savedAt); err != nil {
		t.Fatal(err)
	}
	if body != "（訂正）１株あたり1600円" {
		t.Fatalf("本文が訂正で上書きされていない: %q", body)
	}
	if savedAt != first.Format(time.RFC3339) {
		t.Fatalf("初出時刻が動いた: %q", savedAt)
	}

	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM news WHERE feed_date = '2026-09-11'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("記事が増えた: %d 行", rows)
	}
}

func TestCodesAndGenresAreIndexed(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "2026-09-11", items(), time.Now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM news_codes WHERE code = '7203'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("7203 の記事数 = %d, want 2", n)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM news_genres WHERE genre = '62199'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("62199 の記事数 = %d, want 1", n)
	}
}

// 取れなかった日は「1 件も無かった日」と区別できる形で残る。
func TestRecordFailure(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.RecordFailure(ctx, "2026-09-12", time.Now(), "timeout"); err != nil {
		t.Fatal(err)
	}
	done, err := s.DoneDays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if done["2026-09-12"] {
		t.Fatal("失敗した日が済みになっている")
	}
	var status string
	if err := s.DB().QueryRow(
		`SELECT status FROM news_days WHERE feed_date = '2026-09-12'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "timeout" {
		t.Fatalf("status = %q", status)
	}
}

// 失敗した日を取り直せたら済みになる。
func TestFailureThenSuccess(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.RecordFailure(ctx, "2026-09-12", time.Now(), "timeout"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "2026-09-12", items(), time.Now()); err != nil {
		t.Fatal(err)
	}
	done, err := s.DoneDays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !done["2026-09-12"] {
		t.Fatal("取り直したのに済みになっていない")
	}
}
