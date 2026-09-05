// Package config はデイトレの設定（config/daytrade/daytrade.toml）。
//
// 資金の上限から銘柄数 N を決めるのが要（fees.PositionsFor）。「N をいくつにするか」を
// 人が書くと、資金を変えたときに手数料段階と合わなくなる。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/fees"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/session"
	"github.com/pelletier/go-toml/v2"
	"github.com/shopspring/decimal"
)

// Filename は設定ファイル名。
const Filename = "daytrade.toml"

// DefaultConfigDir は既定の設定ディレクトリ。
const DefaultConfigDir = "config/daytrade"

// Segments は市場区分の呼び方（universe.segments に書く値）。
var Segments = []string{"prime", "standard", "growth"}

// Capital は資金（[capital]、円）と戦略のスイッチ。
type Capital struct {
	// Enabled が false なら plan / open は何もしない（close は台帳に当日の買いが
	// 残っていれば売る——止めた日に建玉を持ち越さないため）。
	Enabled bool `toml:"enabled"`
	// MaxCapital は 1 日に使う資金の上限。**0 なら N = 0**（様子見モード:
	// スクリーニングと候補の表示はするが買わない）。
	MaxCapital decimal.Decimal `toml:"max_capital"`
	// OrderBudget は 1 注文の目安。N = MaxCapital ÷ OrderBudget。
	// 67 万円は「20〜100 万円は一律」の手数料段階と分散のバランス（研究の結論）。
	OrderBudget decimal.Decimal `toml:"order_budget"`
	// MaxPositions は N の上限。研究では N10 を超えると Sharpe が下がった。
	MaxPositions int `toml:"max_positions"`
	// Weighting は N 銘柄への配分。equal（等金額）か inverse_vol（20 日ボラの逆数）。
	Weighting string `toml:"weighting"`
}

// Margin は信用売り（ショート）の資金と条件（[margin]）。jp_gap_fade_margin 専用。
type Margin struct {
	Enabled bool `toml:"enabled"`
	// Cash は保証金として差し入れる現金（円）。建玉は現金より大きくなるので、
	// 年率や DD はこれに対して見る。0 なら capital.MaxCapital を現金とみなす。
	Cash         decimal.Decimal `toml:"cash"`
	MaxCapital   decimal.Decimal `toml:"max_capital"`
	OrderBudget  decimal.Decimal `toml:"order_budget"`
	MaxPositions int             `toml:"max_positions"`
	Weighting    string          `toml:"weighting"`

	// --- ショート専用の母集団（[universe] はロング用） ---

	// Segments はプライム限定が既定。張り付き（引けストップ高で返済できない）率が
	// プライム 2.1%、グロース 5.3%・スタンダード 5.8% で、持ち越しの損失が OOS を崩す。
	Segments    []string        `toml:"segments"`
	MinTurnover decimal.Decimal `toml:"min_turnover"`
	// ExcludeCapTerciles はショートは小型に効きが厚いので既定は外さない。
	ExcludeCapTerciles   int  `toml:"exclude_cap_terciles"`
	ExcludeEarningsPrev  bool `toml:"exclude_earnings_prev"`
	ExcludeEarningsToday bool `toml:"exclude_earnings_today"`
	// ExcludeMarginAlert は建てられる規制銘柄に効きの差が無いので既定は外さない。
	ExcludeMarginAlert bool `toml:"exclude_margin_alert"`
	// ExcludeJsfStop は日証金の申込停止（売り禁）。新規売りが出せないので必ず外す。
	ExcludeJsfStop bool `toml:"exclude_jsf_stop"`

	// MinGap はギャップがこれ**以上**の銘柄だけ（ロングの逆）。5〜7% は +29 bp/取引で
	// 薄く、7〜10% +79、10〜15% +112、15% 以上 +304。大きい順に取る。
	MinGap decimal.Decimal `toml:"min_gap"`
	// MaxGap はギャップの上限（これ**未満**）。
	MaxGap decimal.Decimal `toml:"max_gap"`
	// SkipLimitUp は寄付がストップ高の銘柄を売らない（踏み上げの初動を売る危険を避ける）。
	SkipLimitUp bool `toml:"skip_limit_up"`

	// MultiplierNormal / MultiplierLongWeak はショート側の資金の倍率（シーソー）。
	// 弱い日限定は Sharpe が高いが稼働が 1/3 に減るので既定は常時 1.0。
	MultiplierNormal   decimal.Decimal `toml:"multiplier_normal"`
	MultiplierLongWeak decimal.Decimal `toml:"multiplier_long_weak"`
	// CarryPenalty は検証のみ: 引けが制限値幅に張り付いて手仕舞えなかった取引
	// （売建の引けストップ高、買建の引けストップ安）を「翌営業日の寄付で手仕舞い」
	// として計上する係数（1 で全額、0 で無視）。ロングだけの検証（Simulate）でも使う。
	CarryPenalty decimal.Decimal `toml:"carry_penalty"`
	// LongShrink はロング側の資産曲線による縮小を効かせるか。false なら合図は
	// ショートのシーソーにだけ使い、ロングは縮めない。
	LongShrink bool `toml:"long_shrink"`
	// ExtraCostBP はショートの往復コスト（bp）。信用手数料 0 円 + 貸株料 + 滑り。
	ExtraCostBP decimal.Decimal `toml:"extra_cost_bp"`
	// LongViaMargin はロング側も信用買い（日計り）で建てる。手数料 0 円になり、
	// 代わりに金利を LongExtraCostBP で見る。
	LongViaMargin   bool            `toml:"long_via_margin"`
	LongExtraCostBP decimal.Decimal `toml:"long_extra_cost_bp"`
}

// Universe は母集団（[universe]）。前夜に確定する条件だけを置く。
type Universe struct {
	Segments     []string        `toml:"segments"`
	MinTurnover  decimal.Decimal `toml:"min_turnover"`
	TurnoverDays int             `toml:"turnover_days"`
	// ExcludeCapTerciles は時価総額の 3 分位のうち下位からいくつ外すか
	// （0=外さない、1=下位 1/3 を外す、2=上位 1/3 のみ）。
	ExcludeCapTerciles int `toml:"exclude_cap_terciles"`
	// ExcludeEarningsPrev は前日引け後に決算短信を開示した銘柄を外す
	// （決算翌日はギャップの符号が反転する）。
	ExcludeEarningsPrev bool `toml:"exclude_earnings_prev"`
	// ExcludeEarningsToday は当日に決算発表の予定がある銘柄を外す（場中開示は宝くじ）。
	ExcludeEarningsToday bool `toml:"exclude_earnings_today"`
	// ExcludeMarginAlert は前日に日々公表信用残の対象だった銘柄を外す（全条件で負ける）。
	ExcludeMarginAlert bool `toml:"exclude_margin_alert"`
}

// Signal は 9:00 に決める条件（[signal]）。
type Signal struct {
	// MaxGap はギャップ（寄付 ÷ 前日終値 − 1）がこれ**未満**の銘柄だけ。
	MaxGap decimal.Decimal `toml:"max_gap"`
	// MinGap はギャップの下限（これ**以上**）。研究では下限を切るほど悪化した。
	MinGap decimal.Decimal `toml:"min_gap"`
	// SkipLimitDown は 9:00 の気配がストップ安の銘柄は買わない
	// （売り殺到の板では引けの売りが約定せず持ち越しになる。勝率 9%）。
	SkipLimitDown bool `toml:"skip_limit_down"`
	// SkipOpened は 9:01 の時点で**既に寄っている**銘柄を候補から外す。
	// ロング・ショートの**両方**に効く（気配そのものを落とすため）。
	//
	// 利益源は特別気配で寄りが遅れる銘柄で、9:00 にすんなり寄る銘柄には戻りが無い
	// （2 年で長短合わせて −13 万円）。しかも寄っている銘柄を 9:01 に成行で買うと
	// 最初の 1 分の反発ぶん平均 +27 bp 高く買う（研究ノート 2026-09-jp-gap-minute の発見 1）。
	//
	// **既定は false。** 真にしてよいのは「寄り前の時価問合（pDPP）が特別気配の
	// 気配値を返す」ことを実機で確かめてから——返らないなら利益源の銘柄はギャップ 0 と
	// 見えて候補に載らず、これを真にすると候補が 1 つも残らない（毎日 no_picks になる）。
	SkipOpened bool `toml:"skip_opened"`
}

// Regime は危険信号（[regime]）。詳細と検証は daytrade/regime と研究ノート。
type Regime struct {
	// IVGate は日経 225 オプションの前日 IV がこれを超える日だけ取引する。0 なら常時。
	IVGate decimal.Decimal `toml:"iv_gate"`
	// SkipMonths は取引しない月（1〜12）。12 月は 9 年中 7 年がマイナス。
	SkipMonths []int `toml:"skip_months"`
	// DriftDays は市場の日中ドリフト（TOPIX 寄り→引け）を取る日数。
	DriftDays int `toml:"drift_days"`
	// DriftGate はドリフトがこれ以下（比率）なら取引しない。nil で無効。
	// 2018・2021 年を黒字にするが 2022 年以降の利益を 3 割削るので既定は無効。
	DriftGate *decimal.Decimal `toml:"drift_gate"`
	// DriftGapOverride は市場ギャップの絶対値がこれを超える日はドリフトのゲートを無視する
	// （急落・急騰の寄付は逆張りが最も効く日）。
	DriftGapOverride decimal.Decimal `toml:"drift_gap_override"`
	// EquityCurveDays は戦略自身の直近 N 日の損益が 0 以下なら資金を縮める。0 で無効。
	EquityCurveDays int `toml:"equity_curve_days"`
	// EquityCurveScale は縮めた後の倍率。0 なら休む。
	EquityCurveScale decimal.Decimal `toml:"equity_curve_scale"`
	// UsSkipLow / UsSkipHigh は前夜の S&P500 の終値リターンがこの帯にあれば休む。
	// UsSkipHigh が nil で無効。研究の既定は 0〜+1%。
	UsSkipLow  decimal.Decimal  `toml:"us_skip_low"`
	UsSkipHigh *decimal.Decimal `toml:"us_skip_high"`
	// UsVixOverride は VIX がこれを超えていれば米国のゲートを無視する。
	UsVixOverride decimal.Decimal `toml:"us_vix_override"`
}

// Execution は発注の振る舞い（[execution]）。
type Execution struct {
	Broker         string                `toml:"broker"`
	TaxAccountType domain.TaxAccountType `toml:"tax_account_type"`
	// QuoteSource は 9:00 の気配の取得元。tachibana / csv。
	QuoteSource string `toml:"quote_source"`
	// QuoteFile は csv のときの置き場（symbol,price[,at] の CSV）。
	QuoteFile string `toml:"quote_file"`
	// EntryWindow は寄付買いを出してよい時間帯（JST）。外なら何もしない。
	EntryWindow []string `toml:"entry_window"`
	// ExitWindow は手仕舞いの成行売りを出してよい時間帯（JST）。15:25 以降の注文は
	// クロージング・オークションに回り引け値で約定する。
	ExitWindow []string `toml:"exit_window"`
	KillSwitch bool     `toml:"kill_switch"`
	// MaxQuoteAge は気配のタイムスタンプがこれより古ければ使わない（秒）。
	MaxQuoteAge int `toml:"max_quote_age"`
}

// Config はデイトレの設定ぜんぶ。
type Config struct {
	// Extends は土台にする設定ディレクトリ（この設定ディレクトリからの相対パス）。
	// 土台を読んだ上に、このファイルに書いた項目だけを重ねる。ロング側の規則を
	// config/daytrade と config/daytrade_margin の 2 箇所に書かないため。
	Extends   string    `toml:"extends"`
	Capital   Capital   `toml:"capital"`
	Universe  Universe  `toml:"universe"`
	Signal    Signal    `toml:"signal"`
	Regime    Regime    `toml:"regime"`
	Execution Execution `toml:"execution"`
	Margin    Margin    `toml:"margin"`
	Book      Book      `toml:"book"`
}

// Book は板・気配の記録（`daytrade snap`。docs/OPENING_DATA.md）。
//
// 集めるだけで、選定には一切使わない。板は過去に遡れない——J-Quants の分足にも
// ティックにも板は無く、立花のリアルタイムから記録を始めた日からしか手に入らない。
// 「どんな気配なら勝率が上がるか」に答えられるようになるのは、ここが溜まってから。
type Book struct {
	// Enabled が false なら snap は何もしない。
	Enabled bool `toml:"enabled"`
	// Columns は時価問合で取りに行く列（sTargetColumn）。空なら始値・現在値・
	// 現在値時刻・前日終値の 4 つ。**板の列名は実機で確かめてから足す**
	// （`daytrade quotes --raw --columns "..."`）。応答に返った列は指定の有無に
	// かかわらず全部記録するので、ここは「何を要求するか」だけ。
	Columns string `toml:"columns"`
	// Scope は記録する銘柄。all（前夜の plan の全行 ＝ 全上場）/ universe（母集団だけ）。
	// 既定は all——母集団の条件を将来変えたくなったとき、記録が無いと検証できない。
	Scope string `toml:"scope"`
}

// Default は既定値。TOML に書かれた項目だけが上書きされる。
func Default() Config {
	return Config{
		Capital: Capital{
			Enabled:      true,
			MaxCapital:   decimal.NewFromInt(2_000_000),
			OrderBudget:  decimal.NewFromInt(670_000),
			MaxPositions: 10,
			Weighting:    "inverse_vol",
		},
		Universe: Universe{
			Segments:             []string{"prime"},
			MinTurnover:          decimal.NewFromInt(100_000_000),
			TurnoverDays:         20,
			ExcludeCapTerciles:   1,
			ExcludeEarningsPrev:  true,
			ExcludeEarningsToday: true,
			ExcludeMarginAlert:   true,
		},
		Signal: Signal{
			MaxGap:        decimal.Zero,
			MinGap:        decimal.NewFromInt(-1),
			SkipLimitDown: true,
			SkipOpened:    false,
		},
		Regime: Regime{
			IVGate:           decimal.Zero,
			DriftDays:        20,
			DriftGapOverride: decimal.RequireFromString("0.01"),
			EquityCurveDays:  0,
			EquityCurveScale: decimal.RequireFromString("0.5"),
			UsSkipLow:        decimal.Zero,
			UsVixOverride:    decimal.NewFromInt(24),
		},
		Execution: Execution{
			Broker:         "tachibana",
			TaxAccountType: domain.TaxAccountSpecific,
			QuoteSource:    "tachibana",
			EntryWindow:    []string{"09:00", "09:15"},
			ExitWindow:     []string{"15:20", "15:30"},
			MaxQuoteAge:    90,
		},
		Book: Book{Enabled: true, Scope: "all"},
		Margin: Margin{
			Enabled:              false,
			OrderBudget:          decimal.NewFromInt(670_000),
			MaxPositions:         10,
			Weighting:            "inverse_vol",
			Segments:             []string{"prime"},
			MinTurnover:          decimal.NewFromInt(100_000_000),
			ExcludeCapTerciles:   0,
			ExcludeEarningsPrev:  true,
			ExcludeEarningsToday: true,
			ExcludeMarginAlert:   false,
			ExcludeJsfStop:       true,
			MinGap:               decimal.RequireFromString("0.05"),
			MaxGap:               decimal.NewFromInt(1),
			SkipLimitUp:          true,
			MultiplierNormal:     decimal.NewFromInt(1),
			MultiplierLongWeak:   decimal.NewFromInt(1),
			CarryPenalty:         decimal.NewFromInt(1),
			LongShrink:           true,
			ExtraCostBP:          decimal.NewFromInt(5),
			LongViaMargin:        false,
			LongExtraCostBP:      decimal.NewFromInt(5),
		},
	}
}

// Positions はこの資金で持つ銘柄数 N。資金 0 なら 0。
func (c Capital) Positions() int {
	if c.MaxCapital.IsZero() {
		return 0
	}
	n, err := fees.PositionsFor(c.MaxCapital, c.OrderBudget, c.MaxPositions)
	if err != nil {
		return 0
	}
	return n
}

// BudgetPerOrder は 1 注文の予算（MaxCapital ÷ N、円未満切り捨て）。N = 0 なら 0。
func (c Capital) BudgetPerOrder() decimal.Decimal {
	n := c.Positions()
	if n == 0 {
		return decimal.Zero
	}
	return c.MaxCapital.Div(decimal.NewFromInt(int64(n))).Floor()
}

// Positions はショート側の銘柄数 N。無効か資金 0 なら 0。
func (m Margin) Positions() int {
	if !m.Enabled || m.MaxCapital.IsZero() {
		return 0
	}
	n, err := fees.PositionsFor(m.MaxCapital, m.OrderBudget, m.MaxPositions)
	if err != nil {
		return 0
	}
	return n
}

// BudgetPerOrder はショート側の 1 注文の予算。
func (m Margin) BudgetPerOrder() decimal.Decimal {
	n := m.Positions()
	if n == 0 {
		return decimal.Zero
	}
	return m.MaxCapital.Div(decimal.NewFromInt(int64(n))).Floor()
}

// Window は entry / exit の時間帯（JST の時・分）。
func (e Execution) Window(name string) (startHour, startMinute, endHour, endMinute int, err error) {
	raw := e.ExitWindow
	if name == "entry" {
		raw = e.EntryWindow
	}
	if len(raw) != 2 {
		return 0, 0, 0, 0, fmt.Errorf("%s_window は開始と終了の 2 要素", name)
	}
	sh, sm, err := session.ParseTime(raw[0], name+"_window")
	if err != nil {
		return 0, 0, 0, 0, err
	}
	eh, em, err := session.ParseTime(raw[1], name+"_window")
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if sh*60+sm >= eh*60+em {
		return 0, 0, 0, 0, fmt.Errorf("時間帯の開始が終了より後です: %v", raw)
	}
	return sh, sm, eh, em, nil
}

// Load は設定を読む。ファイルが無い・内容が不正ならエラー。
func Load(configDir string) (Config, error) {
	if configDir == "" {
		configDir = DefaultConfigDir
	}
	cfg, err := load(configDir, nil)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", filepath.Join(configDir, Filename), err)
	}
	return cfg, nil
}

// load は 1 つの設定ディレクトリを読む。extends があれば先にその土台を読み、
// その上にこのファイルの項目を重ねる（配列は置き換え、表は項目ごとに上書き）。
// visited は循環（A extends B extends A）の検出。
func load(configDir string, visited []string) (Config, error) {
	path := filepath.Join(configDir, Filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("デイトレの設定が見つかりません: %s", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for _, seen := range visited {
		if seen == abs {
			return Config{}, fmt.Errorf("%s: extends が循環しています", path)
		}
	}
	visited = append(visited, abs)

	// extends だけ先に読む（土台を決めてから全体を重ねる）
	var head struct {
		Extends string `toml:"extends"`
	}
	if err := toml.Unmarshal(raw, &head); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg := Default()
	if head.Extends != "" {
		base, err := load(filepath.Join(configDir, head.Extends), visited)
		if err != nil {
			return Config{}, fmt.Errorf("%s の土台（extends = %q）: %w", path, head.Extends, err)
		}
		cfg = base
	}

	decoder := toml.NewDecoder(newReader(raw))
	// 未知の項目を黙って無視すると、綴りを間違えた設定が「効いているつもり」で
	// 効かないまま本番に乗る。読めた時点で弾く。
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate は範囲と整合を確かめる。
func (c Config) Validate() error {
	if err := validateWeighting(c.Capital.Weighting); err != nil {
		return err
	}
	if c.Capital.OrderBudget.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("capital.order_budget は正の値")
	}
	if c.Capital.MaxCapital.IsNegative() {
		return fmt.Errorf("capital.max_capital は 0 以上（0 は「買わない」）")
	}
	if err := validateSegments(c.Universe.Segments); err != nil {
		return fmt.Errorf("universe.%w", err)
	}
	if c.Universe.ExcludeCapTerciles < 0 || c.Universe.ExcludeCapTerciles > 2 {
		return fmt.Errorf("universe.exclude_cap_terciles は 0〜2")
	}
	if err := validateGap(c.Signal.MaxGap, "signal.max_gap"); err != nil {
		return err
	}
	if err := validateGap(c.Signal.MinGap, "signal.min_gap"); err != nil {
		return err
	}
	for _, m := range c.Regime.SkipMonths {
		if m < 1 || m > 12 {
			return fmt.Errorf("regime.skip_months は 1〜12: %d", m)
		}
	}
	if c.Regime.EquityCurveScale.IsNegative() || c.Regime.EquityCurveScale.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("regime.equity_curve_scale は 0〜1")
	}
	if c.Regime.UsSkipHigh != nil && c.Regime.UsSkipHigh.LessThanOrEqual(c.Regime.UsSkipLow) {
		return fmt.Errorf("regime.us_skip_high は us_skip_low より大きい値")
	}
	if c.Regime.DriftDays < 0 || c.Regime.EquityCurveDays < 0 {
		return fmt.Errorf("regime.drift_days / equity_curve_days は 0 以上")
	}
	if _, _, _, _, err := c.Execution.Window("entry"); err != nil {
		return err
	}
	if _, _, _, _, err := c.Execution.Window("exit"); err != nil {
		return err
	}
	if err := validateWeighting(c.Margin.Weighting); err != nil {
		return err
	}
	if err := validateSegments(c.Margin.Segments); err != nil {
		return fmt.Errorf("margin.%w", err)
	}
	if c.Margin.ExcludeCapTerciles < 0 || c.Margin.ExcludeCapTerciles > 2 {
		return fmt.Errorf("margin.exclude_cap_terciles は 0〜2")
	}
	if c.Margin.OrderBudget.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("margin.order_budget は正の値")
	}
	if err := validateGap(c.Margin.MinGap, "margin.min_gap"); err != nil {
		return err
	}
	if err := validateGap(c.Margin.MaxGap, "margin.max_gap"); err != nil {
		return err
	}
	if c.Margin.CarryPenalty.IsNegative() || c.Margin.CarryPenalty.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("margin.carry_penalty は 0〜1")
	}
	for name, v := range map[string]decimal.Decimal{
		"margin.cash":                 c.Margin.Cash,
		"margin.max_capital":          c.Margin.MaxCapital,
		"margin.extra_cost_bp":        c.Margin.ExtraCostBP,
		"margin.long_extra_cost_bp":   c.Margin.LongExtraCostBP,
		"margin.multiplier_normal":    c.Margin.MultiplierNormal,
		"margin.multiplier_long_weak": c.Margin.MultiplierLongWeak,
		"margin.min_turnover":         c.Margin.MinTurnover,
	} {
		if v.IsNegative() {
			return fmt.Errorf("%s は 0 以上", name)
		}
	}
	// 資金と 1 注文の目安が矛盾していないかは、N を実際に導いて確かめる
	if !c.Capital.MaxCapital.IsZero() {
		if _, err := fees.PositionsFor(c.Capital.MaxCapital, c.Capital.OrderBudget, c.Capital.MaxPositions); err != nil {
			return fmt.Errorf("capital: %w", err)
		}
	}
	if c.Margin.Enabled && !c.Margin.MaxCapital.IsZero() {
		if _, err := fees.PositionsFor(c.Margin.MaxCapital, c.Margin.OrderBudget, c.Margin.MaxPositions); err != nil {
			return fmt.Errorf("margin: %w", err)
		}
	}
	return nil
}

// StrategyName はログと注文の理由に書く戦略名。ショートの脚が有効なら jp_gap_fade_margin。
func (c Config) StrategyName() string {
	if c.Margin.Enabled {
		return "jp_gap_fade_margin"
	}
	return "jp_gap_fade"
}

func validateWeighting(v string) error {
	if v != "equal" && v != "inverse_vol" {
		return fmt.Errorf("weighting は equal か inverse_vol: %s", v)
	}
	return nil
}

func validateSegments(v []string) error {
	if len(v) == 0 {
		return fmt.Errorf("segments が空です")
	}
	var unknown []string
	for _, s := range v {
		if !slices.Contains(Segments, s) {
			unknown = append(unknown, s)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("segments に未知の値: %v（使えるのは %v）", unknown, Segments)
	}
	return nil
}

func validateGap(v decimal.Decimal, name string) error {
	if v.LessThan(decimal.NewFromInt(-1)) || v.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("%s は −1〜1 の比率で書く", name)
	}
	return nil
}
