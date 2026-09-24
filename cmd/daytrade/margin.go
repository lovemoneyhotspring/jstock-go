package main

// 委託保証金から建玉の上限を安全側へ寄せる経路。
//
// 朝 8:53（`daytrade warm-margin`）に立花証券へ繋いで保証金を data/daytrade/margin.json へ焼き、
// 9:01 の open はそのファイルを読むだけ——寄付の判断にブローカーの遅さを持ち込まない。
// VIX（usmarket）と同じ約束で、取れなければ設定の値で建てる。
//
// **前夜ではなく朝に取る。** 代用有価証券の評価は前営業日終値 × 掛目で、受入保証金は
// 夜間更新（5:30 頃）に確定する。前夜の値では ETF の評価替えを取りこぼす。

import (
	"fmt"
	"os"
	"strings"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/margincap"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newWarmMarginCmd() *cobra.Command {
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "warm-margin",
		Short: "委託保証金を照会してキャッシュに焼く（朝 8:53 に cron で回す）。9:01 の open はこれを読む",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if !cfg.Capital.Enabled {
				fmt.Println("jp_gap_fade は無効（capital.enabled = false）。何もしません")
				logInfo("daytrade.skip", "戦略が無効", map[string]any{"reason": "disabled"})
				return nil
			}
			day, err := dayOrToday(dateFlag, clock.NowUTC())
			if err != nil {
				return err
			}
			if skipHoliday(day, "warm-margin") {
				return nil
			}
			warmMargin(cfg, day)
			return nil
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

// marginCachePath は保証金キャッシュの置き場。
func marginCachePath() string { return margincap.DefaultCachePath(appSettings.DataDir) }

// marginCapNeeded は保証金を見る設定か（どちらかの脚が建てる設定なら要る）。
func marginCapNeeded(cfg dtconfig.Config) bool {
	return cfg.Capital.Positions() > 0 || cfg.Margin.Positions() > 0
}

// warmMargin は委託保証金を取ってキャッシュに焼く。
// 失敗しても落とさない——取れなければ open は設定の値で建てる（建てられなくはしない）。
func warmMargin(cfg dtconfig.Config, day time.Time) {
	if !marginCapNeeded(cfg) {
		return
	}
	fields := map[string]any{"day": day.Format(DateLayout)}
	warn := func(msg string, err error) {
		fields["error"] = err.Error()
		logWarn("daytrade.margin_warm", msg, fields)
	}

	b, err := connectBroker(cfg)
	if err != nil {
		warn("保証金を取れない（ブローカーに繋げない。open は設定の値で建てる）", err)
		return
	}
	// 締め切りを持たせる。daytrade のコマンドでここだけ SetDeadline を呼んでおらず、
	// 電文 1 本 30 秒 + 再送で最悪 90 秒 /tmp/daytrade.lock を握る——8:55 の snap は
	// 待たない（wait 0）ので締め出され、板の記録が 1 スロット欠ける（2026-09-16 のレビュー）
	broker.SetDeadline(b, clock.NowUTC().Add(60*time.Second))
	source, ok := b.(broker.MarginSource)
	if !ok {
		logWarn("daytrade.margin_warm", "このブローカーは保証金を返さない（open は設定の値で建てる）", fields)
		return
	}
	rows, err := source.MarginSummaries()
	if err != nil {
		warn("保証金の照会に失敗（open は設定の値で建てる）", err)
		return
	}
	snapshot, err := margincap.Conservative(day, rows)
	if err != nil {
		warn("保証金の内訳を読めない（open は設定の値で建てる）", err)
		return
	}
	if err := margincap.Write(marginCachePath(), snapshot); err != nil {
		warn("保証金をキャッシュに書けない（open は設定の値で建てる）", err)
		return
	}
	// 日付つきの控え（evaluate が順位表の無い日を作り直すときに、その日の総額を決め直す）。失敗しても open には効かない
	if err := margincap.Write(margincap.DatedCachePath(appSettings.DataDir, snapshot.Day), snapshot); err != nil {
		fields["error"] = err.Error()
		logWarn("daytrade.margin_warm", "保証金の日付つきの控えを書けない（evaluate の作り直しが設定の値になる）", fields)
		delete(fields, "error")
	}

	fields["snapshot"] = snapshot.Describe()
	fields["source_date"] = snapshot.SourceDate
	// 応答に無かった項目。**不足額（追証）が欠けていれば「追証の日は建てない」は効いていない**
	// ——この電文での項目名は実機で確認できていない（docs/BROKER_VERIFY.md。2026-09-17 のレビュー）
	if len(snapshot.Missing) > 0 {
		fields["missing"] = snapshot.Missing
		logWarn("daytrade.margin_warm", "保証金の応答に無い項目がある（0 と読んでいる）", fields)
	}
	// 正体の分かっていない拘束金。枠をそのまま削るので、出ていたら必ず残す
	if snapshot.SonotaKousokukin.IsPositive() {
		fields["sonota_kousokukin"] = snapshot.SonotaKousokukin.StringFixed(0)
	}
	if snapshot.Fusokugaku.IsPositive() {
		fields["fusokugaku"] = snapshot.Fusokugaku.StringFixed(0)
		logWarn("daytrade.margin_warm", "不足額（追証）が出ている", fields)
		alert("daytrade: 追証", "委託保証金に不足額が出ています: "+snapshot.Fusokugaku.StringFixed(0)+" 円")
		return
	}
	fmt.Println(snapshot.Describe())
	logInfo("daytrade.margin_warm", "保証金をキャッシュに焼いた", fields)
}

// applyMarginCap は朝の保証金で建玉の上限を決める。向きは設定で違う:
//   - margin.capacity_ratio なし: **下げ方向のみ**。設定は狙いの水準で、ここは「その日それが本当に
//     建てられるか」の検算。保証金が増えていても勝手には上げない（増えたぶんを使うかは人が決める）
//   - margin.capacity_ratio あり（規則 R）: 建可能額の比で**上げ下げ両方**に決め直す（margincap.Apply → applyRatio）。
//     キャッシュが無い・古い朝は ratioFallback（設定の値か前日の値の小さい方。ショック日も同額で頭打ち）
//
// 下げた結果が検証を通らない朝は**建てない**（watchOnlyConfig）。保証金は読めていて、その値が設定より
// 小さいと言っている朝なので、元の設定（満額）に戻すのは逆向き（2026-09-25 のレビュー R1）。
// 保証金が読めない朝は従来どおり設定の値で建てる（規則 R では ratioFallback で下げ方向のみ）。
func applyMarginCap(cfg dtconfig.Config, day time.Time) dtconfig.Config {
	if !marginCapNeeded(cfg) {
		return cfg
	}
	fields := map[string]any{"day": day.Format(DateLayout)}

	snapshot, ok := margincap.Read(marginCachePath())
	if !ok {
		logWarn("daytrade.margin_cap", "保証金のキャッシュが無い（設定の値で建てる。規則 R では ratioFallback で長短合計を設定の固定値に、ショック日も同額で頭打ち）", fields)
		return ratioFallback(cfg, day, nil, fields)
	}
	if !snapshot.IsFresh(day) {
		fields["cached_day"] = snapshot.Day
		logWarn("daytrade.margin_cap", "保証金のキャッシュが当日ぶんでない（設定の値で建てる。規則 R では ratioFallback で設定の固定値とこのキャッシュの値の小さい方に決め直す。追証・建可能額 0 なら建てない）", fields)
		return ratioFallback(cfg, day, &snapshot, fields)
	}

	capped, res := margincap.Apply(cfg, snapshot)
	// 縮小した後の設定でも、ショック日の倍率まで含めると枠を超えることがある
	// （比で決め直す margin.capacity_ratio ではショック日の総額を SizeDay が頭打ちにするので見ない）
	if over, total := margincap.ShockExceeds(capped, snapshot); over && !res.Ratio {
		fields["shock_total"] = total.StringFixed(0)
		logWarn("daytrade.margin_cap", "ショック日の倍率を掛けると建玉が保証金から導いた上限を超える", fields)
	}
	for k, v := range res.Fields() {
		fields[k] = v
	}
	fields["snapshot"] = snapshot.Describe()

	if res.Shortfall {
		alert("daytrade: 追証", "委託保証金に不足額が出ています（"+snapshot.Fusokugaku.StringFixed(0)+" 円）")
	}
	// 上書きは Load の外なので Validate は走っていない。ここで確かめ直す
	// ——通らない設定では建てない。元の設定に戻すと、小さく建てるべき朝ほど満額で建てる（R1）
	if out, err := validOrWatchOnly(cfg, capped); err != nil {
		fields["error"] = err.Error()
		logError("daytrade.margin_cap", "保証金で決め直した設定が検証を通らない（今日は建てない）", fields)
		alert("daytrade: 保証金の上書きに失敗",
			"決め直した設定が Validate を通らないので、今日は建てません: "+err.Error()+"。"+snapshot.Describe())
		return out
	}
	// N が 0 に落ちると open は watch-only に転ぶ。**黙って「何もしない朝」にしない**
	if res.WatchOnly {
		logWarn("daytrade.margin_cap", "保証金が足りず N が 0 になった（今日は建てない）", fields)
		alert("daytrade: 保証金不足で建てません",
			"保証金から導いた建玉が 1 注文に届かず、今日は何も建てません。"+snapshot.Describe())
		return capped
	}
	if res.Ratio {
		logInfo("daytrade.margin_cap", res.Describe(), fields)
		fmt.Println(res.Describe())
		return capped
	}
	if !res.Applied {
		logInfo("daytrade.margin_cap", "保証金による縮小なし", fields)
		return capped
	}
	logWarn("daytrade.margin_cap", res.Describe(), fields)
	return capped
}

// watchOnlyConfig は建てない設定（両脚の資金 0 → N = 0 → open は watch-only）。
// 保証金で決め直した設定が使えない朝の安全側。元の設定（満額）には戻さない（2026-09-25 のレビュー R1）。
func watchOnlyConfig(cfg dtconfig.Config) dtconfig.Config {
	cfg.Capital.MaxCapital = decimal.Zero
	cfg.Capital.ShockTotalCap = decimal.Zero
	cfg.Margin.MaxCapital = decimal.Zero
	return cfg
}

// validOrWatchOnly は保証金で決め直した設定 capped が検証を通ればそれを、通らなければ建てない設定と
// その理由を返す。**通らないときに元の設定 orig（満額）を返す経路を作らない**ための一本道。
func validOrWatchOnly(orig, capped dtconfig.Config) (dtconfig.Config, error) {
	if err := capped.Validate(); err != nil {
		return watchOnlyConfig(orig), err
	}
	return capped, nil
}

// ratioFallback は規則 R（margin.capacity_ratio）で当日の保証金が読めない朝（8:53 と 8:56 の取得が
// 両方失敗）の設定。**下げる方向にだけ**動かす:
//   - 前の日のキャッシュがあれば、それで決め直した総額が設定の固定値より小さいときだけ使う
//     （前日から保証金が減っていれば、少なくともその分は控える）
//   - 前の日のキャッシュが追証・建可能額 0（決め直すと N = 0）なら建てない。当日の値が読めない以上、
//     分かっている最新の値は「建てられない」で、固定値の満額に戻す理由が無い（2026-09-25 のレビュー）
//   - ショック日の総額は設定の固定値（長短合計）で頭打ち。倍率で固定値を超えて建てない
//     ——保証金が分からない日に、分かっている日より大きく建てる理由が無い
//   - 人に知らせる（1 日 1 回）。固定値は建可能額 × capacity_ratio の目安で置いた値で、保証金が大きく減った
//     翌朝は建可能額を超えうる（2026-09-25 のレビュー）
//
// 比を置いていない設定では何もしない（従来どおり設定の値で建てる）。
func ratioFallback(cfg dtconfig.Config, day time.Time, stale *margincap.Snapshot, fields map[string]any) dtconfig.Config {
	if !cfg.Margin.CapacityRatio.IsPositive() {
		return cfg
	}
	out, total, fromStale := ratioFallbackConfig(cfg, stale)
	if fromStale {
		fields["stale_total"] = total.StringFixed(0)
	}
	fields["fallback_total"] = total.StringFixed(0)
	fields["shock_total_cap"] = out.Capital.ShockTotalCap.StringFixed(0)
	msg := fmt.Sprintf("当日の保証金が読めないので、長短合計 %s 円（ショック日も同額で頭打ち）で建てます", yen(total))
	if !total.IsPositive() {
		msg = "当日の保証金が読めず、前の日の保証金が追証か建可能額 0 なので、今日は建てません"
	}
	fmt.Println(msg)
	logWarn("daytrade.margin_cap", "規則 R: 保証金が読めない（"+msg+"）", fields)
	if markOncePerDay(marginCachePath()+".fallback-alerted", day) {
		alert("daytrade: 保証金が読めません", msg+"。8:53・8:56 の warm-margin が失敗しています（state/logs/daytrade-margin.log）")
	}
	return out
}

// ratioFallbackConfig は ratioFallback の設定の部分（通知・ログなし）。長短合計と、前の日の
// キャッシュで決め直したか（fromStale）も返す。
func ratioFallbackConfig(cfg dtconfig.Config, stale *margincap.Snapshot) (out dtconfig.Config, total decimal.Decimal, fromStale bool) {
	total = cfg.Capital.MaxCapital
	if cfg.Margin.Enabled {
		total = total.Add(cfg.Margin.MaxCapital)
	}
	out = cfg
	if stale != nil {
		capped, res := margincap.Apply(cfg, *stale)
		switch {
		case res.Shortfall || res.WatchOnly || !res.NormalTotal.IsPositive():
			// 前の日が追証・建可能額 0 → 建てない（固定値の満額に戻さない）
			return watchOnlyConfig(cfg), decimal.Zero, true
		case res.NormalTotal.LessThan(total):
			// 前の日の値で決め直した設定が通らない → 満額でなく建てない側へ（R1）
			valid, err := validOrWatchOnly(cfg, capped)
			if err != nil {
				return valid, decimal.Zero, true
			}
			out, total, fromStale = valid, res.NormalTotal, true
		}
	}
	if !out.Capital.ShockTotalCap.IsPositive() || out.Capital.ShockTotalCap.GreaterThan(total) {
		out.Capital.ShockTotalCap = total
	}
	return out, total, fromStale
}

// markOncePerDay は path に当日の日付を書き、その日初めてなら真。書けなければ真（通知を落とさない側）。
func markOncePerDay(path string, day time.Time) bool {
	today := day.Format(DateLayout)
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == today {
		return false
	}
	_ = os.WriteFile(path, []byte(today+"\n"), 0o644)
	return true
}
