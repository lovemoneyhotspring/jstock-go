package main

import (
	"fmt"
	"strings"
	"time"

	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	dtquotes "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/quotes"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// 寄る前の回の「候補の外の板」の記録（open --presnap-until）と、候補の気配を取る時刻（--quotes-at）。
//
// 気配の誤差（見えるギャップ − 始値のギャップ）の検証は、発注の判断に使った気配（history/quotes）が
// 一番本番に近い。ただ open は候補（約 900 銘柄）しか取らないので、深いギャップの帯は行が足りない
// （2026-09-25）。そこで寄る前の回だけ、候補を撮る前に候補の外（約 2,800 銘柄）の板を撮って
// history/book に残す（slot は起動の時刻 HHMMSS）。**記録だけで、選定にも発注にも使わない。**
//
// 時価問合の送信枠（8 回/秒）はプロセスで 1 つなので、候補の外を撮り続けたまま候補を撮ると枠待ちで
// 候補の取得が遅れる。候補の外は --presnap-until で打ち切り（別の接続に締め切りを掛ける。発注の接続の
// 締め切りには触らない）、--quotes-at まで待ってから候補を撮る。打ち切りから候補までを 1 秒空ければ
// 枠は空く。例（crontab）: 起動 8:59:48.0、打ち切り 8:59:51.2、候補 8:59:52.2。
//
// 失敗しても発注は止めない（警告を残して続ける）。

// presnapFetch は候補の外の板を until まで取る（取れた行・行ごとの受信時刻・取れなかった銘柄数・誤り）。
// テストで差し替える。
var presnapFetch = func(symbols []string, columns string, until time.Time) ([]map[string]any, []time.Time, int, error) {
	t, err := dtquotes.Connect(appSettings.Env, appSettings.DotenvMap, appSettings.StateDir)
	if err != nil {
		return nil, nil, 0, err
	}
	t.Broker.SetLogger(run)
	t.Broker.SetDeadline(until)
	rows, received, failed := t.Broker.MarketPricesRawPartialAt(symbols, columns)
	missing := 0
	for _, f := range failed {
		missing += len(f.Symbols)
	}
	return rows, received, missing, nil
}

// presnapSourceOK は候補の外の板を撮れる気配の取得元か（立花だけ。csv は板を取れない）。テストで差し替える。
var presnapSourceOK = func(name string) bool { return name == "tachibana" }

// waitUntil は t まで寝る。テストで差し替える。
var waitUntil = func(t time.Time) {
	if d := time.Until(t); d > 0 {
		time.Sleep(d)
	}
}

// todayAt は "HH:MM:SS(.f)"（JST）を判定日の時刻にする。空なら ok=false。
func todayAt(day time.Time, hms string) (time.Time, bool, error) {
	hms = strings.TrimSpace(hms)
	if hms == "" {
		return time.Time{}, false, nil
	}
	for _, layout := range []string{"15:04:05.999999999", "15:04:05"} {
		t, err := time.ParseInLocation(layout, hms, jst)
		if err != nil {
			continue
		}
		y, m, d := day.In(jst).Date()
		return time.Date(y, m, d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), jst), true, nil
	}
	return time.Time{}, false, fmt.Errorf("時刻は HH:MM:SS(.f)（JST）で: %q", hms)
}

// presnapOthers は寄る前の回で、候補（readQuotes が取る銘柄）の外の板を --presnap-until まで撮って記録する。
func (s *openState) presnapOthers() {
	if s.opts.presnapUntil == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logWarn("daytrade.presnap", "候補の外の板の記録が異常終了（発注は続ける）", map[string]any{"error": fmt.Sprint(r)})
		}
	}()
	until, ok, err := todayAt(s.day, s.opts.presnapUntil)
	if err != nil || !ok {
		logWarn("daytrade.presnap", "--presnap-until を読めないので候補の外の板は撮らない", map[string]any{"value": s.opts.presnapUntil})
		return
	}
	if !s.env.Preopen {
		return // 寄る前の回だけ（9:00 以降の回は候補を撮るのが遅れるだけ）
	}
	if name := s.quoteSourceName(); !presnapSourceOK(name) {
		return // csv（検証・テスト）は板を取れない
	}
	started := clock.NowUTC()
	if !started.Before(until) {
		logWarn("daytrade.presnap", "打ち切りの時刻を過ぎていたので候補の外の板は撮らない",
			map[string]any{"until": until.In(jst).Format("15:04:05.000"), "started": started.In(jst).Format("15:04:05.000")})
		return
	}
	candidates := map[string]bool{}
	quoteSyms, _, _ := s.quoteSymbols()
	for _, sym := range quoteSyms {
		candidates[sym] = true
	}
	all, _, err := snapSymbols(s.cfg.Book.Scope, "", s.day, s.cfg.Book.ExtraSymbols)
	if err != nil {
		logWarn("daytrade.presnap", "候補の外の銘柄を集められない", map[string]any{"error": err.Error()})
		return
	}
	var others []string
	for _, sym := range all {
		if !candidates[sym] {
			others = append(others, sym)
		}
	}
	if len(others) == 0 {
		return
	}
	rows, received, missing, err := presnapFetch(others, s.cfg.Book.Columns, until)
	slot := started.In(jst).Format("150405")
	fields := map[string]any{
		"slot": slot, "requested": len(others), "rows": len(rows), "missing": missing,
		"until": until.In(jst).Format("15:04:05.000"), "elapsed_ms": clock.NowUTC().Sub(started).Milliseconds(),
	}
	if err != nil {
		fields["error"] = err.Error()
		logWarn("daytrade.presnap", "候補の外の板を取れない（発注は続ける）", fields)
		return
	}
	if len(rows) > 0 {
		fields["path"] = appendHistory(dthistory.KindBook, dthistory.BookFrame(rows, received, slot, started), s.day)
	}
	// 打ち切りで残りを落とすのは設計どおりなので info（全部取れた日と見分けるのは missing）
	logInfo("daytrade.presnap", "候補の外の板を記録", fields)
	fmt.Printf("候補の外の板: %d/%d 銘柄を記録（%s まで、slot %s）\n", len(rows), len(others), until.In(jst).Format("15:04:05.0"), slot)
}

// waitQuotesAt は --quotes-at まで待つ（候補の気配を寄りに近い値にし、送信枠を空けるため）。
func (s *openState) waitQuotesAt() {
	at, ok, err := todayAt(s.day, s.opts.quotesAt)
	if err != nil {
		logWarn("daytrade.presnap", "--quotes-at を読めないので待たずに気配を取る", map[string]any{"value": s.opts.quotesAt})
		return
	}
	if !ok || !s.env.Preopen {
		return
	}
	// 締め切り（寄る前の回は 9:00:00）より後まで寝て発注を逃さない
	if !s.deadline.IsZero() && !at.Before(s.deadline) {
		logWarn("daytrade.presnap", "--quotes-at が締め切りより後なので待たない", map[string]any{"value": s.opts.quotesAt})
		return
	}
	waitUntil(at)
}

// quoteSourceName は気配の取得元（--quote-source があればそれ、無ければ設定）。
func (s *openState) quoteSourceName() string {
	if s.opts.quoteSource != "" {
		return s.opts.quoteSource
	}
	return s.cfg.Execution.QuoteSource
}
