package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// readWorkers は月ファイルを同時に読む数。常駐メモリは「絞り込みを通った行」
// ＋「同時に開いている月ぶんのページのバッファ」なので、上限を決めておく。
var readWorkers = min(4, runtime.GOMAXPROCS(0))

// bytesPerCell は Frame の 1 セルが実際に食うメモリの目安。
// 行は map[string]*string なので、map の枠・文字列のヘッダ・値の実体で
// 1 セルおよそ 96 バイト（このリポジトリで 15 列と 30 列を実測した値から）。
const bytesPerCell = 96

// defaultReadBudget は 1 回の読み出しが Frame に載せていい量の既定。
// これを超えると、OOM で殺される代わりにエラーで止まる。
const defaultReadBudget = 2 << 30 // 2 GiB

// ReadBudget は 1 回の読み出しの上限（バイト）。0 以下で無制限。
// JQUANTS_READ_BUDGET_MB で上書きできる（サーバーの実メモリに合わせるため）。
var ReadBudget = readBudgetFromEnv()

func readBudgetFromEnv() int64 {
	raw := os.Getenv("JQUANTS_READ_BUDGET_MB")
	if raw == "" {
		return defaultReadBudget
	}
	mb, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return defaultReadBudget
	}
	return mb << 20
}

// budgetError は読み出しが上限を超えたときのエラー。何を絞れば通るかを書く。
func budgetError(ep Endpoint, cells int64) error {
	return fmt.Errorf(
		"%s の読み出しが上限 %d MB を超えました（約 %d MB）。"+
			"期間を狭めるか ReadOptions の Columns / Keep で絞ってください"+
			"（上限は JQUANTS_READ_BUDGET_MB で変えられます）",
		ep.Path, ReadBudget>>20, (cells*bytesPerCell)>>20)
}

// ReadOptions は読み出しの絞り込み。ゼロ値で「全期間・全列・全行」。
//
// 保管庫は 1 端点 10 年で 1,000 万行あり、全部を Frame に載せると 15GB を超える。
// 要る列と行はここで指定して、読みながら落とす。
type ReadOptions struct {
	// Start / End は日付の範囲（ゼロ値で無指定）。月ファイルの取捨と、
	// 行グループの統計での刈り込みに使う。
	Start, End time.Time
	// Columns は Frame に載せる列。nil なら全列。Keep が見る列は必ず入れる。
	Columns []string
	// Keep は行を残すかどうか。nil なら全部残す。Frame に載る前の行に対して
	// 呼ばれるので、落とした行にはメモリを使わない。判定に使う列は
	// Columns に入れなくてもよい（保存されている列なら覗ける）。
	Keep func(row RowView) bool
}

// Scan は端点の全期間を読む。無ければ空。
// 列は和集合で合わせる（仕様変更で列が増えても古い月と一緒に読める）。
//
// 全行をメモリに載せるので、行数の多い端点（bars・master は 10 年で 1,000 万行）
// には使わない。日付だけなら Dates、一部でよければ ReadWhere を使う。
func (a *Archive) Scan(ep Endpoint) (*Frame, error) {
	return a.Read(ep, time.Time{}, time.Time{})
}

// Read は期間で絞って読む。月ファイル単位で必要なものだけ開く。
// start / end はゼロ値で無指定。
func (a *Archive) Read(ep Endpoint, start, end time.Time) (*Frame, error) {
	return a.ReadWhere(ep, ReadOptions{Start: start, End: end})
}

// ReadWhere は絞り込みを押し下げて読む。月ファイルを並行に読み、
// 絞り込みを通らなかった行はその場で捨てる（Frame には載せない）。
func (a *Archive) ReadWhere(ep Endpoint, opt ReadOptions) (*Frame, error) {
	wanted := a.monthsIn(ep, opt.Start, opt.End)
	scan := scanOptions{dateColumn: ep.DateColumn, keep: opt.Keep}
	if !opt.Start.IsZero() {
		scan.lo = opt.Start.Format(dateLayout)
	}
	if !opt.End.IsZero() {
		scan.hi = opt.End.Format(dateLayout)
	}
	if len(opt.Columns) > 0 {
		scan.columns = make(map[string]bool, len(opt.Columns)+1)
		for _, name := range opt.Columns {
			scan.columns[name] = true
		}
		// 日付列は範囲の判定と月の切り分けに要るので、黙って足す
		for _, name := range ep.DateColumns() {
			scan.columns[name] = true
		}
	}
	frames := make([]*Frame, len(wanted))
	var cells atomic.Int64
	if err := eachMonth(wanted, func(i int, month string) error {
		f, err := scanParquet(a.PathFor(ep, month), scan)
		if err != nil {
			return err
		}
		frames[i] = f
		// 月をまたいだ合計で上限を見る。並行に読んでいるので加算は atomic
		if ReadBudget > 0 {
			total := cells.Add(int64(f.Height()) * int64(len(f.Columns)))
			if total*bytesPerCell > ReadBudget {
				return budgetError(ep, total)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return concatDiagonal(frames...), nil
}

// monthsIn は期間に重なる月ファイル（YYYY-MM）を昇順で返す。
func (a *Archive) monthsIn(ep Endpoint, start, end time.Time) []string {
	var wanted []string
	for _, m := range a.Months(ep) {
		if !start.IsZero() && m < start.Format("2006-01") {
			continue
		}
		if !end.IsZero() && m > end.Format("2006-01") {
			continue
		}
		wanted = append(wanted, m)
	}
	return wanted
}

// eachMonth は月ファイルを readWorkers 本で並行に処理する。
// 1 つでも失敗したら最初のエラーを返す（部分的な結果は返さない）。
func eachMonth(months []string, fn func(i int, month string) error) error {
	if len(months) == 0 {
		return nil
	}
	if len(months) == 1 || readWorkers <= 1 {
		for i, m := range months {
			if err := fn(i, m); err != nil {
				return err
			}
		}
		return nil
	}
	// 次に処理する月は共有の添字で配る。生成役の goroutine を置くと、
	// 失敗で受け手が抜けたときに送信で永久にブロックして残る
	var cursor atomic.Int64
	var wg sync.WaitGroup
	var once sync.Once
	var failure error
	for w := 0; w < readWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(cursor.Add(1)) - 1
				if i >= len(months) {
					return
				}
				if err := fn(i, months[i]); err != nil {
					once.Do(func() { failure = err })
					return
				}
			}
		}()
	}
	wg.Wait()
	return failure
}

// Dates は保存済みの日付（DateColumn の値）を昇順で返す。
//
// 行は組み立てず、日付列だけを読む。10 年（1,000 万行）でも常駐は
// 日付の種類ぶん（2,500 個）で済む。
func (a *Archive) Dates(ep Endpoint) ([]time.Time, error) {
	months := a.Months(ep)
	found := make([]map[string]bool, len(months))
	if err := eachMonth(months, func(i int, month string) error {
		values, err := distinctColumn(a.PathFor(ep, month), ep.DateColumn)
		if err != nil {
			return err
		}
		found[i] = values
		return nil
	}); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []time.Time
	for _, values := range found {
		for v := range values {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			parsed, err := time.Parse(dateLayout, v)
			if err != nil {
				continue
			}
			out = append(out, parsed)
		}
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
		return 0, fmt.Errorf("保管庫の Parquet の差し替えに失敗しました %s: %w", path, err)
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
