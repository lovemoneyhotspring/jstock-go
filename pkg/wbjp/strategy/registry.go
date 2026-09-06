package strategy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/registry"
)

// Registry は設定ファイルの名前から戦略を引く登録簿。
//
// switch でハードコードすると、TOML に書いたパラメータが黙って無視される
// （名前は合っているのに既定値で動く、という最も気づきにくい事故になる）。
// 生成関数を登録しておき、パラメータは各戦略が自分で読む形にする。
var Registry = registry.New[Strategy]("戦略")

// Available は登録済みの戦略名（辞書順）。
func Available() []string { return Registry.Available() }

// Describe は名前と 1 行説明の一覧。CLI の strategies 表示に使う。
func Describe() []registry.Described { return Registry.Describe() }

// Create は名前とパラメータから戦略を作る。
func Create(name string, params map[string]any) (Strategy, error) {
	return Registry.Create(name, params)
}

func init() {
	Registry.MustRegister("sma_cross",
		"移動平均クロス（順張りトレンドフォロー）",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			fast := p.Int("fast", 25)
			slow := p.Int("slow", 75)
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewSMACross(fast, slow)
		})

	Registry.MustRegister("rsi_reversion",
		"RSI 逆張り（レンジ相場・買われすぎ売られすぎ反転）",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			period := p.Int("period", 14)
			oversold := p.Float("oversold", 30.0)
			overbought := p.Float("overbought", 70.0)
			adxPeriod := p.Int("adx_period", 14)
			maxADX := p.Float("max_adx", 40.0)
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewRSIReversion(period, oversold, overbought, adxPeriod, maxADX)
		})

	Registry.MustRegister("atr_breakout",
		"ドンチャンチャネル上抜け/下抜け ＋ ATR ボラティリティフィルタ",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			channel := p.Int("channel", 20)
			atrPeriod := p.Int("atr_period", 14)
			minATRRatio := p.Float("min_atr_ratio", 0.005)
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewATRBreakout(channel, atrPeriod, minATRRatio)
		})

	Registry.MustRegister("trend_pullback",
		"長期上昇トレンド ＋ 出来高枯渇押し目 ＋ 反発ブレイクアウト",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			o := DefaultTrendPullbackOptions()
			o.SMALong = p.Int("sma_long", o.SMALong)
			o.SMAMid = p.Int("sma_mid", o.SMAMid)
			o.SMAShort = p.Int("sma_short", o.SMAShort)
			o.SlopeLookback = p.Int("slope_lookback", o.SlopeLookback)
			o.RSLookback = p.Int("rs_lookback", o.RSLookback)
			o.HighLookback = p.Int("high_lookback", o.HighLookback)
			o.MaxDrawdownFromHigh = p.Float("max_drawdown_from_high", o.MaxDrawdownFromHigh)
			o.BreakoutLookback = p.Int("breakout_lookback", o.BreakoutLookback)
			o.VolumeLookback = p.Int("volume_lookback", o.VolumeLookback)
			o.VolumeDryupMax = p.Float("volume_dryup_max", o.VolumeDryupMax)
			o.ATRPeriod = p.Int("atr_period", o.ATRPeriod)
			o.MinATRRatio = p.Float("min_atr_ratio", o.MinATRRatio)
			o.MaxATRRatio = p.Float("max_atr_ratio", o.MaxATRRatio)
			o.MinDollarVolume = p.Float("min_dollar_volume", o.MinDollarVolume)
			o.Benchmark = p.String("benchmark", o.Benchmark)
			o.BenchmarkSMA = p.Int("benchmark_sma", o.BenchmarkSMA)
			o.BlackoutFile = p.String("blackout_file", "")
			o.BlackoutDaysBefore = p.Int("blackout_days_before", o.BlackoutDaysBefore)
			o.ExitBeforeEarnings = p.Bool("exit_before_earnings", o.ExitBeforeEarnings)
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewTrendPullback(o)
		})

	Registry.MustRegister("rsi_pullback",
		"長期上昇トレンド ＋ RSI(3) 短期売られすぎ反発（勝率重視）",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			o := DefaultRSIPullbackOptions()
			o.SMALong = p.Int("sma_long", o.SMALong)
			o.SMAMid = p.Int("sma_mid", o.SMAMid)
			o.SMAShort = p.Int("sma_short", o.SMAShort)
			o.SlopeLookback = p.Int("slope_lookback", o.SlopeLookback)
			o.RSIPeriod = p.Int("rsi_period", o.RSIPeriod)
			o.RSIEntry = p.Float("rsi_entry", o.RSIEntry)
			o.RSIExit = p.Float("rsi_exit", o.RSIExit)
			o.HighLookback = p.Int("high_lookback", o.HighLookback)
			o.MaxDrawdownFromHigh = p.Float("max_drawdown_from_high", o.MaxDrawdownFromHigh)
			o.ATRPeriod = p.Int("atr_period", o.ATRPeriod)
			o.MinATRRatio = p.Float("min_atr_ratio", o.MinATRRatio)
			o.MaxATRRatio = p.Float("max_atr_ratio", o.MaxATRRatio)
			o.MinDollarVolume = p.Float("min_dollar_volume", o.MinDollarVolume)
			o.VolumeLookback = p.Int("volume_lookback", o.VolumeLookback)
			o.RequireReversalBar = p.Bool("require_reversal_bar", o.RequireReversalBar)
			o.Benchmark = p.String("benchmark", o.Benchmark)
			o.BenchmarkSMA = p.Int("benchmark_sma", o.BenchmarkSMA)
			o.BlackoutFile = p.String("blackout_file", "")
			o.BlackoutDaysBefore = p.Int("blackout_days_before", o.BlackoutDaysBefore)
			o.ExitBeforeEarnings = p.Bool("exit_before_earnings", o.ExitBeforeEarnings)
			o.ExitOnRSI = p.Bool("exit_on_rsi", o.ExitOnRSI)
			o.ExitOnHigh = p.Bool("exit_on_high", o.ExitOnHigh)
			o.ExitOnSMARecovery = p.Bool("exit_on_sma_recovery", o.ExitOnSMARecovery)
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewRSIPullback(o)
		})

	Registry.MustRegister("momentum_rank",
		"銘柄横断の中期モメンタム順位（月次入れ替え・損益比重視）",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			o := DefaultMomentumRankOptions()
			o.Lookback = p.Int("lookback", o.Lookback)
			o.Skip = p.Int("skip", o.Skip)
			o.LongLookback = p.Int("long_lookback", o.LongLookback)
			o.VolLookback = p.Int("vol_lookback", o.VolLookback)
			o.TrendSMA = p.Int("trend_sma", o.TrendSMA)
			o.ATRPeriod = p.Int("atr_period", o.ATRPeriod)
			o.MaxATRRatio = p.Float("max_atr_ratio", o.MaxATRRatio)
			o.MinPrice = p.Float("min_price", o.MinPrice)
			o.MinDollarVolume = p.Float("min_dollar_volume", o.MinDollarVolume)
			o.VolumeLookback = p.Int("volume_lookback", o.VolumeLookback)
			o.Benchmark = p.String("benchmark", o.Benchmark)
			o.BenchmarkSMA = p.Int("benchmark_sma", o.BenchmarkSMA)
			o.TopN = p.Int("top_n", o.TopN)
			o.KeepMultiple = p.Int("keep_multiple", o.KeepMultiple)
			o.Rebalance = p.String("rebalance", o.Rebalance)
			o.CoreSymbol = p.String("core_symbol", "")
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewMomentumRank(o)
		})

	Registry.MustRegister("level_bounce",
		"節目反発（フィボナッチ・キリ番の押し目 ＋ 出来高を伴う反転確認、損益比で足切り）",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			o := DefaultLevelBounceOptions()
			o.SwingLookback = p.Int("swing_lookback", o.SwingLookback)
			o.MinImpulse = p.Float("min_impulse", o.MinImpulse)
			o.MinPullbackDays = p.Int("min_pullback_days", o.MinPullbackDays)
			o.Levels = p.Floats("levels", o.Levels)
			o.RoundNumbers = p.Bool("round_numbers", o.RoundNumbers)
			o.ToleranceATR = p.Float("tolerance_atr", o.ToleranceATR)
			o.BounceWindow = p.Int("bounce_window", o.BounceWindow)
			o.RVOLMin = p.Float("rvol_min", o.RVOLMin)
			o.VolumeLookback = p.Int("volume_lookback", o.VolumeLookback)
			o.CloseStrength = p.Float("close_strength", o.CloseStrength)
			o.MinRewardRisk = p.Float("min_reward_risk", o.MinRewardRisk)
			o.SMALong = p.Int("sma_long", o.SMALong)
			o.ATRPeriod = p.Int("atr_period", o.ATRPeriod)
			o.MinATRRatio = p.Float("min_atr_ratio", o.MinATRRatio)
			o.MaxATRRatio = p.Float("max_atr_ratio", o.MaxATRRatio)
			o.MinDollarVolume = p.Float("min_dollar_volume", o.MinDollarVolume)
			o.Benchmark = p.String("benchmark", o.Benchmark)
			o.BenchmarkSMA = p.Int("benchmark_sma", o.BenchmarkSMA)
			o.ExitOnTarget = p.Bool("exit_on_target", o.ExitOnTarget)
			o.ExitOnBreak = p.Bool("exit_on_break", o.ExitOnBreak)
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewLevelBounce(o)
		})

	Registry.MustRegister("margin_balance",
		"信用需給（信用倍率の銘柄横断分位。売り長を避ける／買い長に傾ける）",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			o := DefaultMarginBalanceOptions()
			o.Mode = p.String("mode", o.Mode)
			o.AvoidBelow = p.Float("avoid_below", o.AvoidBelow)
			o.FavorAbove = p.Float("favor_above", o.FavorAbove)
			o.ExitHeld = p.Bool("exit_held", o.ExitHeld)
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewMarginBalance(o)
		})

	Registry.MustRegister("ross_cameron",
		"ロス・キャメロン流 Gap & Go / マイクロプルバック（日足版）",
		func(raw map[string]any) (Strategy, error) {
			p := newParams(raw)
			o := DefaultRossCameronOptions()
			o.GapMin = p.Float("gap_min", o.GapMin)
			o.RVOLMin = p.Float("rvol_min", o.RVOLMin)
			o.CloseStrength = p.Float("close_strength", o.CloseStrength)
			o.HighLookback = p.Int("high_lookback", o.HighLookback)
			o.EMAFast = p.Int("ema_fast", o.EMAFast)
			o.EMASlow = p.Int("ema_slow", o.EMASlow)
			o.VolumeLookback = p.Int("volume_lookback", o.VolumeLookback)
			o.AllowGapEntry = p.Bool("allow_gap_entry", o.AllowGapEntry)
			o.AllowPullbackEntry = p.Bool("allow_pullback_entry", o.AllowPullbackEntry)
			o.CatalystLookback = p.Int("catalyst_lookback", o.CatalystLookback)
			o.MaxPullbackDays = p.Int("max_pullback_days", o.MaxPullbackDays)
			o.ATRPeriod = p.Int("atr_period", o.ATRPeriod)
			o.MinATRRatio = p.Float("min_atr_ratio", o.MinATRRatio)
			o.MaxATRRatio = p.Float("max_atr_ratio", o.MaxATRRatio)
			o.MinDollarVolume = p.Float("min_dollar_volume", o.MinDollarVolume)
			o.MinPrice = p.OptFloat("min_price")
			o.MaxPrice = p.OptFloat("max_price")
			o.Benchmark = p.String("benchmark", o.Benchmark)
			o.BenchmarkSMA = p.Int("benchmark_sma", o.BenchmarkSMA)
			o.BlackoutFile = p.String("blackout_file", "")
			o.BlackoutDaysBefore = p.Int("blackout_days_before", o.BlackoutDaysBefore)
			o.ExitBeforeEarnings = p.Bool("exit_before_earnings", o.ExitBeforeEarnings)
			o.ExitOnEMA = p.Bool("exit_on_ema", o.ExitOnEMA)
			o.ExitOnPrevLow = p.Bool("exit_on_prev_low", o.ExitOnPrevLow)
			if err := p.Err(); err != nil {
				return nil, err
			}
			return NewRossCameron(o)
		})
}

// paramReader は TOML 由来のパラメータを読む補助。
//
// 読んだキーを覚えておき、最後に「知らないキー」を拾う。綴りを間違えた
// パラメータが黙って無視されると、設定したつもりの値で動かない。
type paramReader struct {
	src  map[string]any
	used map[string]struct{}
	errs []string
}

func newParams(src map[string]any) *paramReader {
	return &paramReader{src: src, used: make(map[string]struct{})}
}

func (p *paramReader) lookup(key string) (any, bool) {
	p.used[key] = struct{}{}
	v, ok := p.src[key]
	return v, ok
}

func (p *paramReader) Int(key string, def int) int {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		if n == float64(int64(n)) {
			return int(n)
		}
	}
	p.errs = append(p.errs, fmt.Sprintf("%s は整数で指定してください（%v）", key, v))
	return def
}

func (p *paramReader) Float(key string, def float64) float64 {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	p.errs = append(p.errs, fmt.Sprintf("%s は数値で指定してください（%v）", key, v))
	return def
}

// Floats は数値の配列（TOML の [0.382, 0.5]）。
func (p *paramReader) Floats(key string, def []float64) []float64 {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	items, ok := v.([]any)
	if !ok {
		p.errs = append(p.errs, fmt.Sprintf("%s は数値の配列で指定してください（%v）", key, v))
		return def
	}
	out := make([]float64, 0, len(items))
	for _, item := range items {
		switch n := item.(type) {
		case float64:
			out = append(out, n)
		case int64:
			out = append(out, float64(n))
		case int:
			out = append(out, float64(n))
		default:
			p.errs = append(p.errs, fmt.Sprintf("%s は数値の配列で指定してください（%v）", key, v))
			return def
		}
	}
	return out
}

// OptFloat は「指定が無ければ無効」の数値。nil は未設定を表す。
func (p *paramReader) OptFloat(key string) *float64 {
	if _, ok := p.src[key]; !ok {
		p.used[key] = struct{}{}
		return nil
	}
	value := p.Float(key, 0)
	return &value
}

func (p *paramReader) Bool(key string, def bool) bool {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	p.errs = append(p.errs, fmt.Sprintf("%s は true / false で指定してください（%v）", key, v))
	return def
}

// String は文字列。空文字は「指定なし」の意味で通す
// （benchmark = "" でベンチマーク無しを表せるようにするため）。
func (p *paramReader) String(key string, def string) string {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	p.errs = append(p.errs, fmt.Sprintf("%s は文字列で指定してください（%v）", key, v))
	return def
}

// Err は型の誤りと未知のキーをまとめて返す。
func (p *paramReader) Err() error {
	var unknown []string
	for key := range p.src {
		if _, ok := p.used[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	msgs := append([]string(nil), p.errs...)
	if len(unknown) > 0 {
		msgs = append(msgs, fmt.Sprintf("知らないパラメータ: %s", strings.Join(unknown, ", ")))
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}
