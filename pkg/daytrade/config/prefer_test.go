package config

import (
	"strings"
	"testing"
)

func TestValidatePrefer(t *testing.T) {
	ok := []Prefer{
		{},
		{Indicator: PreferStochRSI, Max: 0.2, Pool: 20},
		{Indicator: PreferRSI2, Max: 10, Pool: 1},
	}
	for _, p := range ok {
		if err := validatePrefer(p); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	bad := map[string]Prefer{
		"未知の指標":                {Indicator: "rsi14", Max: 30, Pool: 20},
		"stoch_rsi に rsi2 の尺度": {Indicator: PreferStochRSI, Max: 10, Pool: 20},
		"stoch_rsi の 0":        {Indicator: PreferStochRSI, Max: 0, Pool: 20},
		"rsi2 の 100":           {Indicator: PreferRSI2, Max: 100, Pool: 20},
		"範囲 0":                 {Indicator: PreferRSI2, Max: 10, Pool: 0},
	}
	for name, p := range bad {
		if err := validatePrefer(p); err == nil || !strings.Contains(err.Error(), "signal.prefer") {
			t.Errorf("%s を通した: %v", name, err)
		}
	}
}

// 本番の設定（信用版は extends で土台を継ぐ）は 2026-09-25 から記録だけ（優先を掛けない）。
// 有効にするときはこのテストの期待値も替える（うっかり有効・無効が入れ替わらないため）。
func TestRepoConfigPreferRecordOnly(t *testing.T) {
	for _, dir := range []string{"../../../config/daytrade", "../../../config/daytrade_margin"} {
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if cfg.Signal.Prefer.Enabled() {
			t.Errorf("%s: signal.prefer が有効になっている: %+v", dir, cfg.Signal.Prefer)
		}
		// 有効にしたときに日ごとの設定・gap_vol へ戻す設定で消えないこと
		p := Prefer{Indicator: PreferStochRSI, Max: 0.2, Pool: 20}
		cfg.Signal.Prefer = p
		if cfg.Signal.ForDay(true).Prefer != p || cfg.FallbackToGapVol().Signal.Prefer != p {
			t.Errorf("%s: 日ごとの設定で signal.prefer が落ちた", dir)
		}
	}
}
