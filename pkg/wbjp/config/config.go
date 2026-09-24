package config

import (
	"bytes"
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

// parseDec は数値（整数・小数・文字列表記）の項目を読む。書いていなければ既定値。
//
// 読めない値を黙って既定値にすると、「max_daily_loss = "10万"」が 100000 のまま
// 効いているつもりになる。書式不正はエラーにする（daytrade の設定と同じ厳しさ）。
func parseDec(name string, v any, defaultVal decimal.Decimal) (decimal.Decimal, error) {
	d, err := parseOptDec(name, v)
	if err != nil {
		return decimal.Zero, err
	}
	if d == nil {
		return defaultVal, nil
	}
	return *d, nil
}

// parseStrDec は文字列で書く数値の項目を読む。空（未記入）なら既定値、読めなければエラー。
func parseStrDec(name, s string, defaultVal decimal.Decimal) (decimal.Decimal, error) {
	if s == "" {
		return defaultVal, nil
	}
	d, err := decimal.NewFromString(strings.ReplaceAll(s, "_", ""))
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s は数値として読めません: %q", name, s)
	}
	return d, nil
}

// decodeStrict は未知のキーを弾いて TOML を読む。
//
// 未知のキーを黙って無視すると、「kil_switch = true」のような綴りの誤りで緊急停止が
// 効かないまま本番に乗る。daytrade の設定（pkg/daytrade/config）と同じく読んだ時点で止める。
func decodeStrict(data []byte, dst any) error {
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
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
		CashYieldSymbol: raw.CashYieldSymbol,
	}
	for _, item := range []struct {
		name string
		raw  any
		def  decimal.Decimal
		dst  *decimal.Decimal
	}{
		{"regime.exposure_bull", raw.ExposureBull, decimal.NewFromInt(1), &cfg.ExposureBull},
		{"regime.exposure_caution", raw.ExposureCaution, decimal.RequireFromString("0.5"), &cfg.ExposureCaution},
		{"regime.exposure_bear", raw.ExposureBear, decimal.Zero, &cfg.ExposureBear},
	} {
		v, err := parseDec(item.name, item.raw, item.def)
		if err != nil {
			return cfg, err
		}
		*item.dst = v
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

// validateRisk は [risk] の範囲を確かめる。0 や負の上限は「上限なし」ではなく書き誤り。
func validateRisk(r RiskConfig) error {
	for name, v := range map[string]decimal.Decimal{
		"max_order_value":     r.MaxOrderValue,
		"max_daily_loss":      r.MaxDailyLoss,
		"max_position_weight": r.MaxPositionWeight,
		"max_gross_exposure":  r.MaxGrossExposure,
	} {
		if v.LessThanOrEqual(decimal.Zero) {
			return fmt.Errorf("risk.%s は正の数: %s", name, v)
		}
	}
	if r.MaxPreviewDeviation.IsNegative() {
		return fmt.Errorf("risk.max_preview_deviation は 0 以上: %s", r.MaxPreviewDeviation)
	}
	// 0 は「1 件も出さない」と同じで、損切りも含め全注文が止まる（書き忘れも 0 になる）
	if r.MaxOrdersPerDay <= 0 {
		return fmt.Errorf("risk.max_orders_per_day は 1 以上（0 だと損切りも含め全注文が止まる）: %d", r.MaxOrdersPerDay)
	}
	return nil
}

// validateExecution は [execution] の値を確かめる。
//
// order_type の綴りを誤ると黙って指値になり、limit_offset が読めないと黙って 0.005 に
// なっていた。どちらも「設定したつもりで効いていない」なので止める。
func validateExecution(e ExecutionConfig) error {
	switch e.OrderType {
	case "", "limit", "market":
	default:
		return fmt.Errorf("execution.order_type は limit / market: %q", e.OrderType)
	}
	if e.LimitOffset != "" {
		d, err := parseStrDec("execution.limit_offset", e.LimitOffset, decimal.Zero)
		if err != nil {
			return err
		}
		if d.IsNegative() || d.GreaterThanOrEqual(decimal.NewFromInt(1)) {
			return fmt.Errorf("execution.limit_offset は 0 以上 1 未満: %s", d)
		}
	}
	switch e.TaxAccountType {
	case "", "GENERAL", "SPECIFIC", "NISA":
	default:
		return fmt.Errorf("execution.tax_account_type は GENERAL / SPECIFIC / NISA: %q", e.TaxAccountType)
	}
	return nil
}

// LoadSettingsFile は settings.toml を読み込む。
//
// 未知のキー・読めない数値はエラーにする（decodeStrict・parseDec）。
func LoadSettingsFile(configDir string) (*SettingsFile, error) {
	path := filepath.Join(configDir, "settings.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	var raw SettingsFileRaw
	if err := decodeStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse TOML %s: %w", path, err)
	}

	risk := RiskConfig{
		KillSwitch:      raw.Risk.KillSwitch,
		MaxOrdersPerDay: raw.Risk.MaxOrdersPerDay,
	}
	sizing := SizingConfig{
		Method:       raw.Sizing.Method,
		MaxPositions: raw.Sizing.MaxPositions,
	}
	var numErr error
	num := func(name string, v any, def string, dst *decimal.Decimal) {
		if numErr != nil {
			return
		}
		*dst, numErr = parseDec(name, v, decimal.RequireFromString(def))
	}
	str := func(name, v, def string, dst *decimal.Decimal) {
		if numErr != nil {
			return
		}
		*dst, numErr = parseStrDec(name, v, decimal.RequireFromString(def))
	}
	num("risk.max_order_value", raw.Risk.MaxOrderValue, "500000", &risk.MaxOrderValue)
	num("risk.max_daily_loss", raw.Risk.MaxDailyLoss, "100000", &risk.MaxDailyLoss)
	str("risk.max_position_weight", raw.Risk.MaxPositionWeight, "0.25", &risk.MaxPositionWeight)
	str("risk.max_gross_exposure", raw.Risk.MaxGrossExposure, "0.90", &risk.MaxGrossExposure)
	str("risk.max_preview_deviation", raw.Risk.MaxPreviewDeviation, "0.02", &risk.MaxPreviewDeviation)
	str("sizing.risk_per_trade", raw.Sizing.RiskPerTrade, "0.01", &sizing.RiskPerTrade)
	str("sizing.atr_stop_multiple", raw.Sizing.ATRStopMultiple, "2.0", &sizing.ATRStopMultiple)
	num("sizing.fixed_notional", raw.Sizing.FixedNotional, "300000", &sizing.FixedNotional)
	if numErr != nil {
		return nil, fmt.Errorf("%s: %w", path, numErr)
	}
	if err := validateRisk(risk); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if sizing.Method == "" {
		sizing.Method = "atr_risk"
	}
	if sizing.MaxPositions <= 0 {
		sizing.MaxPositions = 5
	}
	if err := validateExecution(raw.Execution); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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

	// 最上位のキーは厳しく読む。[[strategies]] の中は戦略ごとにパラメータが違うので
	// ここでは map で受け、未知のパラメータは戦略の登録簿（strategy.Create）が弾く
	var top struct {
		Combiner       string           `toml:"combiner"`
		EntryThreshold float64          `toml:"entry_threshold"`
		ExitThreshold  float64          `toml:"exit_threshold"`
		Strategies     []map[string]any `toml:"strategies"`
	}
	if err := decodeStrict(data, &top); err != nil {
		return nil, fmt.Errorf("failed to parse TOML %s: %w", path, err)
	}
	var cfg StrategiesConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse TOML %s: %w", path, err)
	}
	for i, s := range cfg.Strategies {
		if s.Name == "" {
			return nil, fmt.Errorf("%s: strategies[%d] に name がありません", path, i)
		}
	}

	switch cfg.Combiner {
	case "":
		cfg.Combiner = "weighted_vote"
	case "weighted_vote", "majority", "veto", "priority":
	default:
		// 綴りを誤ると黙って weighted_vote になっていた（strategy.GetCombinerByName）
		return nil, fmt.Errorf("%s: combiner は weighted_vote / majority / veto / priority: %q", path, cfg.Combiner)
	}
	if cfg.EntryThreshold == 0 {
		cfg.EntryThreshold = 0.3
	}
	if cfg.ExitThreshold == 0 {
		cfg.ExitThreshold = 0.1
	}

	return &cfg, nil
}
