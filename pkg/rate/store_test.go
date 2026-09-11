package rate

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveKeepsFirstSeen(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "rate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	first := time.Date(2026, 9, 11, 7, 5, 0, 0, jst)
	entries, err := Parse(sample, first)
	if err != nil {
		t.Fatal(err)
	}

	added, err := store.Save(ctx, entries, first)
	if err != nil {
		t.Fatal(err)
	}
	if added != len(entries) {
		t.Fatalf("初回の新規 = %d, 欲しいのは %d", added, len(entries))
	}

	// 同じ内容をもう一度見ても新規にはならず、初出時刻も動かない
	second := first.Add(10 * time.Minute)
	added, err = store.Save(ctx, entries, second)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatalf("2 回目の新規 = %d, 欲しいのは 0", added)
	}

	var got, last string
	err = store.DB().QueryRow(`SELECT first_seen_at, last_seen_at FROM ratings WHERE code = '4021'`).Scan(&got, &last)
	if err != nil {
		t.Fatal(err)
	}
	if got != first.Format(time.RFC3339) {
		t.Errorf("初出時刻 = %s, 欲しいのは %s", got, first.Format(time.RFC3339))
	}
	if last != second.Format(time.RFC3339) {
		t.Errorf("最終確認時刻 = %s, 欲しいのは %s", last, second.Format(time.RFC3339))
	}
}
