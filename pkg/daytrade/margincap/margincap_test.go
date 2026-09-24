package margincap

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func day() time.Time { return time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC) }

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// 2026-09-16 に本番口座で取れた実際の値。
func realRows() []domain.MarginSummary {
	return []domain.MarginSummary{
		{Date: "20260916", UkeireHosyoukin: dec("3823546"), GenkinHosyoukin: dec("2055696"),
			DaiyouHyoukagaku: dec("1720000"), SinyouSinkidate: dec("11586503")},
		{Date: "20260918", UkeireHosyoukin: dec("3813827"), GenkinHosyoukin: dec("2093827"),
			DaiyouHyoukagaku: dec("1720000"), SinyouSinkidate: dec("11557051"),
			SonotaKousokukin: dec("3211")},
	}
}

// Conservative は建てられる額を最小、拘束金を最大で採る（どちらも安全側）。
func TestConservativeTakesWorstOfEachKind(t *testing.T) {
	s, err := Conservative(day(), realRows())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.SinyouSinkidate.String(); got != "11557051" {
		t.Errorf("建可能額 %s, want 11557051（最小）", got)
	}
	if got := s.SourceDate; got != "20260918" {
		t.Errorf("採った行 %s, want 20260918", got)
	}
	// 拘束金は当日 0 でも 2 日後に出る。当日だけ見ると枠を過大に見積もる
	if got := s.SonotaKousokukin.String(); got != "3211" {
		t.Errorf("その他拘束金 %s, want 3211（最大）", got)
	}
}

func TestConservativeNeedsAtLeastOneRow(t *testing.T) {
	if _, err := Conservative(day(), nil); err == nil {
		t.Fatal("1 行も無いのにエラーにならなかった")
	}
}

// 建玉合計 = 建可能額 ÷ 1.3。
func TestCapacity(t *testing.T) {
	s := Snapshot{SinyouSinkidate: dec("11557051")}
	if got := s.Capacity().String(); got != "8890039" {
		t.Errorf("建玉合計 %s, want 8890039", got)
	}
}

// 脚への割り振りは**設定の max_capital の比**。固定の 6:4 ではない
// ——縮小はリスクを下げる操作で、長短の方針を変える操作ではない。
func TestLegTargetsFollowsConfigRatio(t *testing.T) {
	// 300 万 : 200 万 = 3 : 2
	long, short := legTargets(prodLike(), dec("1000000"))
	if got := long.String(); got != "600000" {
		t.Errorf("ロング %s, want 600000", got)
	}
	if !long.Add(short).Equal(dec("1000000")) {
		t.Errorf("両脚の和 %s が建玉合計に一致しない", long.Add(short))
	}

	// ショートを据え置いてロングだけ上げた設定（533 万 : 200 万）でも比が保たれる
	skewed := prodLike()
	skewed.Capital.MaxCapital = dec("5330000")
	long2, short2 := legTargets(skewed, dec("7330000"))
	// 5330/7330 ≒ 72.7%
	if long2.LessThan(dec("5329000")) || long2.GreaterThan(dec("5331000")) {
		t.Errorf("ロング %s, want ≒5330000（設定の比を保つ）", long2)
	}
	if short2.LessThan(dec("1999000")) || short2.GreaterThan(dec("2001000")) {
		t.Errorf("ショート %s, want ≒2000000", short2)
	}
}

// ショートが無効ならすべてロングへ。
func TestLegTargetsWithoutShort(t *testing.T) {
	cfg := prodLike()
	cfg.Margin.Enabled = false
	long, short := legTargets(cfg, dec("1000000"))
	if !long.Equal(dec("1000000")) || !short.IsZero() {
		t.Errorf("ロング %s / ショート %s, want 1000000 / 0", long, short)
	}
}

// 両脚とも 0 の設定は割りようがない（0 を返し、下げ方向のみなので何も起きない）。
func TestLegTargetsWithNoCapital(t *testing.T) {
	cfg := prodLike()
	cfg.Capital.MaxCapital = decimal.Zero
	cfg.Margin.MaxCapital = decimal.Zero
	long, short := legTargets(cfg, dec("1000000"))
	if !long.IsZero() || !short.IsZero() {
		t.Errorf("ロング %s / ショート %s, want 0 / 0", long, short)
	}
}

func TestCapacityIsZeroWhenNothingAvailable(t *testing.T) {
	if got := (Snapshot{}).Capacity(); !got.IsZero() {
		t.Errorf("建可能額 0 なのに %s", got)
	}
}

func TestIsFreshOnlyForTheSameDay(t *testing.T) {
	// 2026-09-17 の朝 8:53 JST（= 前日 23:53 UTC）に焼いたもの
	s := Snapshot{Day: "2026-09-17", FetchedAt: time.Date(2026, 9, 16, 23, 53, 0, 0, time.UTC)}
	if !s.IsFresh(day()) {
		t.Error("その日に焼いたものを古いと判定した")
	}
	if s.IsFresh(day().AddDate(0, 0, 1)) {
		t.Error("前日の保証金を使えると判定した")
	}
}

// 前夜に「翌営業日ぶん」として焼いたものは、日付が合っていても使わない。
// 代用有価証券の評価替え（夜間更新で確定）を取りこぼした値だから——実際に
// 2026-09-16 の 20:57 JST に day = 2026-09-17 のファイルが残っていた。
func TestIsFreshRejectsPreviousNightFetch(t *testing.T) {
	s := Snapshot{Day: "2026-09-17", FetchedAt: time.Date(2026, 9, 16, 11, 57, 0, 0, time.UTC)}
	if s.IsFresh(day()) {
		t.Error("前夜に焼いた保証金を当日ぶんとして使えると判定した")
	}
}

// 取得時刻の無い（古い形式の）キャッシュも使わない。
func TestIsFreshRejectsMissingFetchedAt(t *testing.T) {
	if (Snapshot{Day: "2026-09-17"}).IsFresh(day()) {
		t.Error("取得時刻の無いキャッシュを使えると判定した")
	}
}

func TestReadWriteRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "margin.json")
	if _, ok := Read(path); ok {
		t.Fatal("無いファイルを読めたことになっている")
	}
	want, err := Conservative(day(), realRows())
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok := Read(path)
	if !ok {
		t.Fatal("書いたものを読めない")
	}
	if !got.SinyouSinkidate.Equal(want.SinyouSinkidate) || got.Day != want.Day {
		t.Errorf("往復で変わった: %+v vs %+v", got, want)
	}
}

// 長短 3 : 2・両脚 N3 の設定。比と N の扱いを見るための土台で、本番の金額とは独立
// （本番は 2026-09-16 にロングだけ 500 万へ上げ、3 : 2 ではなくなっている）。
func prodLike() config.Config {
	cfg := config.Default()
	cfg.Capital.MaxCapital = dec("3000000")
	cfg.Capital.OrderBudget = dec("1000000")
	cfg.Capital.MaxOrder = dec("1500000")
	cfg.Margin.Enabled = true
	cfg.Margin.MaxCapital = dec("2000000")
	cfg.Margin.OrderBudget = dec("670000")
	cfg.Margin.MaxOrder = dec("1000000")
	return cfg
}

// 保証金に余裕がある日は何もしない——増えたぶんを勝手には使わない。
func TestApplyNeverRaises(t *testing.T) {
	cfg := prodLike()
	got, res := Apply(cfg, Snapshot{SinyouSinkidate: dec("11557051")})
	if res.Applied {
		t.Errorf("余裕があるのに上書きした: %s", res.Describe())
	}
	if !got.Capital.MaxCapital.Equal(dec("3000000")) || !got.Margin.MaxCapital.Equal(dec("2000000")) {
		t.Errorf("設定を変えてしまった: ロング %s / ショート %s", got.Capital.MaxCapital, got.Margin.MaxCapital)
	}
}

// 保証金が足りない日は下げる。**このとき N が変わってはいけない**。
func TestApplyShrinksAndKeepsN(t *testing.T) {
	cfg := prodLike()
	// 建可能額 390 万 → 建玉 300 万（ロング 180 万 / ショート 120 万）
	got, res := Apply(cfg, Snapshot{SinyouSinkidate: dec("3900000")})
	if !res.Applied {
		t.Fatal("足りないのに下げていない")
	}
	if got.Capital.MaxCapital.GreaterThan(dec("1800000")) {
		t.Errorf("ロングが下がりきっていない: %s", got.Capital.MaxCapital)
	}
	if got.Capital.Positions() != 3 || got.Margin.Positions() != 3 {
		t.Errorf("N が変わった: ロング %d / ショート %d, want 3 / 3",
			got.Capital.Positions(), got.Margin.Positions())
	}
	if res.Long.NBefore != res.Long.NAfter || res.Short.NBefore != res.Short.NAfter {
		t.Errorf("N の前後が食い違う: %+v %+v", res.Long, res.Short)
	}
	// 下げた設定がそのまま Validate を通ること（open は上書き後に検証し直す）
	if err := got.Validate(); err != nil {
		t.Errorf("下げた設定が Validate を通らない: %v", err)
	}
}

// 1 銘柄の上限も同率で縮む。0（上限なし）は触らない。
func TestApplyScalesMaxOrderButLeavesUnlimited(t *testing.T) {
	cfg := prodLike()
	got, _ := Apply(cfg, Snapshot{SinyouSinkidate: dec("3900000")})
	if !got.Capital.MaxOrder.LessThan(dec("1500000")) {
		t.Errorf("max_order が縮んでいない: %s", got.Capital.MaxOrder)
	}

	unlimited := prodLike()
	unlimited.Capital.MaxOrder = decimal.Zero
	got2, _ := Apply(unlimited, Snapshot{SinyouSinkidate: dec("3900000")})
	if !got2.Capital.MaxOrder.IsZero() {
		t.Errorf("上限なし（0）を書き換えた: %s", got2.Capital.MaxOrder)
	}
}

// 縮めた結果が 1 円未満に丸まっても 0（上限なし）に化けない。縮めるほど栓は締まる。
func TestScaleCapNeverTurnsIntoUnlimited(t *testing.T) {
	cases := []struct {
		name       string
		cap, ratio string
		want       string
	}{
		{"ふつうの縮小", "2500000", "0.6", "1500000"},
		{"端数は切り捨て", "1000001", "0.5", "500000"},
		{"ちょうど 1 円", "2500000", "0.0000004", "1"},
		{"1 円未満に丸まる", "2500000", "0.0000003", "1"},
		{"比が 0", "2500000", "0", "1"},
		{"もともと上限なし", "0", "0.0000003", "0"},
	}
	for _, c := range cases {
		got := scaleCap(dec(c.cap), dec(c.ratio))
		if !got.Equal(dec(c.want)) {
			t.Errorf("%s: scaleCap(%s, %s) = %s, want %s", c.name, c.cap, c.ratio, got, c.want)
		}
	}
}

// Apply を通しても同じ。保証金がほぼ無い日に max_order が 0 に落ちてはいけない。
func TestApplyKeepsMaxOrderCapWhenMarginIsTiny(t *testing.T) {
	got, _ := Apply(prodLike(), Snapshot{SinyouSinkidate: dec("1")})
	if !got.Capital.MaxOrder.IsPositive() {
		t.Errorf("ロングの max_order が上限なし（0）に化けた: %s", got.Capital.MaxOrder)
	}
	if !got.Margin.MaxOrder.IsPositive() {
		t.Errorf("ショートの max_order が上限なし（0）に化けた: %s", got.Margin.MaxOrder)
	}
}

// 様子見モード（max_capital = 0）には触らない。
func TestApplyLeavesWatchOnlyConfigAlone(t *testing.T) {
	cfg := prodLike()
	cfg.Capital.MaxCapital = decimal.Zero
	got, _ := Apply(cfg, Snapshot{SinyouSinkidate: dec("3900000")})
	if !got.Capital.MaxCapital.IsZero() {
		t.Errorf("様子見モードを書き換えた: %s", got.Capital.MaxCapital)
	}
}

// 追証が出ている日は印を付ける（呼び出し側が alert する）。
func TestApplyFlagsShortfall(t *testing.T) {
	_, res := Apply(prodLike(), Snapshot{SinyouSinkidate: dec("11557051"), Fusokugaku: dec("1")})
	if !res.Shortfall {
		t.Error("不足額があるのに印が付いていない")
	}
}

// 保証金がほぼ無い日は N が 0 に落ちる。**黙って続けてはいけない**ので印を付ける。
func TestApplyFlagsWatchOnlyCollapse(t *testing.T) {
	_, res := Apply(prodLike(), Snapshot{SinyouSinkidate: dec("1")})
	if !res.WatchOnly {
		t.Error("N が 0 に落ちたのに印が付いていない")
	}
}

// margin.capacity_ratio（規則 R）: 長短合計 = min(建可能額 × capacity_ratio（62%）, 天井 1,000 万) を上げ下げ両方に当てる。
// 本番の設定と 2026-09-24 朝の実際の建可能額で確かめる。
func TestApplyRatioLiveConfig(t *testing.T) {
	cfg, err := config.Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if !cfg.Margin.CapacityRatio.IsPositive() {
		t.Skip("margin.capacity_ratio が無い設定")
	}
	for _, c := range []struct {
		name                         string
		sinkidate, fusoku            string
		wantLong, wantShort          string
		wantNormal, wantShock        string
		wantRatio, wantWatch, change bool
	}{
		// 1,115 万 × 68% = 758 万。ショートは長短比 2:7 で 216 万 → 上限 200 万、残り 558 万がロング
		{name: "今朝の値", sinkidate: "11150590", wantLong: "4938117", wantShort: "1975248",
			wantNormal: "6913365", wantShock: "8028424", wantRatio: true, change: true},
		// 天井 1,000 万で止まる（ショック日も天井を超えない）
		{name: "保証金が増えた", sinkidate: "20000000", wantLong: "8000000", wantShort: "2000000",
			wantNormal: "10000000", wantShock: "10000000", wantRatio: true, change: true},
		// 小さい朝はショートを長短比で割る（ロングが 0 にならない）
		{name: "保証金が小さい", sinkidate: "2000000", wantLong: "885714", wantShort: "354286",
			wantNormal: "1240000", wantShock: "1440000", wantRatio: true, change: true},
		// 追証の日は建てない
		{name: "追証", sinkidate: "11150590", fusoku: "1000", wantLong: "0", wantShort: "0",
			wantNormal: "0", wantShock: "0", wantRatio: true, wantWatch: true, change: true},
		// 当日ぶんのキャッシュで建可能額が 0 なら建てない（取れない朝は applyMarginCap が設定の値に落とす）
		{name: "建可能額 0", sinkidate: "0", wantLong: "0", wantShort: "0",
			wantNormal: "0", wantShock: "0", wantRatio: true, wantWatch: true, change: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := Snapshot{Day: "2026-09-24", SinyouSinkidate: dec(c.sinkidate)}
			if c.fusoku != "" {
				s.Fusokugaku = dec(c.fusoku)
			}
			got, res := Apply(cfg, s)
			if got.Capital.MaxCapital.String() != c.wantLong || got.Margin.MaxCapital.String() != c.wantShort {
				t.Errorf("ロング %s ショート %s, want %s / %s", got.Capital.MaxCapital, got.Margin.MaxCapital, c.wantLong, c.wantShort)
			}
			if res.NormalTotal.String() != c.wantNormal || res.ShockTotal.String() != c.wantShock {
				t.Errorf("合計 平日 %s ショック %s, want %s / %s", res.NormalTotal, res.ShockTotal, c.wantNormal, c.wantShock)
			}
			if !got.Capital.ShockTotalCap.Equal(res.ShockTotal) {
				t.Errorf("ShockTotalCap %s, want %s", got.Capital.ShockTotalCap, res.ShockTotal)
			}
			if res.Ratio != c.wantRatio || res.WatchOnly != c.wantWatch || res.Applied != c.change {
				t.Errorf("ratio %v watch %v applied %v, want %v / %v / %v", res.Ratio, res.WatchOnly, res.Applied, c.wantRatio, c.wantWatch, c.change)
			}
			// 長短の合計は平日の上限を超えない
			if c.wantRatio && got.Capital.MaxCapital.Add(got.Margin.MaxCapital).GreaterThan(res.NormalTotal) {
				t.Errorf("長短合計 %s が上限 %s を超えた", got.Capital.MaxCapital.Add(got.Margin.MaxCapital), res.NormalTotal)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("適用後の設定が検証を通らない: %v", err)
			}
			if !c.wantWatch && got.Capital.Positions() != cfg.Capital.MaxPositions {
				t.Errorf("ロング N = %d, want max_positions %d", got.Capital.Positions(), cfg.Capital.MaxPositions)
			}
		})
	}
}
