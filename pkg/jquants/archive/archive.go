package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Archive は data/jquants/ の読み書き。1 端点 = 1 ディレクトリ、1 月 = 1 ファイル。
type Archive struct {
	Root string
}

// NewArchive は保管庫を開く（ディレクトリは書き込み時に作る）。
func NewArchive(root string) *Archive { return &Archive{Root: root} }

// LedgerPath は台帳（SQLite）の場所。
func (a *Archive) LedgerPath() string { return filepath.Join(a.Root, "ledger.db") }

// RawDir は一括ダウンロードの CSV を残す場所
// （変換にバグがあっても取り直さずに済む保険）。
func (a *Archive) RawDir(ep Endpoint) string {
	return filepath.Join(a.Root, "_raw", ep.Name())
}

// Directory は端点のディレクトリ。
func (a *Archive) Directory(ep Endpoint) string {
	return filepath.Join(a.Root, ep.Name())
}

// PathFor は端点 × 月（YYYY-MM）の Parquet の場所。
func (a *Archive) PathFor(ep Endpoint, month string) string {
	return filepath.Join(a.Directory(ep), month+".parquet")
}

// Months は保存済みの月（YYYY-MM）。Parquet の実体を走査する。
func (a *Archive) Months(ep Endpoint) []string {
	entries, err := os.ReadDir(a.Directory(ep))
	if err != nil {
		return nil
	}
	var months []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".parquet") {
			continue
		}
		months = append(months, strings.TrimSuffix(e.Name(), ".parquet"))
	}
	sort.Strings(months)
	return months
}

// Scan は端点の全期間を読む。無ければ空。
// 列は和集合で合わせる（仕様変更で列が増えても古い月と一緒に読める）。
func (a *Archive) Scan(ep Endpoint) (*Frame, error) {
	return a.Read(ep, time.Time{}, time.Time{})
}

// Read は期間で絞って読む。月ファイル単位で必要なものだけ開く。
// start / end はゼロ値で無指定。
func (a *Archive) Read(ep Endpoint, start, end time.Time) (*Frame, error) {
	months := a.Months(ep)
	var wanted []string
	for _, m := range months {
		if !start.IsZero() && m < start.Format("2006-01") {
			continue
		}
		if !end.IsZero() && m > end.Format("2006-01") {
			continue
		}
		wanted = append(wanted, m)
	}
	frames := make([]*Frame, 0, len(wanted))
	for _, m := range wanted {
		f, err := readParquet(a.PathFor(ep, m))
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	}
	out := concatDiagonal(frames...)
	if start.IsZero() && end.IsZero() {
		return out, nil
	}
	// 月の粒度で絞ったあと、日付列で厳密に絞る
	lo, hi := start.Format(dateLayout), end.Format(dateLayout)
	kept := make([]map[string]*string, 0, out.Height())
	for _, row := range out.Rows {
		v := row[ep.DateColumn]
		if v == nil {
			continue
		}
		if !start.IsZero() && *v < lo {
			continue
		}
		if !end.IsZero() && *v > hi {
			continue
		}
		kept = append(kept, row)
	}
	out.Rows = kept
	return out, nil
}

// Dates は保存済みの日付（DateColumn の値）を昇順で返す。
func (a *Archive) Dates(ep Endpoint) ([]time.Time, error) {
	f, err := a.Scan(ep)
	if err != nil {
		return nil, err
	}
	if !f.HasColumn(ep.DateColumn) {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []time.Time
	for _, row := range f.Rows {
		v := row[ep.DateColumn]
		if v == nil || seen[*v] {
			continue
		}
		seen[*v] = true
		parsed, err := time.Parse(dateLayout, *v)
		if err != nil {
			continue
		}
		out = append(out, parsed)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

// Upsert は鍵で後勝ちに合流する。返り値は増えた・変わった行数。
//
// 月ごとに: 既存を読む → 新しい行を後ろに足す → 鍵で最後を残す → 一時ファイルに
// 書いて rename。途中で落ちても壊れたファイルは残らない。
//
// 端点ごとにファイルロックを取る。jquants sync（cron）と accum sync
// （足の取得の書き戻し）は別プロセスで、同じ月ファイルを同時に読んで
// 書き戻すと後から rename した側が先の更新を握り潰すため。
func (a *Archive) Upsert(ep Endpoint, f *Frame) (int, error) {
	if f.Height() == 0 {
		return 0, nil
	}
	var missing []string
	for _, k := range ep.Key {
		if !f.HasColumn(k) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%s の行に鍵の列がありません: %v", ep.Path, missing)
	}
	// 月ごとに分ける。日付が取れない行は落とす（どの月に置くか決まらない）
	byMonth := map[string]*Frame{}
	var order []string
	for _, row := range f.Rows {
		v := row[ep.DateColumn]
		if v == nil || len(*v) < 7 {
			continue
		}
		month := (*v)[:7]
		part, ok := byMonth[month]
		if !ok {
			part = &Frame{Columns: append([]string(nil), f.Columns...)}
			byMonth[month] = part
			order = append(order, month)
		}
		part.Rows = append(part.Rows, row)
	}
	unlock, err := a.lock(ep)
	if err != nil {
		return 0, err
	}
	defer unlock()
	changed := 0
	for _, month := range order {
		n, err := a.upsertMonth(ep, month, byMonth[month])
		if err != nil {
			return changed, err
		}
		changed += n
	}
	return changed, nil
}

// lock は端点ディレクトリの排他ロック（プロセス間）を取る。
func (a *Archive) lock(ep Endpoint) (func(), error) {
	directory := a.Directory(ep)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("端点のディレクトリを作れません %s: %w", directory, err)
	}
	handle, err := os.OpenFile(filepath.Join(directory, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("ロックを開けません %s: %w", directory, err)
	}
	if err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX); err != nil {
		handle.Close()
		return nil, fmt.Errorf("ロックを取れません %s: %w", directory, err)
	}
	return func() {
		_ = syscall.Flock(int(handle.Fd()), syscall.LOCK_UN)
		handle.Close()
	}, nil
}

func (a *Archive) upsertMonth(ep Endpoint, month string, new *Frame) (int, error) {
	path := a.PathFor(ep, month)
	new = dedupeLast(new, ep.Key)
	var merged *Frame
	changed := 0
	if _, err := os.Stat(path); err == nil {
		old, err := readParquet(path)
		if err != nil {
			return 0, err
		}
		merged = concatDiagonal(old, new)
		changed = countChanged(old, new, ep.Key)
	} else {
		merged = new
		changed = new.Height()
	}
	merged = dedupeLast(merged, ep.Key)
	sortByKey(merged, ep.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("保存先を作れません %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := writeParquet(tmp, merged, ep.dateColumnSet()); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, fmt.Errorf("Parquet の差し替えに失敗しました %s: %w", path, err)
	}
	return changed, nil
}

// ExistingParquetDirs は Parquet を 1 つ以上持つ端点ディレクトリ名。
// DuckDB のビュー登録（query コマンド）に使う。
func (a *Archive) ExistingParquetDirs() []string {
	entries, err := os.ReadDir(a.Root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		if hasParquet(filepath.Join(a.Root, e.Name())) {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs
}

func hasParquet(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".parquet") {
			return true
		}
	}
	return false
}
