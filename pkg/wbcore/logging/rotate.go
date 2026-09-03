package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ログの日次ローテーション。
//
// Python 版は TimedRotatingFileHandler(when="midnight", utc=True, backupCount=90)
// を使っていた。Go には相当するものが標準に無く、移植で落ちていたため、
// 同じ約束をここで実装する。落ちていた間、state/logs の 1 ファイルが無限に
// 増え続けていた（1 日 数 MB）。
//
// 退避したファイルは `<name>.jsonl.YYYY-MM-DD`。日付は**その中身が書かれた日**
// （UTC）で、退避した日ではない。日付ごとに分かれるので、ある日のログを読むのに
// 他の日を読まずに済む。

// RetainDays は退避したログを残す日数。
const RetainDays = 90

// rotatedSuffix は退避ファイルの日付部分の書式。
const rotatedSuffix = "2006-01-02"

// rotateLog は日付が変わっていれば現在のログを退避し、古い退避を消す。
//
// cron から複数のプロセスが同じファイルに書くので、ロックを取ってから
// もう一度確かめる（先に別のプロセスが退避していれば何もしない）。
// 退避に失敗してもログ出力自体は続けたいので、呼び出し側は警告に留める。
func rotateLog(path string, now time.Time, retainDays int) error {
	unlock, err := lockForRotate(path)
	if err != nil {
		return err
	}
	defer unlock()

	// ロックを取ったあとに確かめる。ここまでに別のプロセスが退避していれば
	// ファイルは消えている（次の OpenFile が作り直す）
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pruneRotated(path, now, retainDays)
		}
		return err
	}
	if info.Size() == 0 {
		return pruneRotated(path, now, retainDays)
	}

	// 中身が書かれた日（UTC）。今日と同じならまだ退避しない
	day := info.ModTime().UTC().Format(rotatedSuffix)
	if day == now.UTC().Format(rotatedSuffix) {
		return pruneRotated(path, now, retainDays)
	}

	target := path + "." + day
	if _, err := os.Stat(target); err == nil {
		// 同じ日の退避が既にある（時計が巻き戻った等）。上書きして消さない
		return fmt.Errorf("退避先が既にあります: %s", target)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("ログを退避できません（%s → %s）: %w", path, target, err)
	}
	return pruneRotated(path, now, retainDays)
}

// pruneRotated は保持日数を過ぎた退避ファイルを消す。
func pruneRotated(path string, now time.Time, retainDays int) error {
	if retainDays <= 0 {
		return nil
	}
	dir := filepath.Dir(path)
	prefix := filepath.Base(path) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	cutoff := now.UTC().AddDate(0, 0, -retainDays).Format(rotatedSuffix)
	var stale []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		day := strings.TrimPrefix(name, prefix)
		// 日付として読めないものは触らない（人が置いたファイルかもしれない）
		if _, err := time.Parse(rotatedSuffix, day); err != nil {
			continue
		}
		if day < cutoff {
			stale = append(stale, filepath.Join(dir, name))
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// lockForRotate は退避のあいだだけ排他を取る。
//
// ロックファイルは残るが中身は空。運用者が `ls state/logs` でログを見るときに
// 邪魔にならないよう、先頭にドットを付けて隠しファイルにする。
func lockForRotate(path string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".rotate.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
