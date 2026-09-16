// Package margincap は委託保証金から「その日に建てられる額」を導き、設定の上限を安全側へ寄せる。
//
// 設定（config）は**狙いの水準**で、ここが出すのは**その日それが本当に建てられるかの検算**。
// したがって上書きは常に**下げ方向のみ**——保証金が増えたぶんを使うかどうかは人が決める
// （代用有価証券の ETF は日本株と一緒に下がるので、下げた日に建玉も縮めると
// 底で縮小して戻りを取り逃す。研究ノート 2026-09-daytrade-collateral-etf）。
//
// 前夜に plan が焼き、朝の open が読む（data/daytrade/margin.json）。取れなければ設定の値で建てる
// ——保証金が読めないことを理由に売買を止めない。usmarket（VIX）と同じ約束。
package margincap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

const dateLayout = "2006-01-02"

// SafetyFactor は建玉に対する保証金の余裕。ショック日の目減り（最悪 −11%）に備える。
//
// 1.3 は研究ノート 2026-09-daytrade-collateral-etf の採用値。締めすぎは高くつく
// （9 年の複利試算で 1.3 → 1.94 億、2.0 → 1.43 億、3.0 → 9,891 万）。
var SafetyFactor = decimal.RequireFromString("1.3")

// Snapshot は前夜に焼いた保証金の状態（キャッシュのファイル形式）。
type Snapshot struct {
	// Day は判定日（この保証金で建てる営業日）。
	Day string `json:"day"`
	// SinyouSinkidate は信用新規建可能額。建玉の上限を導く起点。
	// 受入保証金 ÷ 委託保証金率が証券会社側で計算済みの値なので、保証金率を自分で持たなくてよい。
	SinyouSinkidate decimal.Decimal `json:"sinyou_sinkidate"`
	// UkeireHosyoukin / GenkinHosyoukin / DaiyouHyoukagaku は内訳（記録と点検のため）。
	UkeireHosyoukin  decimal.Decimal `json:"ukeire_hosyoukin"`
	GenkinHosyoukin  decimal.Decimal `json:"genkin_hosyoukin"`
	DaiyouHyoukagaku decimal.Decimal `json:"daiyou_hyoukagaku"`
	// SonotaKousokukin は「その他拘束金」の最大。**正体が分かっていない**まま枠を削るので必ず残す
	// （docs/BROKER_VERIFY.md。2026-09-14 に 1,000 円、2026-09-18 に 3,211 円）。
	SonotaKousokukin decimal.Decimal `json:"sonota_kousokukin"`
	// Fusokugaku は不足額（追証）の最大。0 でなければ異常として扱う。
	Fusokugaku decimal.Decimal `json:"fusokugaku"`
	// Missing は応答に無かった項目名。**不足額（追証）がここに出ていたら、
	// 「追証の日は建てない」は効いていない**（2026-09-17 のレビュー）。
	Missing []string `json:"missing,omitempty"`
	// SourceDate はこの数字が出てきた受渡日（YYYYMMDD）。最小を採った行の日付。
	SourceDate string `json:"source_date"`
	// FetchedAt は取得時刻。
	FetchedAt time.Time `json:"fetched_at"`
}

// Conservative は受渡日ごとの内訳から、いちばん厳しい 1 日ぶんを選ぶ。
//
// 当日の行だけを見ないのは、「その他拘束金」が後日の行に出るため（実機では当日 0 で 2 日後に 3,211 円）。
// 建てられる額は最小を採り、拘束金と不足額は最大を採る——どちらも安全側。
func Conservative(day time.Time, rows []domain.MarginSummary) (Snapshot, error) {
	if len(rows) == 0 {
		return Snapshot{}, fmt.Errorf("保証金の内訳が 1 日ぶんも返っていません")
	}
	out := Snapshot{Day: day.Format(dateLayout), FetchedAt: time.Now().UTC()}
	found := false
	missing := map[string]bool{}
	for _, r := range rows {
		for _, name := range r.Missing {
			if !missing[name] {
				missing[name] = true
				out.Missing = append(out.Missing, name)
			}
		}
		if !found || r.SinyouSinkidate.LessThan(out.SinyouSinkidate) {
			out.SinyouSinkidate = r.SinyouSinkidate
			out.UkeireHosyoukin = r.UkeireHosyoukin
			out.GenkinHosyoukin = r.GenkinHosyoukin
			out.DaiyouHyoukagaku = r.DaiyouHyoukagaku
			out.SourceDate = r.Date
			found = true
		}
		if r.SonotaKousokukin.GreaterThan(out.SonotaKousokukin) {
			out.SonotaKousokukin = r.SonotaKousokukin
		}
		if r.Fusokugaku.GreaterThan(out.Fusokugaku) {
			out.Fusokugaku = r.Fusokugaku
		}
	}
	return out, nil
}

// Capacity は建てられる額。ロングとショートに割り振る前の合計。
//
//	建玉合計 = 信用新規建可能額 ÷ SafetyFactor
//
// 脚への割り振りは設定の max_capital の比で決める（legTargets）。ここに比を持たないのは、
// 長短比が設定側の判断だから——ハードコードすると、縮小した日だけ設定と違う比に
// 引き戻されることになる。
func (s Snapshot) Capacity() decimal.Decimal {
	if s.SinyouSinkidate.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	return s.SinyouSinkidate.Div(SafetyFactor).Floor()
}

// IsFresh は判定日ぶんとして使えるか（その日に**その日のうちに**焼いたものだけ）。
// 古い保証金で建てない——増資も評価損も反映されていない値で枠を広げるのは危険。
//
// day だけでなく fetched_at も見るのは、前夜に「翌営業日ぶん」として焼いたファイルが
// 翌朝そのまま当日ぶんとして通るため。代用有価証券の評価替え（前営業日終値 × 掛目、
// 夜間更新で確定）を取りこぼした値で建ててしまう——8:53 の warm-margin が失敗した朝に
// 前夜の値へ黙って落ちるのは、取得を朝へ移した意味を消す（2026-09-16 のレビュー）。
func (s Snapshot) IsFresh(day time.Time) bool {
	if s.Day != day.Format(dateLayout) {
		return false
	}
	if s.FetchedAt.IsZero() {
		return false
	}
	return clock.ToZone(s.FetchedAt, clock.Tokyo).Format(dateLayout) == s.Day
}

// Describe はログ用の 1 行。
func (s Snapshot) Describe() string {
	return fmt.Sprintf("受入保証金 %s（現金 %s + 代用 %s）/ 建可能額 %s → 建玉 %s",
		s.UkeireHosyoukin.StringFixed(0), s.GenkinHosyoukin.StringFixed(0),
		s.DaiyouHyoukagaku.StringFixed(0), s.SinyouSinkidate.StringFixed(0),
		s.Capacity().StringFixed(0))
}

// DefaultCachePath は data ディレクトリ配下の置き場。
func DefaultCachePath(dataDir string) string {
	return filepath.Join(dataDir, "daytrade", "margin.json")
}

// Read はキャッシュを読む。無ければ ok = false。
func Read(path string) (Snapshot, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, false
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil || s.Day == "" {
		return Snapshot{}, false
	}
	return s, true
}

// Write はキャッシュを書く。一時ファイルに書いて rename（途中で落ちても壊れたキャッシュを残さない）。
func Write(path string, s Snapshot) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
