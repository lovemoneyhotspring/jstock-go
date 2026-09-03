package strategy

import (
	"math"
	"sort"
	"sync"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// Series は 1 銘柄の全履歴と、そこから計算した指標列のキャッシュ。
//
// ここで扱う指標はすべて「その日までの足しか見ない」因果的な計算なので、
// 全履歴で一度計算してから日付で切り詰めても、切り詰めてから計算した結果と
// 一致する。バックテストは同じ銘柄を日数ぶん繰り返し評価するため、毎回
// 計算し直すと日数の二乗に比例して遅くなる。キャッシュはそこを断ち切る。
type Series struct {
	Symbol string
	Bars   []domain.Bar

	Open   []float64
	High   []float64
	Low    []float64
	Close  []float64
	Volume []float64

	mu   sync.Mutex
	cols map[string][]float64
}

// NewSeries は日足（date 昇順）から Series を作る。
func NewSeries(symbol string, bars []domain.Bar) *Series {
	n := len(bars)
	s := &Series{
		Symbol: symbol,
		Bars:   bars,
		Open:   make([]float64, n),
		High:   make([]float64, n),
		Low:    make([]float64, n),
		Close:  make([]float64, n),
		Volume: make([]float64, n),
		cols:   make(map[string][]float64),
	}
	for i, b := range bars {
		s.Open[i], _ = b.Open.Float64()
		s.High[i], _ = b.High.Float64()
		s.Low[i], _ = b.Low.Float64()
		s.Close[i], _ = b.Close.Float64()
		s.Volume[i], _ = b.Volume.Float64()
	}
	return s
}

// column は key の列をキャッシュから引く。無ければ compute で作って覚える。
func (s *Series) column(key string, compute func() []float64) []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if col, ok := s.cols[key]; ok {
		return col
	}
	col := compute()
	if len(col) != len(s.Bars) {
		// 長さが違うと切り詰めの前提が崩れる。NaN で埋めて長さを合わせる。
		fixed := nanSlice(len(s.Bars))
		copy(fixed, col)
		col = fixed
	}
	s.cols[key] = col
	return col
}

// View は Series を判断基準日まで切り詰めた眺め。
//
// 実体は共有したまま長さだけを絞るので、日ごとに作っても複製は起きない。
// 未来の足を見る手段が構造として存在しないため、先読みバイアスは
// 「気をつける」ではなく「不可能」になる。
type View struct {
	series *Series
	n      int
}

func (v View) Symbol() string     { return v.series.Symbol }
func (v View) Len() int           { return v.n }
func (v View) Bars() []domain.Bar { return v.series.Bars[:v.n] }
func (v View) Opens() []float64   { return v.series.Open[:v.n] }
func (v View) Highs() []float64   { return v.series.High[:v.n] }
func (v View) Lows() []float64    { return v.series.Low[:v.n] }
func (v View) Closes() []float64  { return v.series.Close[:v.n] }
func (v View) Volumes() []float64 { return v.series.Volume[:v.n] }

// Date は i 番目の足の日付（YYYY-MM-DD）。
func (v View) Date(i int) string {
	if i < 0 || i >= v.n {
		return ""
	}
	return v.series.Bars[i].Date
}

// LastDate は基準日の足の日付。
func (v View) LastDate() string { return v.Date(v.n - 1) }

// col は指標列を（切り詰めたうえで）返す。計算は全履歴に対して一度だけ。
func (v View) col(key string, compute func(s *Series) []float64) []float64 {
	return v.series.column(key, func() []float64 { return compute(v.series) })[:v.n]
}

// Context は戦略に渡す判断材料。
//
// 戦略は注文を出さない。ここにある材料だけを見て意見（Signal）を返す。
// I/O もブローカーも時計も知らないので、モックなしで単体テストできる。
type Context struct {
	// AsOf は判断の基準日（YYYY-MM-DD）。この日の足まで見える。
	AsOf string
	// Equity は判断時点の総資産。サイジングには使わないが、戦略が
	// 資産規模に応じた判断をしたい場合に参照できる。
	Equity decimal.Decimal

	views     map[string]View
	positions map[string]domain.Position
	symbols   []string
}

// Symbols は足が用意されている銘柄（辞書順）。
func (c *Context) Symbols() []string { return c.symbols }

// Bars は symbol の足。無ければ ok=false。
func (c *Context) Bars(symbol string) (View, bool) {
	v, ok := c.views[symbol]
	return v, ok
}

// HasBars は minimum 本以上の足があるか。
func (c *Context) HasBars(symbol string, minimum int) bool {
	v, ok := c.views[symbol]
	return ok && v.n >= minimum
}

// Close は最新の終値。足が無ければ NaN。
func (c *Context) Close(symbol string) float64 {
	v, ok := c.views[symbol]
	if !ok || v.n == 0 {
		return math.NaN()
	}
	return v.series.Close[v.n-1]
}

// Position は現在の建玉。無ければ ok=false。
func (c *Context) Position(symbol string) (domain.Position, bool) {
	p, ok := c.positions[symbol]
	return p, ok
}

// HasPosition は数量が正の建玉を持っているか。
func (c *Context) HasPosition(symbol string) bool {
	p, ok := c.positions[symbol]
	return ok && p.Quantity.IsPositive()
}

// HeldSymbols は保有中の銘柄（辞書順）。
func (c *Context) HeldSymbols() []string {
	out := make([]string, 0, len(c.positions))
	for sym, p := range c.positions {
		if p.Quantity.IsPositive() {
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

// Universe は銘柄ごとの Series を束ねた入れ物。
//
// バックテストはこれを一度だけ作り、日ごとに At で Context を切り出す。
// 指標のキャッシュが Series 側にあるので、日数ぶん再計算されない。
type Universe struct {
	series map[string]*Series
}

// NewUniverse は銘柄→日足（date 昇順）から Universe を作る。
func NewUniverse(bars map[string][]domain.Bar) *Universe {
	u := &Universe{series: make(map[string]*Series, len(bars))}
	for sym, b := range bars {
		u.series[sym] = NewSeries(sym, b)
	}
	return u
}

// At は asOf 時点の Context を切り出す。asOf が空なら全期間を見せる。
//
// 各銘柄の足は asOf 以前に切り詰められる。1 本も無い銘柄は入れない。
func (u *Universe) At(asOf string, positions map[string]domain.Position, equity decimal.Decimal) *Context {
	ctx := &Context{
		AsOf:      asOf,
		Equity:    equity,
		views:     make(map[string]View, len(u.series)),
		positions: positions,
	}
	if ctx.positions == nil {
		ctx.positions = map[string]domain.Position{}
	}
	for sym, s := range u.series {
		n := len(s.Bars)
		if asOf != "" {
			// 日付は昇順なので二分探索で「asOf 以前」の本数が出る。
			n = sort.Search(len(s.Bars), func(i int) bool { return s.Bars[i].Date > asOf })
		}
		if n == 0 {
			continue
		}
		ctx.views[sym] = View{series: s, n: n}
		ctx.symbols = append(ctx.symbols, sym)
	}
	sort.Strings(ctx.symbols)
	return ctx
}

// NewContext は 1 日ぶんの判断のために Context を直接作る（ライブ実行・テスト用）。
func NewContext(asOf string, bars map[string][]domain.Bar, positions map[string]domain.Position, equity decimal.Decimal) *Context {
	return NewUniverse(bars).At(asOf, positions, equity)
}
