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
type Frame struct {
	Columns []string
	Rows    []map[string]*string
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
	if !f.HasColumn(name) {
		f.Columns = append(f.Columns, name)
	}
}

// Get は行 i の列 name の値。無ければ nil。
func (f *Frame) Get(i int, name string) *string {
	if i < 0 || i >= len(f.Rows) {
		return nil
	}
	return f.Rows[i][name]
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
	dates := ep.dateColumnSet()
	for _, row := range f.Rows {
		for name := range dates {
			if v, ok := row[name]; ok {
				row[name] = normalizeDate(v)
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
	f := &Frame{}
	for _, row := range rows {
		// 列の出現順を安定させるため、行ごとにキーを並べてから足す
		names := make([]string, 0, len(row))
		for k := range row {
			names = append(names, k)
		}
		sort.Strings(names)
		out := make(map[string]*string, len(row))
		for _, k := range names {
			f.addColumn(k)
			out[k] = stringify(row[k])
		}
		f.Rows = append(f.Rows, out)
	}
	if err := withDates(f, ep); err != nil {
		return nil, err
	}
	return f, nil
}

// CSVToFrame は一括ダウンロードの csv.gz を保存形にする。列名は API と同じ前提。
func CSVToFrame(payload []byte, ep Endpoint) (*Frame, error) {
	raw := payload
	if len(payload) >= 2 && payload[0] == 0x1f && payload[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("一括 CSV の展開に失敗しました: %w", err)
		}
		defer zr.Close()
		decoded, err := io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("一括 CSV の展開に失敗しました: %w", err)
		}
		raw = decoded
	}
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.FieldsPerRecord = -1 // 列数の揺れ（末尾の空列など）で全体を落とさない
	reader.LazyQuotes = true
	header, err := reader.Read()
	if err == io.EOF {
		return &Frame{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("一括 CSV の見出しを読めません: %w", err)
	}
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
		row := make(map[string]*string, len(header))
		for i, name := range header {
			if i >= len(record) || record[i] == "" {
				row[name] = nil // 空欄は NULL（polars の read_csv + when/then と同じ）
				continue
			}
			row[name] = strptr(record[i])
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
		for i := 0; i < limit && ok; i++ {
			v := f.Rows[i][name]
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

// rowSignature は行の内容を比較・ハッシュするための正規化文字列。
// 列名でそろえるので、列順が違っても同じ内容なら同じ文字列になる。
func rowSignature(row map[string]*string, columns []string) string {
	var b strings.Builder
	for _, name := range columns {
		b.WriteString(name)
		b.WriteByte('=')
		if v := row[name]; v == nil {
			b.WriteByte(0x00) // NULL と空文字を混同しない
		} else {
			b.WriteString(*v)
		}
		b.WriteByte(0x1f)
	}
	return b.String()
}

// keyOf は鍵列の値を連結したもの。上書きの単位。
func keyOf(row map[string]*string, key []string) string {
	var b strings.Builder
	for _, name := range key {
		if v := row[name]; v != nil {
			b.WriteString(*v)
		}
		b.WriteByte(0x1f)
	}
	return b.String()
}

// DigestOf は応答の内容のハッシュ。同じ内容の取り直しを台帳で見分ける。
func DigestOf(f *Frame) string {
	h := sha256.New()
	if f.Height() == 0 {
		return hex.EncodeToString(h.Sum(nil))
	}
	columns := append([]string(nil), f.Columns...)
	sort.Strings(columns)
	signatures := make([]string, 0, f.Height())
	for _, row := range f.Rows {
		signatures = append(signatures, rowSignature(row, columns))
	}
	sort.Strings(signatures) // 行順に依らない
	for _, s := range signatures {
		h.Write([]byte(s))
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
	previous := make(map[string]string, old.Height())
	for _, row := range old.Rows {
		previous[keyOf(row, key)] = rowSignature(row, common)
	}
	changed := 0
	for _, row := range new.Rows {
		before, ok := previous[keyOf(row, key)]
		if !ok || before != rowSignature(row, common) {
			changed++
		}
	}
	return changed
}

// dedupeLast は鍵で重複を潰し、後に現れた行を残す（後勝ち）。出現順は保つ。
func dedupeLast(f *Frame, key []string) *Frame {
	position := make(map[string]int, f.Height())
	kept := make([]map[string]*string, 0, f.Height())
	for _, row := range f.Rows {
		k := keyOf(row, key)
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
func sortByKey(f *Frame, key []string) {
	sort.SliceStable(f.Rows, func(i, j int) bool {
		return keyOf(f.Rows[i], key) < keyOf(f.Rows[j], key)
	})
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
		out.Rows = append(out.Rows, f.Rows...)
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
	visible := make([]map[string]*string, 0, f.Height())
	for _, row := range f.Rows {
		d := row[dateColumn]
		if d == nil || *d > cutoff {
			continue
		}
		visible = append(visible, row)
	}
	order := []string{dateColumn}
	if f.HasColumn("DiscTime") {
		order = append(order, "DiscTime")
	}
	sort.SliceStable(visible, func(i, j int) bool {
		return keyOf(visible[i], order) < keyOf(visible[j], order)
	})
	latest := map[string]map[string]*string{}
	var groups []string
	for _, row := range visible {
		g := ""
		if v := row[by]; v != nil {
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
