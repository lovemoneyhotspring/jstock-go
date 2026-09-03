package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/shopspring/decimal"
)

type UniverseConfig struct {
	Market string `toml:"market"`
	// DataProvider は足データの取得元。空なら市場の既定（JP は jquants）。
	DataProvider string   `toml:"data_provider"`
	Symbols      []string `toml:"symbols"`
	// SymbolsFile は銘柄リストのファイル（1行1銘柄、# はコメント）。設定
	// ディレクトリからの相対パス。読み込んだ銘柄は Symbols に合流し、
	// allowlist としても働く。
	SymbolsFile      string         `toml:"symbols_file"`
	TOPIX500Symbols  []string       `toml:"topix500_symbols"`
	LotSizeOverrides map[string]int `toml:"lot_size_overrides"`
}

type RiskConfigRaw struct {
	KillSwitch          bool   `toml:"kill_switch"`
	MaxOrderValue       any    `toml:"max_order_value"`
	MaxOrdersPerDay     int    `toml:"max_orders_per_day"`
	MaxDailyLoss        any    `toml:"max_daily_loss"`
	MaxPositionWeight   string `toml:"max_position_weight"`
	MaxGrossExposure    string `toml:"max_gross_exposure"`
	MaxPreviewDeviation string `toml:"max_preview_deviation"`
}

type RiskConfig struct {
	KillSwitch          bool
	MaxOrderValue       decimal.Decimal
	MaxOrdersPerDay     int
	MaxDailyLoss        decimal.Decimal
	MaxPositionWeight   decimal.Decimal
	MaxGrossExposure    decimal.Decimal
	MaxPreviewDeviation decimal.Decimal
}

type SizingConfigRaw struct {
	Method          string `toml:"method"`
	RiskPerTrade    string `toml:"risk_per_trade"`
	ATRStopMultiple string `toml:"atr_stop_multiple"`
	FixedNotional   any    `toml:"fixed_notional"`
	MaxPositions    int    `toml:"max_positions"`
}

type SizingConfig struct {
	Method          string
	RiskPerTrade    decimal.Decimal
	ATRStopMultiple decimal.Decimal
	FixedNotional   decimal.Decimal
	MaxPositions    int
}

// StopsConfigRaw は [stops] セクションの生の値。TOML では小数を数値でも
// 文字列でも書けるため any で受け、decimal へ変換する。
type StopsConfigRaw struct {
	Trailing            bool   `toml:"trailing"`
	BreakevenAfterR     any    `toml:"breakeven_after_r"`
	StaleExitDays       *int   `toml:"stale_exit_days"`
	MaxHoldDays         *int   `toml:"max_hold_days"`
	InitialStopPct      any    `toml:"initial_stop_pct"`
	TakeProfitR         any    `toml:"take_profit_r"`
	TakeProfitFraction  any    `toml:"take_profit_fraction"`
	TrendExitSMA        *int   `toml:"trend_exit_sma"`
	TrendExitAlways     bool   `toml:"trend_exit_always"`
	TrendExitKind       string `toml:"trend_exit_kind"`
	TrailingATRMultiple any    `toml:"trailing_atr_multiple"`
	TrailingPct         any    `toml:"trailing_pct"`
}

// StopsConfig は損切り・利確の動かし方。ストップ価格そのものは
// risk.StopBook が持つ。初期ストップ幅は InitialStopPct が無ければ
// sizing.atr_stop_multiple（= 1R）で決まる。
//
// 「無効」と「0」を区別する必要があるため、任意項目はポインタで持つ。
// 0 を既定値にすると「breakeven_after_r = 0」（建てた瞬間に建値ストップ）と
// 「未設定」が同じ意味になってしまう。
type StopsConfig struct {
	// Trailing は ATR トレーリングストップを使うか（上げるだけ、下げない）。
	Trailing bool
	// BreakevenAfterR は含み益が初期リスクの何倍でストップを建値へ上げるか。nil で無効。
	BreakevenAfterR *decimal.Decimal
	// StaleExitDays は建ててから何営業日で含み益ゼロ以下なら手仕舞うか。nil で無効。
	StaleExitDays *int
	// MaxHoldDays は最大保有営業日数。nil で無効。
	MaxHoldDays *int
	// InitialStopPct は初期ストップ幅を建値からの比率で固定する（0.04 = -4%）。
	// nil なら ATR ベース。
	InitialStopPct *decimal.Decimal
	// TakeProfitR は 2 段階利確の 1 段目を発動する R 倍。nil で無効。
	TakeProfitR *decimal.Decimal
	// TakeProfitFraction は 1 段目で手仕舞う比率（既定 0.5）。
	TakeProfitFraction decimal.Decimal
	// TrendExitSMA は残り玉を手仕舞う移動平均の期間。nil で無効。
	TrendExitSMA *int
	// TrendExitAlways は利確前の建玉にも TrendExitSMA 割れを適用するか。
	TrendExitAlways bool
	// TrendExitKind は線の種類: sma / ema / donchian。
	TrendExitKind string
	// TrailingATRMultiple はトレーリング時の幅（ATR 倍率）。nil なら初期ストップと同じ。
	TrailingATRMultiple *decimal.Decimal
	// TrailingPct は %トレーリング（0.08 = 最高終値から -8%）。設定すると ATR 追従の代わりに使う。
	TrailingPct *decimal.Decimal
}

// RegimeConfigRaw は [regime] セクションの生の値。
type RegimeConfigRaw struct {
	Enabled         bool   `toml:"enabled"`
	Benchmark       string `toml:"benchmark"`
	SMALong         int    `toml:"sma_long"`
	SMAMid          int    `toml:"sma_mid"`
	SlopeLookback   int    `toml:"slope_lookback"`
	ExposureBull    any    `toml:"exposure_bull"`
	ExposureCaution any    `toml:"exposure_caution"`
	ExposureBear    any    `toml:"exposure_bear"`
	CashYieldSymbol string `toml:"cash_yield_symbol"`
}

// RegimeConfig は相場レジーム（指数の環境認識）による露出の制御。
//
// 買い持ちの弱点は暴落時に全額被弾すること。指数の位置で 3 段階に分け、
// 弱気では全建玉を手仕舞って現金に退避する。
//
//   - 強気: 終値 > SMA長期 かつ 長期線が上向き
//   - 警戒: 終値 > SMA長期 だが 終値 < SMA中期（または長期線が下向き）
//   - 弱気: 終値 < SMA長期（露出 0 なら全手仕舞い）
type RegimeConfig struct {
	Enabled   bool
	Benchmark string
	SMALong   int
	SMAMid    int
	// SlopeLookback は長期線の傾きを測る日数。
	SlopeLookback   int
	ExposureBull    decimal.Decimal
	ExposureCaution decimal.Decimal
	ExposureBear    decimal.Decimal
	// CashYieldSymbol は待機資金の利回りに使う系列。空で無利息。
	CashYieldSymbol string
}

type ExecutionConfig struct {
	Broker         string `toml:"broker"`
	TaxAccountType string `toml:"tax_account_type"`
	OrderType      string `toml:"order_type"`
	LimitOffset    string `toml:"limit_offset"`
}

type SettingsFileRaw struct {
	Universe  UniverseConfig  `toml:"universe"`
	Risk      RiskConfigRaw   `toml:"risk"`
	Sizing    SizingConfigRaw `toml:"sizing"`
	Execution ExecutionConfig `toml:"execution"`
	Stops     StopsConfigRaw  `toml:"stops"`
	Regime    RegimeConfigRaw `toml:"regime"`
}

type SettingsFile struct {
	Universe  UniverseConfig
	Risk      RiskConfig
	Sizing    SizingConfig
	Execution ExecutionConfig
	Stops     StopsConfig
	Regime    RegimeConfig
}

// StrategyEntryRaw は [[strategies]] の共通項目。
//
// 戦略ごとの固有パラメータはここに載せない。go-toml/v2 は `toml:",inline"`
// を解釈しないため、残りのキーを map で受ける書き方は黙って空を返す
// （＝設定が一切届かない）。固有パラメータは cmd/wbjp/root.go の
// loadStrategyParams が生の map として読み、登録簿に渡している。
type StrategyEntryRaw struct {
	Name    string  `toml:"name"`
	Enabled *bool   `toml:"enabled"`
	Weight  float64 `toml:"weight"`
	Fast    int     `toml:"fast"`
	Slow    int     `toml:"slow"`
	Period  int     `toml:"period"`
}

func (e *StrategyEntryRaw) IsEnabled() bool {
	if e.Enabled == nil {
		return true
	}
	return *e.Enabled
}

type StrategiesConfig struct {
	Combiner       string             `toml:"combiner"`
	EntryThreshold float64            `toml:"entry_threshold"`
	ExitThreshold  float64            `toml:"exit_threshold"`
	Strategies     []StrategyEntryRaw `toml:"strategies"`
}

func parseDec(v any, defaultVal decimal.Decimal) decimal.Decimal {
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
	return defaultVal
}

func parseStrDec(s string, defaultVal decimal.Decimal) decimal.Decimal {
	if s == "" {
		return defaultVal
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return defaultVal
	}
	return d
}

// parseOptDec は任意項目の decimal を読む。TOML に無ければ nil。
//
// 「未設定」と「0」を区別するためにポインタを返す。既定値で埋めてしまうと
// 設定していない機能が黙って動き出す。書式不正は黙って無効化せずエラーに
// する（設定した気でいるのに効いていない、が一番危ない）。
func parseOptDec(name string, v any) (*decimal.Decimal, error) {
	if v == nil {
		return nil, nil
	}
	var d decimal.Decimal
	switch val := v.(type) {
	case int64:
		d = decimal.NewFromInt(val)
	case int:
		d = decimal.NewFromInt(int64(val))
	case float64:
		d = decimal.NewFromFloat(val)
	case string:
		parsed, err := decimal.NewFromString(strings.ReplaceAll(val, "_", ""))
		if err != nil {
			return nil, fmt.Errorf("%s は数値として読めません: %q", name, val)
		}
		d = parsed
	default:
		return nil, fmt.Errorf("%s は数値または文字列で書いてください: %v", name, v)
	}
	return &d, nil
}

// ReadSymbolsFile は銘柄リストを読む。1行1銘柄、# 以降はコメント、重複は最初の1つ。
//
// ファイルが無いときに黙って空を返すと「対象銘柄ゼロ」で静かに何も
// 起きなくなるため、必ずエラーにする。
func ReadSymbolsFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("銘柄リストが読めません %s: %w", path, err)
	}
	var symbols []string
	seen := make(map[string]struct{})
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		if line == "" {
			continue
		}
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		symbols = append(symbols, line)
	}
	return symbols, nil
}

// buildStops は [stops] を検証しつつ既定値を埋める。
func buildStops(raw StopsConfigRaw) (StopsConfig, error) {
	cfg := StopsConfig{
		Trailing:           raw.Trailing,
		StaleExitDays:      raw.StaleExitDays,
		MaxHoldDays:        raw.MaxHoldDays,
		TakeProfitFraction: decimal.RequireFromString("0.5"),
		TrendExitSMA:       raw.TrendExitSMA,
		TrendExitAlways:    raw.TrendExitAlways,
		TrendExitKind:      raw.TrendExitKind,
	}
	for _, item := range []struct {
		name string
		raw  any
		dst  **decimal.Decimal
	}{
		{"stops.breakeven_after_r", raw.BreakevenAfterR, &cfg.BreakevenAfterR},
		{"stops.initial_stop_pct", raw.InitialStopPct, &cfg.InitialStopPct},
		{"stops.take_profit_r", raw.TakeProfitR, &cfg.TakeProfitR},
		{"stops.trailing_atr_multiple", raw.TrailingATRMultiple, &cfg.TrailingATRMultiple},
		{"stops.trailing_pct", raw.TrailingPct, &cfg.TrailingPct},
	} {
		v, err := parseOptDec(item.name, item.raw)
		if err != nil {
			return cfg, err
		}
		*item.dst = v
	}
	f, err := parseOptDec("stops.take_profit_fraction", raw.TakeProfitFraction)
	if err != nil {
		return cfg, err
	}
	if f != nil {
		cfg.TakeProfitFraction = *f
	}
	if cfg.TrendExitKind == "" {
		cfg.TrendExitKind = "sma"
	}

	switch cfg.TrendExitKind {
	case "sma", "ema", "donchian":
	default:
		return cfg, fmt.Errorf("stops.trend_exit_kind は sma / ema / donchian: %s", cfg.TrendExitKind)
	}
	for name, v := range map[string]*decimal.Decimal{
		"breakeven_after_r": cfg.BreakevenAfterR,
		"take_profit_r":     cfg.TakeProfitR,
	} {
		if v != nil && v.LessThanOrEqual(decimal.Zero) {
			return cfg, fmt.Errorf("stops.%s は正の数: %s", name, v)
		}
	}
	for name, v := range map[string]*int{
		"stale_exit_days": cfg.StaleExitDays,
		"max_hold_days":   cfg.MaxHoldDays,
		"trend_exit_sma":  cfg.TrendExitSMA,
	} {
		if v != nil && *v <= 0 {
			return cfg, fmt.Errorf("stops.%s は正の整数: %d", name, *v)
		}
	}
	ratios := map[string]*decimal.Decimal{
		"initial_stop_pct":     cfg.InitialStopPct,
		"take_profit_fraction": &cfg.TakeProfitFraction,
		"trailing_pct":         cfg.TrailingPct,
	}
	for name, v := range ratios {
		if v == nil {
			continue
		}
		if v.LessThanOrEqual(decimal.Zero) || v.GreaterThanOrEqual(decimal.NewFromInt(1)) {
			return cfg, fmt.Errorf("stops.%s は 0 より大きく 1 未満: %s", name, v)
		}
	}
	// 時間切れ手仕舞いは最大保有期間より先に来ないと意味がない。
	if cfg.StaleExitDays != nil && cfg.MaxHoldDays != nil && *cfg.StaleExitDays > *cfg.MaxHoldDays {
		return cfg, fmt.Errorf("stops.stale_exit_days (%d) は max_hold_days (%d) 以下にしてください",
			*cfg.StaleExitDays, *cfg.MaxHoldDays)
	}
	return cfg, nil
}

// buildRegime は [regime] を検証しつつ既定値を埋める。
func buildRegime(raw RegimeConfigRaw) (RegimeConfig, error) {
	cfg := RegimeConfig{
		Enabled:         raw.Enabled,
		Benchmark:       raw.Benchmark,
		SMALong:         raw.SMALong,
		SMAMid:          raw.SMAMid,
		SlopeLookback:   raw.SlopeLookback,
		ExposureBull:    parseDec(raw.ExposureBull, decimal.NewFromInt(1)),
		ExposureCaution: parseDec(raw.ExposureCaution, decimal.RequireFromString("0.5")),
		ExposureBear:    parseDec(raw.ExposureBear, decimal.Zero),
		CashYieldSymbol: raw.CashYieldSymbol,
	}
	if cfg.Benchmark == "" {
		cfg.Benchmark = "SPY"
	}
	if cfg.SMALong <= 0 {
		cfg.SMALong = 200
	}
	if cfg.SMAMid <= 0 {
		cfg.SMAMid = 50
	}
	if cfg.SlopeLookback <= 0 {
		cfg.SlopeLookback = 20
	}

	one := decimal.NewFromInt(1)
	for name, v := range map[string]decimal.Decimal{
		"exposure_bull":    cfg.ExposureBull,
		"exposure_caution": cfg.ExposureCaution,
		"exposure_bear":    cfg.ExposureBear,
	} {
		if v.LessThan(decimal.Zero) || v.GreaterThan(one) {
			return cfg, fmt.Errorf("regime.%s は 0〜1: %s", name, v)
		}
	}
	if cfg.ExposureBear.GreaterThan(cfg.ExposureCaution) || cfg.ExposureCaution.GreaterThan(cfg.ExposureBull) {
		return cfg, fmt.Errorf("regime の露出は 弱気 ≤ 警戒 ≤ 強気 の順にしてください（%s / %s / %s）",
			cfg.ExposureBear, cfg.ExposureCaution, cfg.ExposureBull)
	}
	if cfg.SMAMid >= cfg.SMALong {
		return cfg, fmt.Errorf("regime.sma_mid (%d) は sma_long (%d) より短くしてください", cfg.SMAMid, cfg.SMALong)
	}
	return cfg, nil
}

// LoadSettingsFile は settings.toml を読み込む。
func LoadSettingsFile(configDir string) (*SettingsFile, error) {
	path := filepath.Join(configDir, "settings.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	var raw SettingsFileRaw
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse TOML %s: %w", path, err)
	}

	risk := RiskConfig{
		KillSwitch:          raw.Risk.KillSwitch,
		MaxOrderValue:       parseDec(raw.Risk.MaxOrderValue, decimal.NewFromInt(500000)),
		MaxOrdersPerDay:     raw.Risk.MaxOrdersPerDay,
		MaxDailyLoss:        parseDec(raw.Risk.MaxDailyLoss, decimal.NewFromInt(100000)),
		MaxPositionWeight:   parseStrDec(raw.Risk.MaxPositionWeight, decimal.RequireFromString("0.25")),
		MaxGrossExposure:    parseStrDec(raw.Risk.MaxGrossExposure, decimal.RequireFromString("0.90")),
		MaxPreviewDeviation: parseStrDec(raw.Risk.MaxPreviewDeviation, decimal.RequireFromString("0.02")),
	}

	sizing := SizingConfig{
		Method:          raw.Sizing.Method,
		RiskPerTrade:    parseStrDec(raw.Sizing.RiskPerTrade, decimal.RequireFromString("0.01")),
		ATRStopMultiple: parseStrDec(raw.Sizing.ATRStopMultiple, decimal.RequireFromString("2.0")),
		FixedNotional:   parseDec(raw.Sizing.FixedNotional, decimal.NewFromInt(300000)),
		MaxPositions:    raw.Sizing.MaxPositions,
	}
	if sizing.Method == "" {
		sizing.Method = "atr_risk"
	}
	if sizing.MaxPositions <= 0 {
		sizing.MaxPositions = 5
	}

	stops, err := buildStops(raw.Stops)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	regime, err := buildRegime(raw.Regime)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	universe := raw.Universe
	if universe.Market == "" {
		universe.Market = "JP"
	}
	if universe.DataProvider == "" {
		// 売買できるのは東証だけなので、既定は日本株のプロバイダ。
		universe.DataProvider = "jquants"
	}
	// symbols_file の銘柄は symbols に合流させる。allowlist は
	// universe.Symbols を見るので、ここで混ぜないとファイル指定の銘柄が
	// リスク判定で全部弾かれる。
	if universe.SymbolsFile != "" {
		listed, err := ReadSymbolsFile(filepath.Join(configDir, universe.SymbolsFile))
		if err != nil {
			return nil, err
		}
		existing := make(map[string]struct{}, len(universe.Symbols))
		for _, sym := range universe.Symbols {
			existing[sym] = struct{}{}
		}
		for _, sym := range listed {
			if _, dup := existing[sym]; !dup {
				universe.Symbols = append(universe.Symbols, sym)
				existing[sym] = struct{}{}
			}
		}
	}

	return &SettingsFile{
		Universe:  universe,
		Risk:      risk,
		Sizing:    sizing,
		Execution: raw.Execution,
		Stops:     stops,
		Regime:    regime,
	}, nil
}

// LoadStrategiesConfig は strategies.toml を読み込む。
func LoadStrategiesConfig(configDir string) (*StrategiesConfig, error) {
	path := filepath.Join(configDir, "strategies.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	var cfg StrategiesConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse TOML %s: %w", path, err)
	}

	if cfg.Combiner == "" {
		cfg.Combiner = "weighted_vote"
	}
	if cfg.EntryThreshold == 0 {
		cfg.EntryThreshold = 0.3
	}
	if cfg.ExitThreshold == 0 {
		cfg.ExitThreshold = 0.1
	}

	return &cfg, nil
}
