package archive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
)

// epochDay は Parquet の DATE 論理型（1970-01-01 からの日数）に変換する。
func epochDay(iso string) (int32, bool) {
	t, err := time.Parse(dateLayout, iso)
	if err != nil {
		return 0, false
	}
	return int32(t.Unix() / 86400), true
}

// fromEpochDay は DATE 論理型を "YYYY-MM-DD" に戻す。
func fromEpochDay(days int32) string {
	return time.Unix(int64(days)*86400, 0).UTC().Format(dateLayout)
}

// buildSchema は列名から Parquet のスキーマを組み立てる。
// 値はすべて省略可能（NULL あり）で、日付列だけ DATE、他は UTF8 文字列。
// parquet-go の Group は列名の昇順に並ぶので、書き出しの列順もそれに従う。
func buildSchema(columns []string, dateColumns map[string]bool) (*parquet.Schema, []string) {
	sorted := append([]string(nil), columns...)
	sort.Strings(sorted)
	group := parquet.Group{}
	for _, name := range sorted {
		if dateColumns[name] {
			group[name] = parquet.Optional(parquet.Date())
			continue
		}
		group[name] = parquet.Optional(parquet.String())
	}
	return parquet.NewSchema("jquants", group), sorted
}

// writeRowBatch は 1 回の WriteRows に渡す行数。全行ぶんの []parquet.Row を
// 一度に作ると行数に比例してメモリを食うので、この単位で使い回す。
const writeRowBatch = 8192

// writeParquet は表を 1 ファイルに書き出す。呼び出し側が一時ファイルに書いて
// rename する前提なので、ここでは追記や部分更新をしない。
//
// 行は writeRowBatch ずつ書く（バッファを使い回すので、行数が増えても
// ここのメモリは一定）。
func writeParquet(path string, f *Frame, dateColumns map[string]bool) error {
	schema, order := buildSchema(f.Columns, dateColumns)
	handle, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("保管庫の Parquet を作成できません %s: %w", path, err)
	}
	writer := parquet.NewWriter(handle, schema, parquet.Compression(&parquet.Zstd))
	batch := make([]parquet.Row, 0, writeRowBatch)
	cells := make([]parquet.Value, writeRowBatch*len(order))
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := writer.WriteRows(batch); err != nil {
			return fmt.Errorf("保管庫の Parquet の書き込みに失敗しました %s: %w", path, err)
		}
		batch = batch[:0]
		return nil
	}
	for _, row := range f.Rows {
		values := cells[len(batch)*len(order):][:len(order)]
		for i, name := range order {
			values[i] = parquetValue(row[name], dateColumns[name], i)
		}
		batch = append(batch, values)
		if len(batch) == writeRowBatch {
			if err := flush(); err != nil {
				handle.Close()
				return err
			}
		}
	}
	if err := flush(); err != nil {
		handle.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		handle.Close()
		return fmt.Errorf("保管庫の Parquet の確定に失敗しました %s: %w", path, err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("保管庫の Parquet を閉じられません %s: %w", path, err)
	}
	return nil
}

// parquetValue は 1 セルを Parquet の値にする。NULL は定義レベル 0。
func parquetValue(v *string, isDate bool, column int) parquet.Value {
	if v == nil {
		return parquet.NullValue().Level(0, 0, column)
	}
	if isDate {
		days, ok := epochDay(*v)
		if !ok {
			return parquet.NullValue().Level(0, 0, column)
		}
		return parquet.Int32Value(days).Level(0, 1, column)
	}
	return parquet.ByteArrayValue([]byte(*v)).Level(0, 1, column)
}

// scanOptions は Parquet を読むときの絞り込み。ゼロ値で「全列・全行」。
//
// 保管庫は 1 端点 10 年で 1,000 万行になる。全部を Frame に載せると
// メモリが行数に比例して膨らむので、読み手が要る列と行だけをここで落とす。
type scanOptions struct {
	// columns は Frame に載せる列。nil なら全列。
	columns map[string]bool
	// dateColumn / lo / hi は日付での絞り込み（lo・hi は "YYYY-MM-DD"、空で無指定）。
	// 行グループの統計で丸ごと飛ばせるかの判定にも使う。
	dateColumn string
	lo, hi     string
	// keep は行を残すかどうか。nil なら全部残す。map を組む前に呼ばれるので、
	// 落とす行にはメモリを使わない。
	keep func(row RowView) bool
}

// RowView は読み出し中の 1 行。Frame に載る前の姿で、値を文字列に複製せずに
// 覗ける。scanOptions.keep に渡すのはこれで、判定で落とした行には
// 文字列も map も作らない（1,000 万行を舐めても捨てるぶんは無料になる）。
//
// 有効なのは keep が呼ばれている間だけ。持ち出して後で読んではいけない。
type RowView struct {
	values  []parquet.Value // 列の位置で引ける（NULL は IsNull）
	columns []string
	index   map[string]int
}

// RowViewOf は文字列から RowView を組む。保管庫を通さずに Keep の判定を
// 書いたり試したりするため。キーが無い列は NULL として扱われる。
func RowViewOf(values map[string]string) RowView {
	view := RowView{index: make(map[string]int, len(values))}
	for name, v := range values {
		view.index[name] = len(view.values)
		view.columns = append(view.columns, name)
		view.values = append(view.values, parquet.ByteArrayValue([]byte(v)))
	}
	return view
}

func (r RowView) valueOf(column string) (parquet.Value, bool) {
	i, ok := r.index[column]
	if !ok || i >= len(r.values) {
		return parquet.Value{}, false
	}
	v := r.values[i]
	if v.IsNull() {
		return v, false
	}
	return v, true
}

// Text は列の値を文字列で返す（NULL・列が無い場合は ""）。ここで複製が起きる。
func (r RowView) Text(column string) string {
	v, ok := r.valueOf(column)
	if !ok {
		return ""
	}
	return parquetText(v)
}

// Equal は列の値が want と等しいか。文字列を複製せずに比べる。
func (r RowView) Equal(column, want string) bool {
	v, ok := r.valueOf(column)
	if !ok {
		return false
	}
	if v.Kind() == parquet.Int32 {
		return parquetText(v) == want // 日付は暦日に戻さないと比べられない
	}
	return string(v.ByteArray()) == want
}

// HasPrefix は列の値が prefix で始まるか。文字列を複製せずに比べる。
func (r RowView) HasPrefix(column, prefix string) bool {
	v, ok := r.valueOf(column)
	if !ok {
		return false
	}
	if v.Kind() == parquet.Int32 {
		return strings.HasPrefix(parquetText(v), prefix)
	}
	b := v.ByteArray()
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}

// filtering は行を落とす可能性があるか（事前確保の判断に使う）。
func (o scanOptions) filtering() bool {
	return o.keep != nil || o.lo != "" || o.hi != ""
}

// openParquet はファイルを開いて、列名（葉の末尾）を返す。
func openParquet(path string) (*os.File, *parquet.File, []string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("保管庫の Parquet を開けません %s: %w", path, err)
	}
	info, err := handle.Stat()
	if err != nil {
		handle.Close()
		return nil, nil, nil, fmt.Errorf("保管庫の Parquet の大きさを取れません %s: %w", path, err)
	}
	file, err := parquet.OpenFile(handle, info.Size())
	if err != nil {
		handle.Close()
		return nil, nil, nil, fmt.Errorf("保管庫の Parquet を解釈できません %s: %w", path, err)
	}
	leaves := file.Schema().Columns()
	columns := make([]string, 0, len(leaves))
	for _, leaf := range leaves {
		columns = append(columns, leaf[len(leaf)-1])
	}
	return handle, file, columns, nil
}

// readParquet は 1 ファイルを表として読む。日付列は物理型（INT32）で判別するので、
// 端点の定義が後から変わっても保存済みのファイルは読める。
func readParquet(path string) (*Frame, error) {
	return scanParquet(path, scanOptions{})
}

// scanParquet は絞り込みを通った行だけを載せた表を返す。
//
// 行は読みながら判定して落とすので、ファイルが大きくても常駐するのは
// 「残った行」ぶんだけ。射影で除いた列は文字列に起こさない（複製しないので
// ページのバッファも引きずらない）。
func scanParquet(path string, opt scanOptions) (*Frame, error) {
	handle, file, columns, err := openParquet(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	kept := columns
	if opt.columns != nil {
		kept = make([]string, 0, len(opt.columns))
		for _, name := range columns {
			if opt.columns[name] {
				kept = append(kept, name)
			}
		}
	}
	out := &Frame{Columns: kept}
	if !opt.filtering() {
		// 絞り込みが無いなら行数が分かっているので一度で確保する
		// （append の倍々の伸びで一時的に 2 倍になるのを防ぐ）
		out.Rows = make([]map[string]*string, 0, file.NumRows())
	}
	dateLeaf := -1
	if opt.dateColumn != "" {
		for i, name := range columns {
			if name == opt.dateColumn {
				dateLeaf = i
				break
			}
		}
	}
	// 列の位置で引けるようにしておく（行ごとに作らず使い回す）
	position := make(map[string]int, len(columns))
	for i, name := range columns {
		position[name] = i
	}
	scratch := make([]parquet.Value, len(columns))
	view := RowView{values: scratch, columns: columns, index: position}

	buffer := make([]parquet.Row, 256)
	for _, group := range file.RowGroups() {
		if dateLeaf >= 0 && skipRowGroup(group, dateLeaf, opt.lo, opt.hi) {
			continue
		}
		reader := group.Rows()
		for {
			n, readErr := reader.ReadRows(buffer)
			for i := 0; i < n; i++ {
				// 判定は複製せずに済ませ、残す行だけ map に起こす
				spread(buffer[i], scratch)
				if !opt.inRange(view) {
					continue
				}
				if opt.keep != nil && !opt.keep(view) {
					continue
				}
				out.Rows = append(out.Rows, decodeRow(buffer[i], columns, opt.columns))
			}
			if readErr != nil {
				reader.Close()
				if isEOF(readErr) {
					break
				}
				return nil, fmt.Errorf("保管庫の Parquet の読み取りに失敗しました %s: %w", path, readErr)
			}
			if n == 0 {
				reader.Close()
				break
			}
		}
	}
	return out, nil
}

// inRange は行が日付の範囲に入っているか。日付列が無い・欠測の行は落とす
// （どの日のものか決まらないので、範囲を指定した読み手には渡せない）。
func (o scanOptions) inRange(row RowView) bool {
	if o.lo == "" && o.hi == "" {
		return true
	}
	v, ok := row.valueOf(o.dateColumn)
	if !ok {
		return false
	}
	day := parquetText(v)
	if o.lo != "" && day < o.lo {
		return false
	}
	if o.hi != "" && day > o.hi {
		return false
	}
	return true
}

// skipRowGroup は行グループを丸ごと飛ばせるか、日付列の統計で判定する。
// 統計が無ければ飛ばさない（読んで行ごとに判定する）。
func skipRowGroup(group parquet.RowGroup, leaf int, lo, hi string) bool {
	if lo == "" && hi == "" {
		return false
	}
	chunks := group.ColumnChunks()
	if leaf >= len(chunks) {
		return false
	}
	index, err := chunks[leaf].ColumnIndex()
	if err != nil || index == nil || index.NumPages() == 0 {
		return false
	}
	low, high := "", ""
	for p := 0; p < index.NumPages(); p++ {
		if index.NullPage(p) {
			continue
		}
		mn, mx := index.MinValue(p), index.MaxValue(p)
		if mn.IsNull() || mx.IsNull() {
			return false
		}
		a, b := fromEpochDay(mn.Int32()), fromEpochDay(mx.Int32())
		if low == "" || a < low {
			low = a
		}
		if high == "" || b > high {
			high = b
		}
	}
	if low == "" || high == "" {
		return false
	}
	return (hi != "" && low > hi) || (lo != "" && high < lo)
}

// distinctColumn は 1 列の異なる値だけを読む。行を組み立てないので、
// 10 年ぶん（1,000 万行）でも常駐は値の種類ぶん（日付なら 2,500 個）で済む。
//
// ページの統計で min == max なら、そのページは 1 種類しか入っていないので
// 値を読まずに済ませる。保管庫は日付順に並べて書くので、日付列ではこれがほぼ毎回効く。
func distinctColumn(path, column string) (map[string]bool, error) {
	handle, file, columns, err := openParquet(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	leaf := -1
	for i, name := range columns {
		if name == column {
			leaf = i
			break
		}
	}
	if leaf < 0 {
		return nil, nil
	}
	out := map[string]bool{}
	buffer := make([]parquet.Value, 1024)
	for _, group := range file.RowGroups() {
		chunks := group.ColumnChunks()
		if leaf >= len(chunks) {
			continue
		}
		pages := chunks[leaf].Pages()
		for {
			page, err := pages.ReadPage()
			if err != nil {
				pages.Close()
				if isEOF(err) {
					break
				}
				return nil, fmt.Errorf("保管庫の Parquet の列を読めません %s (%s): %w", path, column, err)
			}
			if mn, mx, ok := page.Bounds(); ok && !mn.IsNull() && parquetText(mn) == parquetText(mx) {
				out[parquetText(mn)] = true
				continue
			}
			values := page.Values()
			for {
				n, err := values.ReadValues(buffer)
				for i := 0; i < n; i++ {
					if buffer[i].IsNull() {
						continue
					}
					out[parquetText(buffer[i])] = true
				}
				if err != nil {
					if isEOF(err) {
						break
					}
					pages.Close()
					return nil, fmt.Errorf("保管庫の Parquet の列を読めません %s (%s): %w", path, column, err)
				}
				if n == 0 {
					break
				}
			}
		}
	}
	return out, nil
}

// parquetText は 1 つの値を保存形の文字列にする（日付は INT32 なので暦日に戻す）。
func parquetText(v parquet.Value) string {
	if v.IsNull() {
		return ""
	}
	if v.Kind() == parquet.Int32 {
		return fromEpochDay(v.Int32())
	}
	return string(v.ByteArray())
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

// spread は 1 行の値を列の位置で引ける並びに移す。使い回すバッファに書くので
// アロケートしない。行に無い列は NULL のままになる。
func spread(row parquet.Row, into []parquet.Value) {
	for i := range into {
		into[i] = parquet.Value{}
	}
	for _, v := range row {
		i := v.Column()
		if i >= 0 && i < len(into) {
			into[i] = v
		}
	}
}

// decodeRow は Parquet の 1 行を文字列の map に戻す。
// want が nil でなければ、その列だけを起こす（除いた列は文字列に複製しない）。
func decodeRow(row parquet.Row, columns []string, want map[string]bool) map[string]*string {
	size := len(columns)
	if want != nil {
		size = len(want)
	}
	out := make(map[string]*string, size)
	for _, v := range row {
		i := v.Column()
		if i < 0 || i >= len(columns) {
			continue
		}
		name := columns[i]
		if want != nil && !want[name] {
			continue
		}
		if v.IsNull() {
			out[name] = nil
			continue
		}
		switch v.Kind() {
		case parquet.Int32:
			out[name] = strptr(fromEpochDay(v.Int32()))
		default:
			out[name] = strptr(string(v.ByteArray()))
		}
	}
	return out
}
