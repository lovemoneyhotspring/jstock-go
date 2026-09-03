package archive

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// dateLayout は保存する日付の表記。
const dateLayout = "2006-01-02"

// Frame は保存形の表。値はすべて文字列（NULL は nil）で、日付列だけ
// "YYYY-MM-DD" に正規化してある（Parquet には DATE 型で書く）。
//
// polars の DataFrame の代わり。列の型を持たないのは方針どおり
// 「生のまま残す」ため。数値への解釈は読むとき（Typed）に行う。
//
// 行は Columns と同じ並びのスライス（Row）。以前は行ごとの map だったが、
// map は 1 セルあたり約 100 バイト（枠・ハッシュ・ポインタ）を食い、1 か月 80 万行の
// 日足で 300MB になっていた。スライスなら 1 セル 24 バイト＋値で済む。
// 列名で引くときは Get / col を使う（Columns の位置は index に控える）。
type Frame struct {
	Columns []string
	Rows    []Row
	// index は列名 → Columns の位置。Columns が変わったら組み直す（col が面倒を見る）。
	index map[string]int
}

// Row は 1 行。Frame.Columns と同じ並びで、NULL は nil。
// 後から列が増えた表では Columns より短いことがある（足りない分は NULL）。
type Row []*string

// col は列の位置。無ければ -1。
func (f *Frame) col(name string) int {
	if f == nil {
		return -1
	}
	if f.index == nil || len(f.index) != len(f.Columns) {
		f.index = make(map[string]int, len(f.Columns))
		for i, c := range f.Columns {
			f.index[c] = i
		}
	}
	if j, ok := f.index[name]; ok {
		return j
	}
	return -1
}

// cols は列名の並びを位置の並びにする（無い列は -1）。
func (f *Frame) cols(names []string) []int {
	out := make([]int, len(names))
	for i, name := range names {
		out[i] = f.col(name)
	}
	return out
}

// cell は行の j 列目。範囲外（短い行・無い列）は nil。
func cell(row Row, j int) *string {
	if j < 0 || j >= len(row) {
		return nil
	}
	return row[j]
}

// AppendRow は列名 → 値の行を足す。知らない列は末尾に足す。
// 取り込み以外（テストの固定データなど）で表を組むときに使う。
func (f *Frame) AppendRow(values map[string]*string) {
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		f.addColumn(k)
	}
	row := make(Row, len(f.Columns))
	for _, k := range names {
		row[f.col(k)] = values[k]
	}
	f.Rows = append(f.Rows, row)
}

// Height は行数。
func (f *Frame) Height() int {
	if f == nil {
		return 0
	}
	return len(f.Rows)
}

// HasColumn は列があるか。
func (f *Frame) HasColumn(name string) bool {
	if f == nil {
		return false
	}
	for _, c := range f.Columns {
		if c == name {
			return true
		}
	}
	return false
}

// addColumn は列を末尾に足す（既にあれば何もしない）。
func (f *Frame) addColumn(name string) {
	if f.col(name) < 0 {
		f.Columns = append(f.Columns, name)
		f.index[name] = len(f.Columns) - 1
	}
}

// Get は行 i の列 name の値。無ければ nil。
func (f *Frame) Get(i int, name string) *string {
	if f == nil || i < 0 || i >= len(f.Rows) {
		return nil
	}
	return cell(f.Rows[i], f.col(name))
}

func strptr(s string) *string { return &s }

// stringify は値を文字列にする。数値は表記を変えない（1.0 を 1 にしない）。
func stringify(v any) *string {
	switch value := v.(type) {
	case nil:
		return nil
	case string:
		return strptr(value)
	case bool:
		if value {
			return strptr("true")
		}
		return strptr("false")
	case json.Number:
		return strptr(value.String())
	case float64:
		// JSON の数値は float64 で来る。整数値は整数として書く（1.0 → 1）
		if value == float64(int64(value)) {
			return strptr(strconv.FormatInt(int64(value), 10))
		}
		return strptr(strconv.FormatFloat(value, 'g', -1, 64))
	case int:
		return strptr(strconv.Itoa(value))
	case int64:
		return strptr(strconv.FormatInt(value, 10))
	default:
		// 入れ子（EDINET の Hldrs など）は JSON 文字列で残す
		b, err := json.Marshal(value)
		if err != nil {
			return strptr(fmt.Sprint(value))
		}
		return strptr(string(b))
	}
}

// normalizeDate は日付文字列を "YYYY-MM-DD" に揃える。解釈できなければ nil
// （polars の strict=False と同じ。日付にならない値は欠測として扱う）。
func normalizeDate(raw *string) *string {
	if raw == nil {
		return nil
	}
	s := strings.TrimSpace(*raw)
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "/", "-")
	if len(s) > 10 {
		s = s[:10]
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return nil
	}
	out := t.Format(dateLayout)
	return &out
}

// withDates は日付列を正規化し、日付列が無ければエラーにする。
func withDates(f *Frame, ep Endpoint) error {
	for name := range ep.dateColumnSet() {
		j := f.col(name)
		if j < 0 {
			continue
		}
		for _, row := range f.Rows {
			if j < len(row) {
				row[j] = normalizeDate(row[j])
			}
		}
	}
	if f.Height() > 0 && !f.HasColumn(ep.DateColumn) {
		return fmt.Errorf("%s の応答に日付列 %s がありません: %v", ep.Path, ep.DateColumn, f.Columns)
	}
	return nil
}

// RowsToFrame は API の行（JSON オブジェクト）を保存形にする。
func RowsToFrame(rows []map[string]any, ep Endpoint) (*Frame, error) {
	f := &Frame{Rows: make([]Row, 0, len(rows))}
	names := make([]string, 0, 32)
	for _, row := range rows {
		// 列の出現順を安定させるため、行ごとにキーを並べてから足す
		names = names[:0]
		for k := range row {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			f.addColumn(k)
		}
		out := make(Row, len(f.Columns))
		for _, k := range names {
			out[f.col(k)] = stringify(row[k])
		}
		f.Rows = append(f.Rows, out)
	}
	if err := withDates(f, ep); err != nil {
		return nil, err
	}
	return f, nil
}

// CSVToFrame は一括ダウンロードの csv.gz を保存形にする。列名は API と同じ前提。
//
// gzip は流しながら読む（展開した全文を一度メモリに置かない。bars の 1 か月は
// 展開すると数十 MB で、Frame 本体と合わせて 2 重に持つことになる）。
func CSVToFrame(payload []byte, ep Endpoint) (*Frame, error) {
	var source io.Reader = bytes.NewReader(payload)
	if len(payload) >= 2 && payload[0] == 0x1f && payload[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("一括 CSV の展開に失敗しました: %w", err)
		}
		defer zr.Close()
		source = zr
	}
	reader := csv.NewReader(source)
	// 行のスライスを使い回す。各フィールドは行ごとの 1 本の文字列の部分文字列なので、
	// 値を Frame に残しても 1 行 1 アロケートで済む（フィールドごとに複製しない）
	reader.ReuseRecord = true
	reader.FieldsPerRecord = -1 // 列数の揺れ（末尾の空列など）で全体を落とさない
	reader.LazyQuotes = true
	header, err := reader.Read()
	if err == io.EOF {
		return &Frame{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("一括 CSV の見出しを読めません: %w", err)
	}
	// ReuseRecord なので見出しのスライスは次の Read で上書きされる。複製しておく
	header = append([]string(nil), header...)
	// BOM 付きで配られることがある
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	f := &Frame{Columns: append([]string(nil), header...)}
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("一括 CSV の読み取りに失敗しました: %w", err)
		}
		row := make(Row, len(header))
		// 値の文字列ヘッダは行ごとに 1 本にまとめる（decodeRow と同じ理由）
		values := make([]string, 0, len(header))
		for i := range header {
			if i >= len(record) || record[i] == "" {
				continue // 空欄は NULL（polars の read_csv + when/then と同じ）
			}
			values = append(values, record[i])
			row[i] = &values[len(values)-1]
		}
		f.Rows = append(f.Rows, row)
	}
	if err := withDates(f, ep); err != nil {
		return nil, err
	}
	return f, nil
}

// typedSkip は「数字だけだが数値ではない」列。Typed で数値化しない。
var typedSkip = map[string]bool{
	"Code": true, "S33": true, "S17": true, "DocId": true, "DiscNo": true, "Section": true,
}

// NumericColumns は Typed が数値として扱う列を返す。先頭 2000 行を見て、
// 欠測を除く全部が float に解釈できる列だけを選ぶ（Python の typed と同じ判定）。
func NumericColumns(f *Frame, exclude ...string) []string {
	if f.Height() == 0 {
		return nil
	}
	skip := make(map[string]bool, len(typedSkip)+len(exclude))
	for k := range typedSkip {
		skip[k] = true
	}
	for _, k := range exclude {
		skip[k] = true
	}
	limit := min(f.Height(), 2000)
	var out []string
	for _, name := range f.Columns {
		if skip[name] {
			continue
		}
		seen, ok := 0, true
		j := f.col(name)
		for i := 0; i < limit && ok; i++ {
			v := cell(f.Rows[i], j)
			if v == nil {
				continue
			}
			seen++
			if _, err := strconv.ParseFloat(strings.TrimSpace(*v), 64); err != nil {
				ok = false
			}
		}
		if ok && seen > 0 {
			out = append(out, name)
		}
	}
	return out
}

// TypedFrame は研究用に数値へ寄せた表。保存形は文字列なので、読むときにこれを通す。
type TypedFrame struct {
	*Frame
	// Numeric は数値として解釈できた列。
	Numeric map[string]bool
}

// Float は行 i の列 name を float64 で返す。数値列でないか欠測なら ok=false。
func (t *TypedFrame) Float(i int, name string) (float64, bool) {
	if !t.Numeric[name] {
		return 0, false
	}
	v := t.Get(i, name)
	if v == nil {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(*v), 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// Typed は数値に解釈できる列を見つけて TypedFrame にする。
func Typed(f *Frame, exclude ...string) *TypedFrame {
	numeric := map[string]bool{}
	for _, name := range NumericColumns(f, exclude...) {
		numeric[name] = true
	}
	return &TypedFrame{Frame: f, Numeric: numeric}
}

// keyOf は鍵列（位置で指定）の値を連結したもの。上書きの単位。
func keyOf(row Row, key []int) string {
	var b strings.Builder
	for _, j := range key {
		if v := cell(row, j); v != nil {
			b.WriteString(*v)
		}
		b.WriteByte(0x1f)
	}
	return b.String()
}

// rowDigest は 1 行の内容のハッシュ。列名でそろえるので列順に依らない。
//
// 行の正規化文字列（rowSignature）を残さずハッシュだけを持つ。80 万行の表で
// 正規化文字列を全部並べると表と同じだけのメモリをもう一度使う。
func rowDigest(row Row, columns []string, positions []int) [sha256.Size]byte {
	h := sha256.New()
	for k, name := range columns {
		h.Write([]byte(name))
		h.Write([]byte{'='})
		if v := cell(row, positions[k]); v == nil {
			h.Write([]byte{0x00}) // NULL と空文字を混同しない
		} else {
			h.Write([]byte(*v))
		}
		h.Write([]byte{0x1f})
	}
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out
}

// DigestOf は応答の内容のハッシュ。同じ内容の取り直しを台帳で見分ける。
// 行順にも列順にも依らない（行ごとのハッシュを並べ替えてから束ねる）。
func DigestOf(f *Frame) string {
	h := sha256.New()
	if f.Height() == 0 {
		return hex.EncodeToString(h.Sum(nil))
	}
	columns := append([]string(nil), f.Columns...)
	sort.Strings(columns)
	positions := f.cols(columns)
	digests := make([][sha256.Size]byte, 0, f.Height())
	for _, row := range f.Rows {
		digests = append(digests, rowDigest(row, columns, positions))
	}
	sort.Slice(digests, func(i, j int) bool { return bytes.Compare(digests[i][:], digests[j][:]) < 0 })
	for i := range digests {
		h.Write(digests[i][:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// countChanged は新しい行のうち、既存に無いか内容が違うものの数。
func countChanged(old, new *Frame, key []string) int {
	common := make([]string, 0, len(new.Columns))
	for _, c := range new.Columns {
		if old.HasColumn(c) {
			common = append(common, c)
		}
	}
	sort.Strings(common)
	oldPos, newPos := old.cols(common), new.cols(common)
	oldKey, newKey := old.cols(key), new.cols(key)
	// 旧表は鍵 → 行ハッシュだけを持つ（正規化文字列を全行ぶん持つと旧表と同じ量のメモリになる）
	previous := make(map[string][sha256.Size]byte, old.Height())
	for _, row := range old.Rows {
		previous[keyOf(row, oldKey)] = rowDigest(row, common, oldPos)
	}
	changed := 0
	for _, row := range new.Rows {
		before, ok := previous[keyOf(row, newKey)]
		if !ok || before != rowDigest(row, common, newPos) {
			changed++
		}
	}
	return changed
}

// dedupeLast は鍵で重複を潰し、後に現れた行を残す（後勝ち）。出現順は保つ。
func dedupeLast(f *Frame, key []string) *Frame {
	keyPos := f.cols(key)
	position := make(map[string]int, f.Height())
	kept := make([]Row, 0, f.Height())
	for _, row := range f.Rows {
		k := keyOf(row, keyPos)
		if i, ok := position[k]; ok {
			kept[i] = row
			continue
		}
		position[k] = len(kept)
		kept = append(kept, row)
	}
	return &Frame{Columns: append([]string(nil), f.Columns...), Rows: kept}
}

// sortByKey は鍵の昇順に並べる。差分を見やすくし、Parquet の圧縮も効かせる。
//
// 鍵は行ごとに一度だけ組む。比較関数の中で組むと O(n log n) 回の文字列生成に
// なり、8 万行で 270 万回のアロケートになる。
func sortByKey(f *Frame, key []string) {
	keyPos := f.cols(key)
	keys := make([]string, len(f.Rows))
	order := make([]int, len(f.Rows))
	for i, row := range f.Rows {
		keys[i] = keyOf(row, keyPos)
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return keys[order[a]] < keys[order[b]] })
	sorted := make([]Row, len(f.Rows))
	for i, from := range order {
		sorted[i] = f.Rows[from]
	}
	f.Rows = sorted
}

// concatDiagonal は列を和集合にして 2 つの表を縦に繋ぐ
// （仕様変更で列が増えても古い月と一緒に読めるようにするため）。
func concatDiagonal(frames ...*Frame) *Frame {
	out := &Frame{}
	for _, f := range frames {
		if f == nil {
			continue
		}
		for _, c := range f.Columns {
			out.addColumn(c)
		}
		// 列の並びが同じならそのまま繋ぐ。違えば行を out の並びに並べ直す
		mapping := out.cols(f.Columns)
		identity := true
		for j, to := range mapping {
			if to != j {
				identity = false
				break
			}
		}
		if identity {
			out.Rows = append(out.Rows, f.Rows...)
			continue
		}
		for _, row := range f.Rows {
			aligned := make(Row, len(out.Columns))
			for j, v := range row {
				if j < len(mapping) && mapping[j] >= 0 {
					aligned[mapping[j]] = v
				}
			}
			out.Rows = append(out.Rows, aligned)
		}
	}
	return out
}

// AsOf は「その時点で見えていた最新の 1 件」を銘柄ごとに取る（ルックアヘッド防止の定型）。
func AsOf(f *Frame, date time.Time, dateColumn, by string) *Frame {
	if dateColumn == "" {
		dateColumn = "DiscDate"
	}
	if by == "" {
		by = "Code"
	}
	cutoff := date.Format(dateLayout)
	dateIdx, byIdx := f.col(dateColumn), f.col(by)
	visible := make([]Row, 0, f.Height())
	for _, row := range f.Rows {
		d := cell(row, dateIdx)
		if d == nil || *d > cutoff {
			continue
		}
		visible = append(visible, row)
	}
	order := []string{dateColumn}
	if f.HasColumn("DiscTime") {
		order = append(order, "DiscTime")
	}
	orderPos := f.cols(order)
	sort.SliceStable(visible, func(i, j int) bool {
		return keyOf(visible[i], orderPos) < keyOf(visible[j], orderPos)
	})
	latest := map[string]Row{}
	var groups []string
	for _, row := range visible {
		g := ""
		if v := cell(row, byIdx); v != nil {
			g = *v
		}
		if _, ok := latest[g]; !ok {
			groups = append(groups, g)
		}
		latest[g] = row
	}
	out := &Frame{Columns: append([]string(nil), f.Columns...)}
	for _, g := range groups {
		out.Rows = append(out.Rows, latest[g])
	}
	return out
}
