package main

// 委託保証金から建玉の上限を安全側へ寄せる経路。
//
// 前夜（plan、20:30）に立花証券へ繋いで保証金を data/daytrade/margin.json へ焼き、
// 朝（open、9:01）はそのファイルを読むだけ——寄付の判断にブローカーの遅さを持ち込まない。
// VIX（usmarket）と同じ約束で、取れなければ設定の値で建てる。

import (
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/margincap"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
)

// marginCachePath は保証金キャッシュの置き場。
func marginCachePath() string { return margincap.DefaultCachePath(appSettings.DataDir) }

// marginCapNeeded は保証金を見る設定か（どちらかの脚が建てる設定なら要る）。
func marginCapNeeded(cfg dtconfig.Config) bool {
	return cfg.Capital.Positions() > 0 || cfg.Margin.Positions() > 0
}

// warmMargin は前夜に委託保証金を取ってキャッシュに焼く。
// 失敗しても plan は成功——取れなければ朝は設定の値で建てる（建てられなくはしない）。
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
		warn("保証金を取れない（ブローカーに繋げない。朝は設定の値で建てる）", err)
		return
	}
	source, ok := b.(broker.MarginSource)
	if !ok {
		logWarn("daytrade.margin_warm", "このブローカーは保証金を返さない（朝は設定の値で建てる）", fields)
		return
	}
	rows, err := source.MarginSummaries()
	if err != nil {
		warn("保証金の照会に失敗（朝は設定の値で建てる）", err)
		return
	}
	snapshot, err := margincap.Conservative(day, rows)
	if err != nil {
		warn("保証金の内訳を読めない（朝は設定の値で建てる）", err)
		return
	}
	if err := margincap.Write(marginCachePath(), snapshot); err != nil {
		warn("保証金をキャッシュに書けない（朝は設定の値で建てる）", err)
		return
	}

	fields["snapshot"] = snapshot.Describe()
	fields["source_date"] = snapshot.SourceDate
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
	logInfo("daytrade.margin_warm", "保証金をキャッシュに焼いた", fields)
}

// applyMarginCap は前夜の保証金で建玉の上限を下げる。**下げ方向のみ**。
//
// 設定は狙いの水準で、ここは「その日それが本当に建てられるか」の検算。
// 保証金が増えていても勝手には上げない（増えたぶんを使うかは人が決める）。
// キャッシュが無い・古い・下げた結果が検証を通らない、のいずれでも元の設定を返す
// ——保証金が読めないことを理由に売買を止めない。
func applyMarginCap(cfg dtconfig.Config, day time.Time) dtconfig.Config {
	if !marginCapNeeded(cfg) {
		return cfg
	}
	fields := map[string]any{"day": day.Format(DateLayout)}

	snapshot, ok := margincap.Read(marginCachePath())
	if !ok {
		logWarn("daytrade.margin_cap", "保証金のキャッシュが無い（設定の値で建てる）", fields)
		return cfg
	}
	if !snapshot.IsFresh(day) {
		fields["cached_day"] = snapshot.Day
		logWarn("daytrade.margin_cap", "保証金のキャッシュが当日ぶんでない（設定の値で建てる）", fields)
		return cfg
	}

	capped, res := margincap.Apply(cfg, snapshot)
	for k, v := range res.Fields() {
		fields[k] = v
	}
	fields["snapshot"] = snapshot.Describe()

	if res.Shortfall {
		alert("daytrade: 追証", "委託保証金に不足額が出ています（"+snapshot.Fusokugaku.StringFixed(0)+" 円）")
	}
	// 上書きは Load の外なので Validate は走っていない。ここで確かめ直す
	// ——通らない設定で建てるくらいなら、元の（検証済みの）設定で建てる
	if err := capped.Validate(); err != nil {
		fields["error"] = err.Error()
		logError("daytrade.margin_cap", "保証金で下げた設定が検証を通らない（設定の値で建てる）", fields)
		alert("daytrade: 保証金の上書きに失敗", "下げた設定が Validate を通りませんでした: "+err.Error())
		return cfg
	}
	// N が 0 に落ちると open は watch-only に転ぶ。**黙って「何もしない朝」にしない**
	if res.WatchOnly {
		logWarn("daytrade.margin_cap", "保証金が足りず N が 0 になった（今日は建てない）", fields)
		alert("daytrade: 保証金不足で建てません",
			"保証金から導いた建玉が 1 注文に届かず、今日は何も建てません。"+snapshot.Describe())
		return capped
	}
	if !res.Applied {
		logInfo("daytrade.margin_cap", "保証金による縮小なし", fields)
		return capped
	}
	logWarn("daytrade.margin_cap", res.Describe(), fields)
	return capped
}
