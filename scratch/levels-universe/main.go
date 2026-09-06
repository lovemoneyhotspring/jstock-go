// 使い捨て: 節目反発戦略（level_bounce）の検証ユニバースを作る。
//
//	go run ./scratch/levels-universe -top 300 -out config/jp-levels/symbols.txt
//
//  1. J-Quants アーカイブの日足から、売買代金（調整後終値 × 出来高）の中央値が大きい
//     株式（ProdCat=011）を上から top 銘柄選ぶ
//  2. その銘柄の調整済み日足を since 以降、wbjp の BarStore（data/bars）へ書く
//
// 「今も上場している銘柄から選ぶ」ので生存バイアスがある。検証の記録に必ず書くこと。
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

const dayLayout = "2006-01-02"

// stockProduct は J-Quants の ProdCat。011 が株式（ETF・REIT・優先株等を除く）。
const stockProduct = "011"

type stock struct {
	code, name string
}

func main() {
	var (
		top      = flag.Int("top", 300, "選ぶ銘柄数")
		skip     = flag.Int("skip", 0, "上位からこの数だけ飛ばす（中小型株の母集団を作るとき）")
		topix    = flag.Bool("topix", false, "TOPIX（^TOPIX）の足も BarStore に書く")
		masterAt = flag.String("master-as-of", "", "銘柄一覧をこの日時点で取る（YYYY-MM-DD。空なら最新＝生存バイアスあり）")
		out      = flag.String("out", "config/jp-levels/symbols.txt", "銘柄リストの出力先")
		since    = flag.String("since", "2015-01-01", "日足の開始日")
		rankFrom = flag.String("rank-from", "2024-01-01", "売買代金で順位付けする期間の開始")
		rankTo   = flag.String("rank-to", "2025-12-31", "売買代金で順位付けする期間の終了")
		jqDir    = flag.String("jquants", "data/jquants", "J-Quants アーカイブ")
	)
	flag.Parse()

	arch := archive.NewArchive(*jqDir)
	store := data.NewBarStore(settings.LoadAppSettings().BarsDir())
	epBars := archive.MustEndpoint("equities_bars_daily")
	epMaster := archive.MustEndpoint("equities_master")

	t0 := time.Now()
	stocks := loadStocks(arch, epMaster, *masterAt)
	fmt.Fprintf(os.Stderr, "株式 %d 銘柄（%.1fs）\n", len(stocks), time.Since(t0).Seconds())

	// 1) 売買代金の中央値で順位付け
	from, _ := time.Parse(dayLayout, *rankFrom)
	to, _ := time.Parse(dayLayout, *rankTo)
	frame, err := arch.ReadWhere(epBars, archive.ReadOptions{
		Start:   from,
		End:     to,
		Columns: []string{"Code", "AdjC", "AdjVo"},
		Keep: func(row archive.RowView) bool {
			_, ok := stocks[row.Text("Code")]
			return ok
		},
	})
	must(err)
	turnover := map[string][]float64{}
	for i := 0; i < frame.Height(); i++ {
		code := text(frame.Get(i, "Code"))
		c, okC := num(frame.Get(i, "AdjC"))
		v, okV := num(frame.Get(i, "AdjVo"))
		if !okC || !okV {
			continue
		}
		turnover[code] = append(turnover[code], c*v)
	}
	days := 0
	for _, xs := range turnover {
		if len(xs) > days {
			days = len(xs)
		}
	}
	type ranked struct {
		code   string
		median float64
	}
	var rows []ranked
	for code, xs := range turnover {
		if len(xs) < days*9/10 { // 期間の大半で立ち会っていること（新規上場を外す）
			continue
		}
		sort.Float64s(xs)
		rows = append(rows, ranked{code, xs[len(xs)/2]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].median > rows[j].median })
	if *skip > 0 && *skip < len(rows) {
		rows = rows[*skip:]
	}
	if len(rows) > *top {
		rows = rows[:*top]
	}
	fmt.Fprintf(os.Stderr, "順位付け: %d 営業日、候補 %d、採用 %d（%.1fs）\n",
		days, len(turnover), len(rows), time.Since(t0).Seconds())

	selected := map[string]bool{}
	for _, r := range rows {
		selected[r.code] = true
	}

	// 2) 調整済み日足を BarStore へ
	start, _ := time.Parse(dayLayout, *since)
	frame, err = arch.ReadWhere(epBars, archive.ReadOptions{
		Start:   start,
		Columns: []string{"Code", "AdjO", "AdjH", "AdjL", "AdjC", "AdjVo"},
		Keep:    func(row archive.RowView) bool { return selected[row.Text("Code")] },
	})
	must(err)
	bars := map[string][]domain.Bar{}
	skipped := 0
	for i := 0; i < frame.Height(); i++ {
		code := text(frame.Get(i, "Code"))
		date := text(frame.Get(i, epBars.DateColumn))
		o, ok1 := num(frame.Get(i, "AdjO"))
		h, ok2 := num(frame.Get(i, "AdjH"))
		l, ok3 := num(frame.Get(i, "AdjL"))
		c, ok4 := num(frame.Get(i, "AdjC"))
		v, ok5 := num(frame.Get(i, "AdjVo"))
		if !(ok1 && ok2 && ok3 && ok4 && ok5) || date == "" {
			skipped++
			continue
		}
		sym := symbolOf(code)
		b, err := domain.NewBar(sym, date,
			decimal.NewFromFloat(o), decimal.NewFromFloat(h), decimal.NewFromFloat(l),
			decimal.NewFromFloat(c), decimal.NewFromFloat(v))
		if err != nil {
			skipped++
			continue
		}
		bars[sym] = append(bars[sym], b)
	}
	fmt.Fprintf(os.Stderr, "足 %d 行を読み、%d 行を捨てた（%.1fs）\n", frame.Height(), skipped, time.Since(t0).Seconds())

	if *topix {
		must(writeTopix(arch, store, start))
	}

	var lines []string
	for _, r := range rows {
		sym := symbolOf(r.code)
		bs := bars[sym]
		sort.Slice(bs, func(i, j int) bool { return bs[i].Date < bs[j].Date })
		if len(bs) == 0 {
			continue
		}
		must(store.Write(sym, bs))
		lines = append(lines, fmt.Sprintf("%s  # %s 売買代金中央値 %.0f 百万円 足 %d 本 %s〜%s",
			sym, stocks[r.code].name, r.median/1e6, len(bs), bs[0].Date, bs[len(bs)-1].Date))
	}
	must(os.WriteFile(*out, []byte(strings.Join(lines, "\n")+"\n"), 0644))
	fmt.Fprintf(os.Stderr, "%d 銘柄を %s と %s に書いた（%.1fs）\n",
		len(lines), *out, settings.LoadAppSettings().BarsDir(), time.Since(t0).Seconds())
}

// writeTopix は TOPIX の日足を ^TOPIX として BarStore に書く（地合いフィルタと買い持ち対照用）。
func writeTopix(arch *archive.Archive, store *data.BarStore, start time.Time) error {
	ep := archive.MustEndpoint("indices_bars_daily_topix")
	frame, err := arch.ReadWhere(ep, archive.ReadOptions{Start: start, Columns: []string{"O", "H", "L", "C"}})
	if err != nil {
		return err
	}
	var bars []domain.Bar
	for i := 0; i < frame.Height(); i++ {
		date := text(frame.Get(i, ep.DateColumn))
		o, ok1 := num(frame.Get(i, "O"))
		h, ok2 := num(frame.Get(i, "H"))
		l, ok3 := num(frame.Get(i, "L"))
		c, ok4 := num(frame.Get(i, "C"))
		if !(ok1 && ok2 && ok3 && ok4) || date == "" {
			continue
		}
		b, err := domain.NewBar("^TOPIX", date, decimal.NewFromFloat(o), decimal.NewFromFloat(h),
			decimal.NewFromFloat(l), decimal.NewFromFloat(c), decimal.Zero)
		if err != nil {
			continue
		}
		bars = append(bars, b)
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	if len(bars) == 0 {
		return fmt.Errorf("TOPIX の足が無い")
	}
	fmt.Fprintf(os.Stderr, "^TOPIX %d 本 %s〜%s（終値 %s → %s）\n", len(bars), bars[0].Date, bars[len(bars)-1].Date,
		bars[0].Close.StringFixed(2), bars[len(bars)-1].Close.StringFixed(2))
	return store.Write("^TOPIX", bars)
}

// loadStocks は銘柄一覧（asOf 時点。空なら最新）から株式（ProdCat=011）だけ拾う。
//
// asOf を過去にすると、その後に上場廃止になった銘柄も母集団に入る（生存バイアスの検定用）。
func loadStocks(arch *archive.Archive, ep archive.Endpoint, asOf string) map[string]stock {
	now := time.Now()
	if asOf != "" {
		now, _ = time.Parse(dayLayout, asOf)
	}
	frame, err := arch.ReadWhere(ep, archive.ReadOptions{
		Start:   now.AddDate(0, 0, -45),
		End:     now,
		Columns: []string{"Code", "CoName", "ProdCat"},
	})
	must(err)
	latest := ""
	for i := 0; i < frame.Height(); i++ {
		if d := text(frame.Get(i, ep.DateColumn)); d > latest {
			latest = d
		}
	}
	out := map[string]stock{}
	for i := 0; i < frame.Height(); i++ {
		if text(frame.Get(i, ep.DateColumn)) != latest || text(frame.Get(i, "ProdCat")) != stockProduct {
			continue
		}
		code := text(frame.Get(i, "Code"))
		if symbolOf(code) == "" {
			continue
		}
		out[code] = stock{code: code, name: text(frame.Get(i, "CoName"))}
	}
	return out
}

// symbolOf は J-Quants の 5 桁コード（72030）を東証の 4 桁（7203）にする。
// 末尾が 0 でないもの（優先株等）は空を返す。
func symbolOf(code string) string {
	if len(code) == 5 && code[4] == '0' {
		return code[:4]
	}
	if len(code) == 4 {
		return code
	}
	return ""
}

func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func num(p *string) (float64, bool) {
	if p == nil || *p == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(*p, 64)
	return v, err == nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
