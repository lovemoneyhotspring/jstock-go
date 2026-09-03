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
	Market           string         `toml:"market"`
	Symbols          []string       `toml:"symbols"`
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
}

type SettingsFile struct {
	Universe  UniverseConfig
	Risk      RiskConfig
	Sizing    SizingConfig
	Execution ExecutionConfig
}

type StrategyEntryRaw struct {
	Name    string         `toml:"name"`
	Enabled *bool          `toml:"enabled"`
	Weight  float64        `toml:"weight"`
	Fast    int            `toml:"fast"`
	Slow    int            `toml:"slow"`
	Period  int            `toml:"period"`
	Params  map[string]any `toml:",inline"`
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

	return &SettingsFile{
		Universe:  raw.Universe,
		Risk:      risk,
		Sizing:    sizing,
		Execution: raw.Execution,
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
