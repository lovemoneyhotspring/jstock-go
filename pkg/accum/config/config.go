package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/pelletier/go-toml/v2"
	"github.com/shopspring/decimal"
)

type ExecutionConfig struct {
	Broker           string         `toml:"broker"`
	OrderType        string         `toml:"order_type"`
	LimitOffset      string         `toml:"limit_offset"`
	FallbackToLimit  bool           `toml:"fallback_to_limit"`
	TaxAccountType   string         `toml:"tax_account_type"`
	LotSizeOverrides map[string]int `toml:"lot_size_overrides"`
}

type TacticEntryRaw struct {
	ID            string         `toml:"id"`
	Tactic        string         `toml:"tactic"`
	Symbols       []string       `toml:"symbols"`
	Enabled       *bool          `toml:"enabled"`
	Market        string         `toml:"market"`
	SignalSymbol  string         `toml:"signal_symbol"`
	SignalMarket  string         `toml:"signal_market"`
	MonthlyBudget any            `toml:"monthly_budget"`
	Multiplier    float64        `toml:"multiplier"`
	Fast          int            `toml:"fast"`
	Mid           int            `toml:"mid"`
	Slow          int            `toml:"slow"`
	Multipliers   any            `toml:"multipliers"`
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
	Multipliers   any
}

func (e *TacticEntry) IsEnabled() bool {
	if e.Enabled == nil {
		return true
	}
	return *e.Enabled
}

type AccumConfigRaw struct {
	MonthlyBudget any             `toml:"monthly_budget"`
	KillSwitch    bool            `toml:"kill_switch"`
	DataProvider  string          `toml:"data_provider"`
	Execution     ExecutionConfig `toml:"execution"`
	Tactics       []TacticEntryRaw `toml:"tactics"`
}

type AccumConfig struct {
	MonthlyBudget decimal.Decimal
	KillSwitch    bool
	DataProvider  string
	Execution     ExecutionConfig
	Tactics       []TacticEntry
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

	var tactics []TacticEntry
	for _, tr := range raw.Tactics {
		budget := parseDecimal(tr.MonthlyBudget)
		if budget.IsZero() {
			budget = defaultBudget
		}

		tactics = append(tactics, TacticEntry{
			ID:            tr.ID,
			Tactic:        tr.Tactic,
			Symbols:       tr.Symbols,
			Enabled:       tr.Enabled,
			Market:        tr.Market,
			SignalSymbol:  tr.SignalSymbol,
			SignalMarket:  tr.SignalMarket,
			MonthlyBudget: budget,
			Multiplier:    tr.Multiplier,
			Fast:          tr.Fast,
			Mid:           tr.Mid,
			Slow:          tr.Slow,
			Multipliers:   tr.Multipliers,
		})
	}

	cfg := &AccumConfig{
		MonthlyBudget: defaultBudget,
		KillSwitch:    raw.KillSwitch,
		DataProvider:  raw.DataProvider,
		Execution:     raw.Execution,
		Tactics:       tactics,
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

	return cfg, nil
}
