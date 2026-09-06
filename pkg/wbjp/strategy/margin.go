package strategy

import (
	"math"
	"sort"
	"time"
)

// MarginPublicationLag は信用残（金曜時点）が使えるようになるまでの暦日数。
//
// 週末の残高は翌週火曜の大引け後に公表されるので、水曜（金曜 + 5 日）から見せる。
// 金曜が休場で木曜時点になる週（全体の 3%）は 1 日早く見えてしまうが、遅らせる側に
// 倒すと通常の週で 1 日古い値を使うことになる。研究（scratch/sentiment-study）と同じ扱い。
const MarginPublicationLag = 5

// MarginRecord は 1 銘柄・1 週の信用残（制度・一般の合計）。
type MarginRecord struct {
	Date  string  // 残高の基準日（YYYY-MM-DD、多くは金曜）
	Long  float64 // 買い残（株）
	Short float64 // 売り残（株）
}

// Ratio は信用倍率（買残 ÷ 売残）。売残ゼロは +Inf（極端な買い長）、両方ゼロは NaN。
func (r MarginRecord) Ratio() float64 {
	switch {
	case r.Short > 0:
		return r.Long / r.Short
	case r.Long > 0:
		return math.Inf(1)
	default:
		return math.NaN()
	}
}

// AvailableFrom は公表を踏まえて使えるようになる日。日付が読めなければ空。
func (r MarginRecord) AvailableFrom() string { return r.availableAfter(MarginPublicationLag) }

func (r MarginRecord) availableAfter(lagDays int) string {
	d, err := time.Parse("2006-01-02", r.Date)
	if err != nil {
		return ""
	}
	return d.AddDate(0, 0, lagDays).Format("2006-01-02")
}

// MarginBook は銘柄→週次の信用残。判断日に「見えている」最新の週を引く。
type MarginBook struct {
	records   map[string][]MarginRecord // Date 昇順
	available map[string][]string       // records と同じ並びの AvailableFrom
}

// NewMarginBook は信用残を銘柄ごとに日付順へ並べて束ねる。
func NewMarginBook(records map[string][]MarginRecord) *MarginBook {
	return NewMarginBookWithLag(records, MarginPublicationLag)
}

// NewMarginBookWithLag は公表までの遅れを lagDays（暦日）に変えて束ねる。
//
// 検証用。実際より長く遅らせても成績が変わらないなら、効いているのは
// 信用残の情報ではなく銘柄の絞り込みそのもの（経路の偶然）だと分かる。
func NewMarginBookWithLag(records map[string][]MarginRecord, lagDays int) *MarginBook {
	b := &MarginBook{
		records:   make(map[string][]MarginRecord, len(records)),
		available: make(map[string][]string, len(records)),
	}
	for sym, rs := range records {
		sorted := append([]MarginRecord(nil), rs...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Date < sorted[j].Date })
		avail := make([]string, len(sorted))
		for i, r := range sorted {
			avail[i] = r.availableAfter(lagDays)
		}
		b.records[sym] = sorted
		b.available[sym] = avail
	}
	return b
}

// Len は信用残のある銘柄数。
func (b *MarginBook) Len() int {
	if b == nil {
		return 0
	}
	return len(b.records)
}

// Weeks は全銘柄の週数の合計。
func (b *MarginBook) Weeks() int {
	if b == nil {
		return 0
	}
	n := 0
	for _, rs := range b.records {
		n += len(rs)
	}
	return n
}

// AsOf は asOf 時点で公表済みの最新の週。asOf が空なら最新。無ければ ok=false。
func (b *MarginBook) AsOf(symbol, asOf string) (MarginRecord, bool) {
	if b == nil {
		return MarginRecord{}, false
	}
	rs := b.records[symbol]
	if len(rs) == 0 {
		return MarginRecord{}, false
	}
	if asOf == "" {
		return rs[len(rs)-1], true
	}
	avail := b.available[symbol]
	// available は昇順。asOf 以下の最後の要素を探す。
	i := sort.Search(len(avail), func(i int) bool { return avail[i] > asOf })
	if i == 0 {
		return MarginRecord{}, false
	}
	return rs[i-1], true
}

// Margin は asOf 時点で見える最新の信用残。信用残が無い環境では常に ok=false。
func (c *Context) Margin(symbol string) (MarginRecord, bool) {
	return c.margin.AsOf(symbol, c.AsOf)
}

// MarginRatioRank は信用倍率の銘柄横断の分位（0〜1。大きいほど買い長）。
//
// 分位は「足が用意されていて信用残もある銘柄」の中で取る。同順位は中央の値。
// 信用残の無い銘柄・売残も買残もゼロの銘柄は ok=false。
func (c *Context) MarginRatioRank(symbol string) (float64, bool) {
	c.marginOnce.Do(c.computeMarginRanks)
	r, ok := c.marginRanks[symbol]
	return r, ok
}

func (c *Context) computeMarginRanks() {
	c.marginRanks = map[string]float64{}
	if c.margin == nil {
		return
	}
	type entry struct {
		symbol string
		ratio  float64
	}
	var entries []entry
	for _, sym := range c.symbols {
		rec, ok := c.margin.AsOf(sym, c.AsOf)
		if !ok {
			continue
		}
		ratio := rec.Ratio()
		if math.IsNaN(ratio) {
			continue
		}
		entries = append(entries, entry{sym, ratio})
	}
	n := len(entries)
	if n == 0 {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ratio < entries[j].ratio })
	for i := 0; i < n; {
		j := i
		for j < n && entries[j].ratio == entries[i].ratio {
			j++
		}
		// [i, j) が同順位。下にある数 + 同順位の半分 を n で割る。
		rank := (float64(i) + float64(j-i)/2) / float64(n)
		for k := i; k < j; k++ {
			c.marginRanks[entries[k].symbol] = rank
		}
		i = j
	}
}
