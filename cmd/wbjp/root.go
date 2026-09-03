package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/pelletier/go-toml/v2"
)

var (
	appSettings   = settings.LoadAppSettings()
	configDirFlag string
)

// buildStrategies は設定ファイルの記述から戦略を組み立てる。
//
// 登録簿（strategy.Registry）を引くので、戦略を足すときにここを触る必要はない。
// TOML に書いたパラメータはそのまま各戦略へ渡る。綴りを間違えたキーは
// 生成時にエラーになる（黙って既定値で動くのが一番たちが悪い）。
func buildStrategies(stratCfg *wbjpcfg.StrategiesConfig) ([]strategy.Strategy, map[string]float64, error) {
	rawEntries, err := loadStrategyParams(configDirFlag)
	if err != nil {
		return nil, nil, err
	}

	var strats []strategy.Strategy
	weights := make(map[string]float64)

	for i, se := range stratCfg.Strategies {
		if !se.IsEnabled() {
			continue
		}

		// 生の TOML から素性のパラメータを取り出す。config の構造体は
		// name / enabled / weight しか受けておらず、戦略ごとの設定は
		// ここでしか拾えない。
		var raw map[string]any
		if i < len(rawEntries) {
			raw = rawEntries[i]
		}

		// 重みの既定は 1.0。書き忘れが 0.0（＝合成で無視）になると、
		// 有効にしたはずの戦略が黙って効かなくなる。
		weight := se.Weight
		if _, ok := raw["weight"]; !ok {
			weight = 1.0
		}
		weights[se.Name] = weight

		params := make(map[string]any, len(raw))
		for k, v := range raw {
			switch k {
			case "name", "enabled", "weight":
				continue // 戦略の設定ではなく、束ね方の設定
			}
			params[k] = v
		}

		s, err := strategy.Create(se.Name, params)
		if err != nil {
			return nil, nil, fmt.Errorf("戦略 %q を作れません: %w", se.Name, err)
		}
		strats = append(strats, s)
	}
	return strats, weights, nil
}

// loadStrategyParams は strategies.toml の [[strategies]] を生のまま読む。
//
// config の StrategyEntryRaw は `toml:",inline"` で全キーを拾おうとしているが、
// go-toml/v2 はそれを解釈しないため Params は常に空になる。設定を確実に
// 反映するには、ここでもう一度素の map として読むしかない。
func loadStrategyParams(configDir string) ([]map[string]any, error) {
	path := filepath.Join(configDir, "strategies.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s を読めません: %w", path, err)
	}
	var doc struct {
		Strategies []map[string]any `toml:"strategies"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s を解釈できません: %w", path, err)
	}
	return doc.Strategies, nil
}
