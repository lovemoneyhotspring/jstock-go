package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLogAt は中身と最終更新日時を指定してログを作る。
func writeLogAt(t *testing.T, path, content string, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

// 日付が変わっていれば退避する。退避名の日付は**中身が書かれた日**。
func TestRotateLogMovesPreviousDay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wbjp-prod.jsonl")
	yesterday := time.Date(2026, 9, 2, 14, 30, 0, 0, time.UTC) // JST 9/2 23:30
	writeLogAt(t, path, "{\"msg\":\"きのう\"}\n", yesterday)

	now := time.Date(2026, 9, 2, 15, 5, 0, 0, time.UTC) // JST 9/3 0:05
	if err := rotateLog(path, now, RetainDays); err != nil {
		t.Fatalf("退避に失敗: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("元のファイルが残っています（退避されていない）")
	}
	rotated := path + ".2026-09-02"
	body, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatalf("退避先がありません: %v", err)
	}
	if string(body) != "{\"msg\":\"きのう\"}\n" {
		t.Errorf("退避で中身が変わりました: %q", body)
	}
}

// 同じ日のうちは退避しない（1 日に何回 cron が走っても 1 ファイル）。
func TestRotateLogKeepsSameDay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wbjp-prod.jsonl")
	morning := time.Date(2026, 9, 3, 1, 0, 0, 0, time.UTC)
	writeLogAt(t, path, "{}\n", morning)

	now := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	if err := rotateLog(path, now, RetainDays); err != nil {
		t.Fatalf("退避に失敗: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("同じ日なのに退避されました")
	}
}

// 境界は JST の 0 時。JST 8:59:45 の open（UTC では前日の 23:59:45）は、9:00 を過ぎても
// 同じ日のファイルに残る（UTC で切ると前日の退避に入り、朝の点検から見えなかった）。
func TestRotateLogBoundaryIsJST(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daytrade-prod.jsonl")
	preopen := time.Date(2026, 9, 17, 23, 59, 45, 0, time.UTC) // JST 9/18 8:59:45
	writeLogAt(t, path, "{}\n", preopen)

	if err := rotateLog(path, time.Date(2026, 9, 18, 0, 0, 3, 0, time.UTC), RetainDays); err != nil {
		t.Fatalf("退避に失敗: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("JST では同じ日（9/18）なのに退避されました")
	}

	// JST の日付が変わった最初の回（9/19 6:00 JST = 9/18 21:00 UTC）で、JST の日付の名前で退避する
	if err := rotateLog(path, time.Date(2026, 9, 18, 21, 0, 0, 0, time.UTC), RetainDays); err != nil {
		t.Fatalf("退避に失敗: %v", err)
	}
	if _, err := os.Stat(path + ".2026-09-18"); err != nil {
		t.Errorf("JST の日付（2026-09-18）で退避されていません: %v", err)
	}
}

// ファイルが無い・空のときは何もしない（初回起動）。
func TestRotateLogNoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wbjp-prod.jsonl")
	if err := rotateLog(path, time.Now(), RetainDays); err != nil {
		t.Errorf("ファイルが無いのにエラー: %v", err)
	}

	writeLogAt(t, path, "", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err := rotateLog(path, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), RetainDays); err != nil {
		t.Errorf("空ファイルでエラー: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("空ファイルが退避されました（退避する中身が無い）")
	}
}

// 保持日数を過ぎた退避は消す。境界の日は残す。
func TestPruneRotatedRemovesOldOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wbjp-prod.jsonl")
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	keep := path + "." + now.AddDate(0, 0, -RetainDays).Format(rotatedSuffix)      // ちょうど 90 日前
	fresh := path + "." + now.AddDate(0, 0, -1).Format(rotatedSuffix)              // 昨日
	stale := path + "." + now.AddDate(0, 0, -(RetainDays+1)).Format(rotatedSuffix) // 91 日前
	other := filepath.Join(dir, "wbjp-prod.jsonl.メモ")                              // 日付でないもの
	for _, name := range []string{keep, fresh, stale, other} {
		if err := os.WriteFile(name, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := pruneRotated(path, now, RetainDays); err != nil {
		t.Fatalf("削除に失敗: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("保持日数を過ぎた退避が残っています")
	}
	for _, name := range []string{keep, fresh, other} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("消してはいけないファイルが消えました: %s", filepath.Base(name))
		}
	}
}

// ロガーを作ると退避が起きる（配線されていること）。
func TestNewLoggerRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wbjp-prod.jsonl")
	writeLogAt(t, path, "{\"msg\":\"むかし\"}\n",
		time.Now().UTC().AddDate(0, 0, -2))

	logger, err := NewLogger("wbjp", "prod", "run-1", "test", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	logger.Info("test", "きょう")

	matches, _ := filepath.Glob(path + ".*")
	rotated := 0
	for _, m := range matches {
		if _, err := time.Parse(rotatedSuffix, filepath.Ext(m)[1:]); err == nil {
			rotated++
		}
	}
	if rotated != 1 {
		t.Fatalf("退避ファイルが %d 個（1 個のはず）: %v", rotated, matches)
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 {
		t.Fatalf("新しいログが書けていません: %v", err)
	}
	if string(body) == "{\"msg\":\"むかし\"}\n" {
		t.Error("古い中身が残っています")
	}
}
