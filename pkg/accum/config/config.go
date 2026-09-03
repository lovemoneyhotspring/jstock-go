package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/basket"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/window"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/session"
	"github.com/pelletier/go-toml/v2"
	"github.com/shopspring/decimal"
)

type ExecutionConfig struct {
	Broker          string `toml:"broker"`
	OrderType       string `toml:"order_type"`
	LimitOffset     string `toml:"limit_offset"`
	FallbackToLimit bool   `toml:"fallback_to_limit"`
	TaxAccountType  string `toml:"tax_account_type"`
	// LotSizeOverrides は売買単位が既定と異なる銘柄の例外 {銘柄コード: 単元株数}。
	// ETF には 1 株や 10 株単位のものがある。既定の 100 株だと月の予算が
	// 1単元に届かず、発注が丸ごと見送りになる。
	LotSizeOverrides map[string]int `toml:"lot_size_overrides"`
	// MaxStaleDays は最終足がこの日数より古い銘柄を判定しないための上限。
	// 取得元の障害で古い足のまま増額判定するのを防ぐ。既定の 6 日は
	// GW・年末年始（最長 6 日）を跨げる。
	MaxStaleDays int `toml:"max_stale_days"`
}

// Validate は実行設定の値域を確かめる。
func (e *ExecutionConfig) Validate() error {
	if e.OrderType != "market" && e.OrderType != "limit" {
		return fmt.Errorf("order_type は market か limit: %q", e.OrderType)
	}
	if e.LimitOffset != "" {
		off, err := decimal.NewFromString(e.LimitOffset)
		if err != nil {
			return fmt.Errorf("limit_offset を数値として解釈できません: %q", e.LimitOffset)
		}
		if off.IsNegative() || off.GreaterThanOrEqual(decimal.RequireFromString("0.2")) {
			return fmt.Errorf("limit_offset は 0 以上 0.2 未満: %s", off)
		}
	}
	if e.MaxStaleDays < 1 {
		return fmt.Errorf("max_stale_days は 1 以上: %d", e.MaxStaleDays)
	}
	return nil
}

type TacticEntryRaw struct {
	ID            string   `toml:"id"`
	Tactic        string   `toml:"tactic"`
	Symbols       []string `toml:"symbols"`
	Enabled       *bool    `toml:"enabled"`
	Market        string   `toml:"market"`
	SignalSymbol  string   `toml:"signal_symbol"`
	SignalMarket  string   `toml:"signal_market"`
	MonthlyBudget any      `toml:"monthly_budget"`
	Multiplier    float64  `toml:"multiplier"`
	Fast          int      `toml:"fast"`
	Mid           int      `toml:"mid"`
	Slow          int      `toml:"slow"`
	Multipliers   any      `toml:"multipliers"`
	Levels        any      `toml:"levels"`
	Values        any      `toml:"values"`
	RequireDown   *bool    `toml:"require_downtrend"`
	Window        any      `toml:"window"`
}

type TacticEntry struct {
	ID            string
	Tactic        string
	Symbols       []string
	Enabled       *bool
	Market        string
	SignalSymbol  string
	SignalMarket  string
	MonthlyBudget decimal.Decimal
	Multiplier    float64
	Fast          int
	Mid           int
	Slow          int
	Multipliers   map[int]float64
	Levels        []float64
	Values        []float64
	RequireDown   bool
	Window        window.TradingWindow
}

func (e *TacticEntry) IsEnabled() bool {
	if e.Enabled == nil {
		return true
	}
	return *e.Enabled
}

// MarketResolved は銘柄の市場。省略時は日本株。
func (e *TacticEntry) MarketResolved() domain.Market {
	if e.Market == "" {
		return domain.MarketJP
	}
	return domain.Market(e.Market)
}

// SignalMarketResolved は判定用銘柄の市場。省略時は Market と同じ。
func (e *TacticEntry) SignalMarketResolved() domain.Market {
	if e.SignalMarket == "" {
		return e.MarketResolved()
	}
	return domain.Market(e.SignalMarket)
}

// SignalLags は判定用の足を 1 日遅らせるかを返す。
//
// 判定用の市場の引けが買う市場より後のとき、同じ日付の判定用の足は
// 判断時点にまだ存在しない。東証の銘柄を米国の指数で判定する場合が該当する。
func (e *TacticEntry) SignalLags() bool {
	if e.SignalSymbol == "" {
		return false
	}
	lags, err := session.ClosesAfter(e.SignalMarketResolved(), e.MarketResolved(), time.Time{})
	if err != nil {
		return false
	}
	return lags
}

// Build は戦略インスタンスを組み立てる。設定の params をここで反映する。
func (e *TacticEntry) Build() (tactics.Tactic, error) {
	var t tactics.Tactic
	var err error
	switch e.Tactic {
	case "constant", "":
		t = &tactics.Constant{}
	case "bear_stack":
		t, err = tactics.NewBearStack(e.Multiplier, e.Fast, e.Mid, e.Slow)
	case "stack_ladder":
		t, err = tactics.NewStackLadder(e.Multipliers, e.Fast, e.Mid, e.Slow)
	case "drawdown_ladder":
		t, err = tactics.NewDrawdownLadder(e.Levels, e.Values, e.RequireDown, e.Slow)
	default:
		return nil, fmt.Errorf("[%s] 未知の戦略です: %q（constant / bear_stack / stack_ladder / drawdown_ladder）", e.ID, e.Tactic)
	}
	if err != nil {
		return nil, fmt.Errorf("[%s] %w", e.ID, err)
	}
	if s, ok := t.(interface{ SetWindow(window.TradingWindow) }); ok {
		s.SetWindow(e.Window)
	}
	return t, nil
}

// BasketEntryRaw は [[baskets]] の生の記述。
type BasketEntryRaw struct {
	ID            string             `toml:"id"`
	Weights       map[string]float64 `toml:"weights"`
	Benchmark     *string            `toml:"benchmark"`
	MonthlyBudget any                `toml:"monthly_budget"`
	Tactic        string             `toml:"tactic"`
	TiltStrength  float64            `toml:"tilt_strength"`
	TiltLookback  int                `toml:"tilt_lookback"`
	Enabled       *bool              `toml:"enabled"`
	Market        string             `toml:"market"`
	// 以下は倍率戦略の固有パラメータ（[[tactics]] と同じ鍵）
	Multiplier  float64 `toml:"multiplier"`
	Fast        int     `toml:"fast"`
	Mid         int     `toml:"mid"`
	Slow        int     `toml:"slow"`
	Multipliers any     `toml:"multipliers"`
	Levels      any     `toml:"levels"`
	Values      any     `toml:"values"`
	RequireDown *bool   `toml:"require_downtrend"`
}

// BasketEntry は複数銘柄への配分（[[baskets]]）。
//
// 戦略（[[tactics]]）と違い、1銘柄が複数のバスケットに現れてよい。バスケットは
// 比較検証が主用途で、実発注は id を指定して行うため二重買付にならない。
type BasketEntry struct {
	ID string
	// Weights は配分（銘柄 → 比率）。合計は 1 でなくてよく、引くときに正規化する。
	Weights map[string]float64
	// Benchmark は同じ資金の流れを投じて比較する銘柄。
	Benchmark string
	// MonthlyBudget はバスケット全体の毎月の予算。省略時は共通設定。
	MonthlyBudget decimal.Decimal
	Tactic        string
	// TiltStrength は高値からの下落率に応じて配分を寄せる強さ。0 なら無効。
	TiltStrength float64
	// TiltLookback は下落率を測る高値の期間（足の本数）。
	TiltLookback int
	Enabled      *bool
	Market       string
	// params は倍率戦略の組み立てに使う。[[tactics]] と同じ手順を通すため、
	// 戦略のパラメータだけを詰めた TacticEntry として持つ。
	params TacticEntry
}

func (b *BasketEntry) IsEnabled() bool {
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

// MarketResolved は構成銘柄の市場。省略時は日本株。
func (b *BasketEntry) MarketResolved() domain.Market {
	if b.Market == "" {
		return domain.MarketJP
	}
	return domain.Market(b.Market)
}

// BuildTactic は各銘柄に掛ける倍率戦略を組み立てる。
func (b *BasketEntry) BuildTactic() (tactics.Tactic, error) {
	return b.params.Build()
}

// BuildTilt は配分の傾斜を組み立てる。強さが 0 なら傾斜なし（nil）。
func (b *BasketEntry) BuildTilt() (*basket.DrawdownTilt, error) {
	if b.TiltStrength <= 0 {
		return nil, nil
	}
	tilt, err := basket.NewDrawdownTilt(b.TiltStrength, b.TiltLookback)
	if err != nil {
		return nil, fmt.Errorf("[%s] %w", b.ID, err)
	}
	return tilt, nil
}

// BuildSchedule は配分表を組み立てる。いまは固定比率のみ。
func (b *BasketEntry) BuildSchedule() (basket.WeightSchedule, error) {
	if len(b.Weights) == 0 {
		return basket.WeightSchedule{}, fmt.Errorf("[%s] weights が必要です", b.ID)
	}
	return basket.Static(b.Weights), nil
}

// Symbols は構成銘柄（比率の正の銘柄）を昇順で返す。
func (b *BasketEntry) Symbols() []string {
	out := make([]string, 0, len(b.Weights))
	for symbol := range b.Weights {
		out = append(out, symbol)
	}
	sort.Strings(out)
	return out
}

type AccumConfigRaw struct {
	MonthlyBudget any              `toml:"monthly_budget"`
	KillSwitch    bool             `toml:"kill_switch"`
	DataProvider  string           `toml:"data_provider"`
	Execution     ExecutionConfig  `toml:"execution"`
	Tactics       []TacticEntryRaw `toml:"tactics"`
	Baskets       []BasketEntryRaw `toml:"baskets"`
}

type AccumConfig struct {
	MonthlyBudget decimal.Decimal
	KillSwitch    bool
	DataProvider  string
	Execution     ExecutionConfig
	Tactics       []TacticEntry
	Baskets       []BasketEntry
}

func parseDecimal(v any) decimal.Decimal {
	switch val := v.(type) {
	case int64:
		return decimal.NewFromInt(val)
	case int:
		return decimal.NewFromInt(int64(val))
	case float64:
		return decimal.NewFromFloat(val)
	case string:
		cleaned := strings.ReplaceAll(val, "_", "")
		d, err := decimal.NewFromString(cleaned)
		if err == nil {
			return d
		}
	}
	return decimal.Zero
}

func LoadAccumConfig(configDir string) (*AccumConfig, error) {
	path := filepath.Join(configDir, "accum.toml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = filepath.Join(configDir, "accum", "accum.toml")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read accum config at %s: %w", path, err)
	}

	var raw AccumConfigRaw
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse TOML %s: %w", path, err)
	}

	defaultBudget := parseDecimal(raw.MonthlyBudget)
	if defaultBudget.IsZero() {
		defaultBudget = decimal.NewFromInt(25000)
	}

	var entries []TacticEntry
	for _, tr := range raw.Tactics {
		budget := parseDecimal(tr.MonthlyBudget)
		if budget.IsZero() {
			budget = defaultBudget
		}
		if !budget.IsPositive() {
			return nil, fmt.Errorf("[%s] monthly_budget は正の値: %s", tr.ID, budget)
		}

		symbols, err := cleanSymbols(tr.Symbols)
		if err != nil {
			return nil, fmt.Errorf("[%s] %w", tr.ID, err)
		}

		win, err := window.Parse(tr.Window)
		if err != nil {
			return nil, fmt.Errorf("[%s] %w", tr.ID, err)
		}

		// multipliers は戦略によって形が違う。
		//   stack_ladder    … { "3" = 1.5, "5" = 2.0 } のテーブル（弱気スコア → 倍率）
		//   drawdown_ladder … [2.0, 3.0, 4.0] の配列（levels と対で並ぶ）
		var mults map[int]float64
		var values []float64
		switch tr.Tactic {
		case "stack_ladder":
			mults, err = toIntFloatMap(tr.Multipliers, "multipliers")
			if err != nil {
				return nil, fmt.Errorf("[%s] %w", tr.ID, err)
			}
		case "drawdown_ladder":
			// values は multipliers の別名。設定では multipliers を使う。
			raw := tr.Multipliers
			if raw == nil {
				raw = tr.Values
			}
			values, err = toFloatSlice(raw, "multipliers")
			if err != nil {
				return nil, fmt.Errorf("[%s] %w", tr.ID, err)
			}
		}

		levels, err := toFloatSlice(tr.Levels, "levels")
		if err != nil {
			return nil, fmt.Errorf("[%s] %w", tr.ID, err)
		}

		// require_downtrend の既定は false（Python 版と同じ）。
		requireDown := false
		if tr.RequireDown != nil {
			requireDown = *tr.RequireDown
		}

		entries = append(entries, TacticEntry{
			ID:            tr.ID,
			Tactic:        tr.Tactic,
			Symbols:       symbols,
			Enabled:       tr.Enabled,
			Market:        tr.Market,
			SignalSymbol:  tr.SignalSymbol,
			SignalMarket:  tr.SignalMarket,
			MonthlyBudget: budget,
			Multiplier:    tr.Multiplier,
			Fast:          tr.Fast,
			Mid:           tr.Mid,
			Slow:          tr.Slow,
			Multipliers:   mults,
			Levels:        levels,
			Values:        values,
			RequireDown:   requireDown,
			Window:        win,
		})
	}

	baskets, err := parseBaskets(raw.Baskets, defaultBudget)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg := &AccumConfig{
		MonthlyBudget: defaultBudget,
		KillSwitch:    raw.KillSwitch,
		DataProvider:  raw.DataProvider,
		Execution:     raw.Execution,
		Tactics:       entries,
		Baskets:       baskets,
	}

	if cfg.Execution.Broker == "" {
		cfg.Execution.Broker = "tachibana"
	}
	if cfg.Execution.OrderType == "" {
		cfg.Execution.OrderType = "limit"
	}
	if cfg.Execution.TaxAccountType == "" {
		cfg.Execution.TaxAccountType = string(domain.TaxAccountSpecific)
	}
	if cfg.Execution.MaxStaleDays == 0 {
		cfg.Execution.MaxStaleDays = 6
	}
	if err := cfg.Execution.Validate(); err != nil {
		return nil, fmt.Errorf("%s の execution: %w", path, err)
	}

	// 戦略が組み立てられることをここで確かめる。未知の戦略名や矛盾した
	// パラメータを、発注直前ではなく設定読み込みの時点で弾く。
	for i := range cfg.Tactics {
		if _, err := cfg.Tactics[i].Build(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	if err := cfg.ValidateAssignment(false); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return cfg, nil
}

// Active は有効な戦略だけを返す。
func (c *AccumConfig) Active() []TacticEntry {
	var out []TacticEntry
	for _, t := range c.Tactics {
		if t.IsEnabled() {
			out = append(out, t)
		}
	}
	return out
}

// ActiveBaskets は有効なバスケットだけを返す。
func (c *AccumConfig) ActiveBaskets() []BasketEntry {
	var out []BasketEntry
	for _, b := range c.Baskets {
		if b.IsEnabled() {
			out = append(out, b)
		}
	}
	return out
}

// parseBaskets は [[baskets]] を読み、既定値を埋めて組み立てを検証する。
func parseBaskets(raws []BasketEntryRaw, defaultBudget decimal.Decimal) ([]BasketEntry, error) {
	var out []BasketEntry
	for _, br := range raws {
		budget := parseDecimal(br.MonthlyBudget)
		if budget.IsZero() {
			budget = defaultBudget
		}
		if !budget.IsPositive() {
			return nil, fmt.Errorf("[%s] monthly_budget は正の値: %s", br.ID, budget)
		}
		for symbol, weight := range br.Weights {
			if weight <= 0 {
				return nil, fmt.Errorf("[%s] 比率は正の値: %s=%v", br.ID, symbol, weight)
			}
		}

		tacticName := br.Tactic
		if tacticName == "" {
			tacticName = "constant"
		}
		lookback := br.TiltLookback
		if lookback == 0 {
			lookback = 252
		}
		// 基準銘柄は省略時 TOPIX 連動 ETF。比較の相手が無いと XIRR の良し悪しを
		// 判断できないため、明示的に空文字を書いたときだけ「基準なし」にする
		benchmark := "1306.T"
		if br.Benchmark != nil {
			benchmark = *br.Benchmark
		}

		var mults map[int]float64
		var values []float64
		var err error
		switch tacticName {
		case "stack_ladder":
			mults, err = toIntFloatMap(br.Multipliers, "multipliers")
		case "drawdown_ladder":
			rawValues := br.Multipliers
			if rawValues == nil {
				rawValues = br.Values
			}
			values, err = toFloatSlice(rawValues, "multipliers")
		}
		if err != nil {
			return nil, fmt.Errorf("[%s] %w", br.ID, err)
		}
		levels, err := toFloatSlice(br.Levels, "levels")
		if err != nil {
			return nil, fmt.Errorf("[%s] %w", br.ID, err)
		}
		requireDown := false
		if br.RequireDown != nil {
			requireDown = *br.RequireDown
		}

		entry := BasketEntry{
			ID:            br.ID,
			Weights:       br.Weights,
			Benchmark:     benchmark,
			MonthlyBudget: budget,
			Tactic:        tacticName,
			TiltStrength:  br.TiltStrength,
			TiltLookback:  lookback,
			Enabled:       br.Enabled,
			Market:        br.Market,
			params: TacticEntry{
				ID:          br.ID,
				Tactic:      tacticName,
				Multiplier:  br.Multiplier,
				Fast:        br.Fast,
				Mid:         br.Mid,
				Slow:        br.Slow,
				Multipliers: mults,
				Levels:      levels,
				Values:      values,
				RequireDown: requireDown,
			},
		}
		// 未知の戦略名や矛盾したパラメータを、検証を走らせる時点ではなく
		// 設定読み込みの時点で弾く
		if _, err := entry.BuildTactic(); err != nil {
			return nil, err
		}
		if _, err := entry.BuildTilt(); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// ValidateAssignment は id の重複と、1銘柄への複数割り当てを検出する。
//
// 同じ銘柄が複数の有効な戦略に現れると、それぞれが独立に予算を消化して
// 二重買付になる。allowOverlap は比較検証のときだけ真にする。
func (c *AccumConfig) ValidateAssignment(allowOverlap bool) error {
	// id は戦略とバスケットで共通の名前空間。比較表の行名になるので、
	// どちらかと重なると結果がどの設定のものか分からなくなる
	seen := map[string]int{}
	for _, t := range c.Tactics {
		seen[t.ID]++
	}
	for _, b := range c.Baskets {
		seen[b.ID]++
	}
	var dup []string
	for id, n := range seen {
		if n > 1 {
			dup = append(dup, id)
		}
	}
	if len(dup) > 0 {
		sort.Strings(dup)
		return fmt.Errorf("id が重複しています: %v", dup)
	}

	if allowOverlap {
		return nil
	}

	owners := map[string][]string{}
	for _, entry := range c.Active() {
		for _, symbol := range entry.Symbols {
			owners[symbol] = append(owners[symbol], entry.ID)
		}
	}
	var conflicts []string
	for symbol, ids := range owners {
		if len(ids) > 1 {
			conflicts = append(conflicts, fmt.Sprintf("%s → %v", symbol, ids))
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return fmt.Errorf("1銘柄に複数の戦略が割り当てられています（二重買付になります）: %s",
			strings.Join(conflicts, "、"))
	}
	return nil
}

// cleanSymbols は空白を除き、空と重複を弾く。
func cleanSymbols(raw []string) ([]string, error) {
	var cleaned []string
	for _, s := range raw {
		if t := strings.TrimSpace(s); t != "" {
			cleaned = append(cleaned, t)
		}
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("symbols が空です")
	}
	count := map[string]int{}
	for _, s := range cleaned {
		count[s]++
	}
	var dup []string
	for s, n := range count {
		if n > 1 {
			dup = append(dup, s)
		}
	}
	if len(dup) > 0 {
		sort.Strings(dup)
		return nil, fmt.Errorf("同じ銘柄が重複しています: %v", dup)
	}
	return cleaned, nil
}

// toFloat は TOML から来た数値を float64 にする。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

// toFloatSlice は TOML の配列を []float64 にする。
func toFloatSlice(v any, field string) ([]float64, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s は数値の配列で指定してください: %v", field, v)
	}
	out := make([]float64, 0, len(arr))
	for _, item := range arr {
		f, ok := toFloat(item)
		if !ok {
			return nil, fmt.Errorf("%s に数値でない要素があります: %v", field, item)
		}
		out = append(out, f)
	}
	return out, nil
}

// toIntFloatMap は TOML のテーブルを map[int]float64 にする。
//
// TOML の鍵は文字列なので multipliers = { "3" = 1.5, "5" = 2.0 } の形で書く。
func toIntFloatMap(v any, field string) (map[int]float64, error) {
	if v == nil {
		return nil, nil
	}
	tbl, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(`%s は { "3" = 1.5 } の形のテーブルで指定してください: %v`, field, v)
	}
	out := make(map[int]float64, len(tbl))
	for k, raw := range tbl {
		key, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Errorf("%s の鍵は整数の文字列で指定してください: %q", field, k)
		}
		f, ok := toFloat(raw)
		if !ok {
			return nil, fmt.Errorf("%s[%s] が数値ではありません: %v", field, k, raw)
		}
		if f < 1.0 {
			return nil, fmt.Errorf("%s[%s] は 1.0 以上で指定してください（減額はしない）: %v", field, k, f)
		}
		out[key] = f
	}
	return out, nil
}
