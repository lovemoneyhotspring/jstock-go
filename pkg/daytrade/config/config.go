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
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/fees"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/rerank"
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
	// MaxOrder は 1 銘柄の金額の上限（円）。候補が N に満たない日に総予算を 1 銘柄に寄せない
	// （margin.max_order のロング版）。0 なら上限なし＝総予算 OrderBudget × N を残った銘柄で按分する。
	MaxOrder decimal.Decimal `toml:"max_order"`
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
	// ExcludeCorpEvents は TOB・MBO・締め出し・上場廃止など価格の行き先が決まった銘柄を外す
	// （news.Classify で TDnet 適時開示の見出しから判定）。買付価格までサヤ寄せしてストップ高に
	// 張り付き、返済の買いが通らない。plan でも付けるが、効かせる本番は open の時点の記録簿での付け直し。
	// 電文は 2026-06-17 からしか無いので**バックテストでは効かない**。
	ExcludeCorpEvents bool `toml:"exclude_corp_events"`
	// CorpEventLookbackDays は何暦日前の開示まで見るか。TOB の最初の公表から締め出しの開示まで
	// 49〜84 日の例がある（60 日だと 4 件が窓の外）ので既定は 120。
	CorpEventLookbackDays int `toml:"corp_event_lookback_days"`
	// CorpEventMaxStalenessMinutes は記録簿の最後の取り込み（成功）がこれより古ければ、その回の
	// ショートを見送る（前夜〜朝の公表を知らないまま売らない）。取り込みは 8:52 と 20:20 と 6:20。
	CorpEventMaxStalenessMinutes int `toml:"corp_event_max_staleness_minutes"`
	// CancelOnCorpEvent は場中（daytrade guard）に材料の出た今日の売建を処置する: 未約定なら取消、
	// 一部約定なら残りを取消して約定分を返済買い、全部約定なら返済買い。open が外すのは判定の時点で
	// 分かっていた材料だけで、取り込みの後や場中の公表で建ててしまった売建を救うため。
	CancelOnCorpEvent bool `toml:"cancel_on_corp_event"`

	// MinGap はギャップがこれ**以上**の銘柄だけ（ロングの逆）。5〜7% は +29 bp/取引で
	// 薄く、7〜10% +79、10〜15% +112、15% 以上 +304。大きい順に取る。
	MinGap decimal.Decimal `toml:"min_gap"`
	// MaxGap はギャップの上限（これ**未満**）。
	MaxGap decimal.Decimal `toml:"max_gap"`
	// SkipLimitUp は寄付がストップ高の銘柄を売らない（踏み上げの初動を売る危険を避ける）。
	SkipLimitUp bool `toml:"skip_limit_up"`
	// MaxShortInterest は空売り残高（markets/short-sale-report の ShrtPosToSO を銘柄ごとに
	// 合計した値。発行済株式数に対する比）の上限。これを**超える**銘柄は売らない。
	// 0 なら無制限（既定）。
	//
	// 残高の重い銘柄は踏み上げ（ショートスクイーズ）の燃料を抱えている。10 年の横断では
	// 残高 ≥ 2% の候補は張り付き率が 2 倍（6.7% → 13%〜15%）で、寄→引もマイナス
	// （＝ショートに不利）——**張り付きやすさと不利さが同じ向きに出る唯一の要因**。
	// 「余地」「過熱」「サイズ縮小」が効かなかったのは、それらが利益源と重なっていたため
	// （研究ノート 2026-09-jp-short-squeeze-fuel）。
	//
	// 報告の無い銘柄は残高 0 として扱う（＝落とさない）。報告義務は 0.5% 以上なので、
	// 報告が無い＝重い残高が無い、と読める。
	MaxShortInterest decimal.Decimal `toml:"max_short_interest"`

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
	// MaxOrder は 1 注文の金額の上限（円）。0 なら上限なし＝候補が N に満たない日は残った
	// 銘柄で総予算（OrderBudget × N）を分け合う（ロングと同じ。selection.PickFrom の既定）。
	//
	// ショートは候補が中央値 2 銘柄/日で N3 が埋まらず、候補 1 銘柄の日は 200 万円が
	// 1 銘柄に乗る。10 年の最悪 20 取引のうち 13 件がこの「全額 1 銘柄」の日で、
	// ショートの尻尾の実体はこれ（研究ノート 2026-09-jp-shock-days）。
	MaxOrder decimal.Decimal `toml:"max_order"`
	// SpillToLong が真なら、ショートで使わなかった資金（候補が無い日の全額、MaxOrder で
	// 頭打ちにした残り）をその日のロングに回す。ロングの銘柄数はその分だけ増え
	// （総予算 ÷ order_budget、capital.max_positions が上限）、長短の合計は変わらない
	// ので保証金の枠も変わらない。ショートは候補が中央値 1 銘柄/日で、10 年の 33% の日は
	// 候補 0（研究ノート 2026-09-jp-shock-days）。
	SpillToLong bool `toml:"spill_to_long"`
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
	// ExcludeLoss は直近の本決算が赤字（当期純利益 ≤ 0）の銘柄を外す。
	// 赤字銘柄のギャップは個別の悪材料で戻らない（10 年 216 件・平均 −11.9 bp・
	// 損益 −21 万円。研究ノート 2026-09-jp-value-signal）。
	ExcludeLoss bool `toml:"exclude_loss"`
}

// Signal は 9:00 に決める条件（[signal]）。
// 候補の並べ方（Signal.RankBy）。
const (
	RankByGap    = "gap"
	RankByGapVol = "gap_vol"
	// RankByLGBM は機械学習（LightGBM）の予測値の高い順。候補・帯・N の取り方は同じで、
	// 並べる順番だけが変わる（daytrade/rerank）。モデルは Signal.Model。
	RankByLGBM = "lgbm"
)

type Signal struct {
	// MaxGap はギャップ（寄付 ÷ 前日終値 − 1）がこれ**未満**の銘柄だけ。
	MaxGap decimal.Decimal `toml:"max_gap"`
	// MinGap はギャップの下限（これ**以上**）。研究では下限を切るほど悪化した。
	MinGap decimal.Decimal `toml:"min_gap"`
	// SkipLimitDown は 9:00 の気配がストップ安の銘柄は買わない
	// （売り殺到の板では引けの売りが約定せず持ち越しになる。勝率 9%）。
	SkipLimitDown bool `toml:"skip_limit_down"`
	// RankBy は候補の並べ方。gap（既定）はギャップの小さい順、gap_vol はギャップを
	// 20 日ボラで割った正規化ギャップの小さい順（1% しか動かない銘柄の −5% は、4% 動く
	// 銘柄の −5% より極端）。ボラが取れない銘柄は末尾。
	// 研究ノート 2026-09-jp-daytrade-selection-2: 探索 +6.7 bp/日（t 2.1）・確認 +6.7 bp（t 1.7）で
	// 事前基準（t 2.5）には届かず、追跡中の仮説。
	RankBy string `toml:"rank_by"`
	// Model は rank_by = "lgbm" のモデル（LightGBM のテキスト形式）。相対パスは
	// **この項目を書いた設定ファイルのディレクトリから**（読み込み時に絶対パスにする。
	// extends で継いだ子の設定からも同じファイルを指すため）。
	Model string `toml:"model"`
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
	// ValuePool は 2 段階選定。0 / 1 なら無効（ギャップ順のまま上位 N）。
	// 2 以上なら「ギャップ順の上位 N × ValuePool を母数にして、その中の益回り
	// （直近の本決算の当期純利益 ÷ 前日の時価総額）が高い順に N 銘柄」を選ぶ。
	//
	// ギャップ逆張りは割安な銘柄ほど強い（益回り 3 分位で 割高 +16.7 / 中位 +31.6 /
	// 割安 +40.9 bp、差 +24.3 bp・t 2.7。IS +26.5 / OOS +22.2 と両半期で同じ大きさ）。
	// 割高・赤字銘柄のギャップは個別のニュースで戻らず、割安銘柄のギャップは市場の
	// 振れなので戻る、という読み（研究ノート 2026-09-jp-value-signal）。
	ValuePool int `toml:"value_pool"`
	// MaxPerSector は同じ 33 業種から建ててよい銘柄数の上限。0 で無制限（既定）。
	//
	// 同じ日に同じ業種を 2 銘柄以上建てた取引は 10 年で平均 +6.4 bp、単独の +37.4 bp に
	// 対して差 −31.0 bp（t −3.5）。IS −36.6（t −3.1）/ OOS −25.4（t −2.0）で両半期とも
	// 劣り、市場のギャップ・銘柄のギャップのどの帯で見ても向きは同じ（交絡ではない）。
	// 同業が揃って候補に載る日は業種まるごとの材料（セクター一括の格下げなど）で、
	// 個別のパニック売りではないという読み（研究ノート 2026-09-jp-sector-crowding）。
	MaxPerSector int `toml:"max_per_sector"`
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
	// UsStaleWaitUntil は前夜の米国セッションがまだ取れていないとき、この時刻（JST の "HH:MM"）
	// より前の回は判定せずに見送り、次の回を待つ。過ぎたら手元の最新で判定する。空なら待たない。
	// 米国の信号（us_skip_high / shock_us_ret）を使う設定でだけ意味を持つ。
	UsStaleWaitUntil string `toml:"us_stale_wait_until"`

	// --- ショック日（予期せぬ急落）のサイズ変更 ---
	//
	// 予定された経済イベント（FOMC・日銀・雇用統計・選挙）には効きが無く、予定できない
	// ショック（前夜の VIX の跳ね・S&P の急落・9:00 の市場ギャップ）だけが効く。ショック日は
	// この戦略の最良の日で（市場ギャップ ≤ −2% の日は +7.8 万/日、t = 6.2、勝率 78%、IS/OOS 一致）、
	// 効くのはロング脚だけ（ショートの勝率は 16〜26%）。研究ノート 2026-09-jp-shock-days。
	//
	// ShockMarketGap は 9:00 の市場ギャップ（候補の中央値）がこれ以下ならショック日。nil で見ない。
	ShockMarketGap *decimal.Decimal `toml:"shock_market_gap"`
	// ShockUsRet は前夜の S&P500 の終値リターンがこれ以下ならショック日。nil で見ない。
	ShockUsRet *decimal.Decimal `toml:"shock_us_ret"`
	// ShockLongScale / ShockShortScale はショック日にロング／ショートの資金に掛ける倍率。
	// 既定 1（記録だけして変えない）。資産曲線の縮小とは掛け算で重なる。
	ShockLongScale  decimal.Decimal `toml:"shock_long_scale"`
	ShockShortScale decimal.Decimal `toml:"shock_short_scale"`
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
	// GuardWindow は材料の出た売建を取消・返済してよい時間帯（daytrade guard）。引けの手仕舞い
	// （15:20〜）と重ねない。
	GuardWindow []string `toml:"guard_window"`
	KillSwitch  bool     `toml:"kill_switch"`
	// MaxQuoteAge は気配のタイムスタンプがこれより古ければ使わない（秒）。
	MaxQuoteAge int `toml:"max_quote_age"`
	// MaxRunSeconds は open / close の 1 回の実行に許す時間（秒）。開始からこれだけ経つか
	// 時間帯（entry_window / exit_window）の終わりが来たら、その先の電文は送らず、
	// 送信中のものは打ち切る。ブローカーが遅い日に 1 回の実行がロックを握り続けて
	// 次の cron まで潰す（発注の機会が丸ごと消える）のを防ぐ。cron の間隔より短くする。
	// 0 なら時間帯の終わりだけを締め切りにする。
	MaxRunSeconds int `toml:"max_run_seconds"`
}

// RunDeadline は now に始めた実行の締め切り。
//
// 時間帯の終わり（useWindow が真のとき。JST の今日）と now + MaxRunSeconds の早い方。
// どちらも無ければゼロ値（締め切りなし）。
func (e Execution) RunDeadline(name string, now time.Time, useWindow bool, jst *time.Location) time.Time {
	var deadline time.Time
	if e.MaxRunSeconds > 0 {
		deadline = now.Add(time.Duration(e.MaxRunSeconds) * time.Second)
	}
	if useWindow {
		if _, _, eh, em, err := e.Window(name); err == nil {
			local := now.In(jst)
			end := time.Date(local.Year(), local.Month(), local.Day(), eh, em, 0, 0, jst)
			if deadline.IsZero() || end.Before(deadline) {
				deadline = end
			}
		}
	}
	return deadline
}

// InWindow は now が name（entry / exit）の時間帯か（JST）。
//
// 終わりは RunDeadline と同じ「HH:MM:00」で、秒まで見て切る。分単位で両端を含めると
// 9:15:30 が「窓の中」なのに締め切り済みになり、全候補が失敗として通知される。
// 時間帯の設定が読めなければ偽（外として何もしない）。
func (e Execution) InWindow(name string, now time.Time, jst *time.Location) bool {
	sh, sm, eh, em, err := e.Window(name)
	if err != nil {
		return false
	}
	local := now.In(jst)
	start := time.Date(local.Year(), local.Month(), local.Day(), sh, sm, 0, 0, jst)
	end := time.Date(local.Year(), local.Month(), local.Day(), eh, em, 0, 0, jst)
	return !local.Before(start) && local.Before(end)
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
	// MaxRunSeconds は snap 1 回に許す時間（秒）。snap は open / close とロックを共有する
	// ので、遅い日にここで粘ると 9:01 の発注がロックを取れずに消える。全上場 3,700 銘柄
	// でも正常なら 10 秒で終わる。0 なら締め切りなし。
	MaxRunSeconds int `toml:"max_run_seconds"`
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
			RankBy:        RankByGap,
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
			ShockLongScale:   decimal.NewFromInt(1),
			ShockShortScale:  decimal.NewFromInt(1),
		},
		Execution: Execution{
			Broker:         "tachibana",
			TaxAccountType: domain.TaxAccountSpecific,
			QuoteSource:    "tachibana",
			EntryWindow:    []string{"09:00", "09:15"},
			ExitWindow:     []string{"15:20", "15:30"},
			GuardWindow:    []string{"09:00", "15:19"},
			MaxQuoteAge:    90,
			MaxRunSeconds:  150,
		},
		Book: Book{Enabled: true, Scope: "all", MaxRunSeconds: 50},
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
			ExcludeCorpEvents:    true,
			// 窓と鮮度の既定は config.Margin の説明を参照
			CorpEventLookbackDays:        120,
			CorpEventMaxStalenessMinutes: 90,
			CancelOnCorpEvent:            true,
			MinGap:                       decimal.RequireFromString("0.05"),
			MaxGap:                       decimal.NewFromInt(1),
			SkipLimitUp:                  true,
			MultiplierNormal:             decimal.NewFromInt(1),
			MultiplierLongWeak:           decimal.NewFromInt(1),
			CarryPenalty:                 decimal.NewFromInt(1),
			LongShrink:                   true,
			MaxOrder:                     decimal.Zero,
			ExtraCostBP:                  decimal.NewFromInt(5),
			LongViaMargin:                false,
			LongExtraCostBP:              decimal.NewFromInt(5),
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

// Window は entry / guard / exit の時間帯（JST の時・分）。それ以外の名前は exit。
func (e Execution) Window(name string) (startHour, startMinute, endHour, endMinute int, err error) {
	raw := e.ExitWindow
	switch name {
	case "entry":
		raw = e.EntryWindow
	case "guard":
		raw = e.GuardWindow
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

// WaitsForUs は now（JST）が regime.us_stale_wait_until より前か——前夜の米国セッションが
// 取れていない回を見送って次の回を待つか。設定が空（読めない）なら偽。
func (r Regime) WaitsForUs(now time.Time, jst *time.Location) bool {
	if r.UsStaleWaitUntil == "" {
		return false
	}
	h, m, err := session.ParseTime(r.UsStaleWaitUntil, "regime.us_stale_wait_until")
	if err != nil {
		return false
	}
	local := now.In(jst)
	return local.Before(time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, jst))
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
		// extends は「この設定ディレクトリからの相対」が基本だが、絶対パスで
		// 書かれたらそのまま使う。filepath.Join に絶対パスを渡すと先頭の / が
		// 落ちて configDir の下にぶら下がってしまう（/a を土台にしたつもりが
		// <configDir>/a を読みに行き「設定が見つかりません」になる）。
		baseDir := head.Extends
		if !filepath.IsAbs(baseDir) {
			baseDir = filepath.Join(configDir, baseDir)
		}
		base, err := load(baseDir, visited)
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
	// signal.model はこのファイルに書かれていたときだけ、このディレクトリから解決する
	var own struct {
		Signal struct {
			Model string `toml:"model"`
		} `toml:"signal"`
	}
	if err := toml.Unmarshal(raw, &own); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if own.Signal.Model != "" && !filepath.IsAbs(own.Signal.Model) {
		model, err := filepath.Abs(filepath.Join(configDir, own.Signal.Model))
		if err != nil {
			return Config{}, fmt.Errorf("%s: signal.model: %w", path, err)
		}
		cfg.Signal.Model = model
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
	if c.Capital.MaxOrder.IsNegative() {
		return fmt.Errorf("capital.max_order は 0 以上（0 は上限なし）")
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
	switch c.Signal.RankBy {
	case RankByGap, RankByGapVol:
	case RankByLGBM:
		if c.Signal.Model == "" {
			return fmt.Errorf("signal.rank_by = %q には signal.model（モデルのファイル）が要る", RankByLGBM)
		}
		// 読めないモデルで朝を迎えないよう、設定を読んだ時点で確かめる（読んだものは使い回す）
		if _, err := rerank.Cached(c.Signal.Model); err != nil {
			return fmt.Errorf("signal.model: %w", err)
		}
	default:
		return fmt.Errorf("signal.rank_by は %s / %s / %s: %q", RankByGap, RankByGapVol, RankByLGBM, c.Signal.RankBy)
	}
	if c.Signal.ValuePool < 0 {
		return fmt.Errorf("signal.value_pool は 0 以上（0 / 1 は無効）")
	}
	if c.Signal.MaxPerSector < 0 {
		return fmt.Errorf("signal.max_per_sector は 0 以上（0 で無制限）")
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
	if c.Regime.UsStaleWaitUntil != "" {
		if _, _, err := session.ParseTime(c.Regime.UsStaleWaitUntil, "regime.us_stale_wait_until"); err != nil {
			return err
		}
	}
	if c.Regime.DriftDays < 0 || c.Regime.EquityCurveDays < 0 {
		return fmt.Errorf("regime.drift_days / equity_curve_days は 0 以上")
	}
	if c.Regime.ShockLongScale.IsNegative() || c.Regime.ShockShortScale.IsNegative() {
		return fmt.Errorf("regime.shock_long_scale / shock_short_scale は 0 以上")
	}
	if c.Regime.ShockMarketGap != nil && c.Regime.ShockMarketGap.GreaterThanOrEqual(decimal.Zero) {
		return fmt.Errorf("regime.shock_market_gap は負の値（急落の市場ギャップ）")
	}
	if c.Regime.ShockUsRet != nil && c.Regime.ShockUsRet.GreaterThanOrEqual(decimal.Zero) {
		return fmt.Errorf("regime.shock_us_ret は負の値（前夜の急落）")
	}
	if _, _, _, _, err := c.Execution.Window("entry"); err != nil {
		return err
	}
	if _, _, _, _, err := c.Execution.Window("exit"); err != nil {
		return err
	}
	// 書き間違えると guard が毎回「時間帯の外」で黙って何もしなくなる
	if _, _, _, _, err := c.Execution.Window("guard"); err != nil {
		return err
	}
	if c.Execution.MaxRunSeconds < 0 || c.Book.MaxRunSeconds < 0 {
		return fmt.Errorf("execution.max_run_seconds / book.max_run_seconds は 0 以上")
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
	if c.Margin.MaxOrder.IsNegative() {
		return fmt.Errorf("margin.max_order は 0 以上（0 は上限なし）")
	}
	// guard（cancel_on_corp_event）も同じ窓と鮮度で記録簿を読むので、どちらか一方だけ使うときも検査する
	if (c.Margin.ExcludeCorpEvents || c.Margin.CancelOnCorpEvent) &&
		(c.Margin.CorpEventLookbackDays < 1 || c.Margin.CorpEventMaxStalenessMinutes < 1) {
		return fmt.Errorf("margin.exclude_corp_events か cancel_on_corp_event を使うなら corp_event_lookback_days と corp_event_max_staleness_minutes は 1 以上")
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
		"capital.max_order":           c.Capital.MaxOrder,
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
