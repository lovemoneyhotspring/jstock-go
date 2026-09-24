package backtest

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/usmarket"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/shopspring/decimal"
)

// 格子（複数の設定を 1 プロセスで回す）。
//
// パネルと危険信号の材料は設定に依存しないので 1 回だけ読み、設定ごとに母集団を
// 当て直して（universeViewWith）Simulate に掛ける。11 本回す日で読み込みを 11 回払わない。

// GridEntry は格子の 1 本（設定と、出力に付ける名前）。
type GridEntry struct {
	Name   string
	Config config.Config
}

// GridResult は 1 本ぶんの結果。ロングだけの設定は Margin が nil。
type GridResult struct {
	Name    string
	Config  config.Config
	Result  *Result
	Margin  *MarginResult
	Elapsed time.Duration
}

// Summary はロング単独・長短どちらでも使える要約。
func (g GridResult) Summary() Summary {
	if g.Margin != nil {
		return g.Margin.Summary
	}
	return g.Result.Summary
}

// Capital はその設定の資金（年率の分母。長短なら現金）。
func (g GridResult) Capital() float64 {
	if g.Margin == nil {
		v, _ := g.Config.Capital.MaxCapital.Float64()
		return v
	}
	cash := g.Config.Margin.Cash
	if cash.IsZero() {
		cash = g.Config.Capital.MaxCapital
	}
	v, _ := cash.Float64()
	return v
}

// Carried はショートの張り付き件数（長短のときだけ）。
func (g GridResult) Carried() (carried, total int) {
	if g.Margin == nil {
		return 0, 0
	}
	for _, t := range g.Margin.ShortTrades {
		if t.Carried {
			carried++
		}
	}
	return carried, len(g.Margin.ShortTrades)
}

// RunGrid は複数の設定を 1 回の読み込みで回す。
//
// build はパネルを見て約定モデルを作る関数（分足。銘柄集合はパネルで決まるので 1 回）。
// 危険信号の材料は regime の設定が同じ設定どうしで使い回す。
func RunGrid(arch *archive.Archive, entries []GridEntry, start, end time.Time,
	us usmarket.Fetcher, cachePath string, build OptionsFor) ([]GridResult, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("設定が 1 つも渡されていません")
	}
	// 下限は並べた設定の中で最も緩いもの（どの設定の母集団も落とさない）
	floor := PanelTurnoverFloor
	lower := func(d decimal.Decimal) {
		if v, _ := d.Float64(); v > 0 && v < floor {
			floor = v
		}
	}
	for _, e := range entries {
		lower(e.Config.Universe.MinTurnover)
		if e.Config.Margin.Enabled {
			lower(e.Config.Margin.MinTurnover)
		}
	}
	base, err := LoadPanelWith(arch, start, end, entries[0].Config, PanelOptions{KeepAll: true, TurnoverFloor: floor})
	if err != nil {
		return nil, err
	}
	var opts Options
	if build != nil {
		if opts, err = build(base); err != nil {
			return nil, err
		}
	}
	signals := map[string]*Inputs{}
	// 3 分位は min_turnover にしか依存しない。同じ設定どうしで使い回す
	terciles := map[float64][]int{}
	viewRows := 0
	out := make([]GridResult, 0, len(entries))
	for _, e := range entries {
		began := time.Now()
		key := signalsKey(e.Config)
		in, ok := signals[key]
		if !ok {
			if in, err = SignalsFor(arch, e.Config, base, us, cachePath); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name, err)
			}
			signals[key] = in
		}
		minTurnover, _ := e.Config.Universe.MinTurnover.Float64()
		t, ok := terciles[minTurnover]
		if !ok {
			t = tercilesFor(base, e.Config)
			terciles[minTurnover] = t
		}
		view := universeViewWith(base, e.Config, t, viewRows)
		viewRows = len(view.Rows)
		g := GridResult{Name: e.Name, Config: e.Config}
		if e.Config.Margin.Enabled {
			g.Margin, err = SimulateMarginWith(view, e.Config, in, opts)
		} else {
			g.Result, err = SimulateWith(view, e.Config, in, opts)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name, err)
		}
		g.Elapsed = time.Since(began)
		out = append(out, g)
	}
	return out, nil
}

// signalsKey は「危険信号の材料が同じになる設定」をまとめる鍵。
func signalsKey(cfg config.Config) string {
	return fmt.Sprintf("drift=%d|us=%v", cfg.Regime.DriftDays, cfg.Regime.UsSkipHigh != nil)
}
