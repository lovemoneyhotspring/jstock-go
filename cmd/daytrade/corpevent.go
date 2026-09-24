package main

import (
	"context"
	"fmt"
	"os"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/news"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// corpEvents は記録簿（news の TDnet 適時開示）から判定した、その日の材料の印。
type corpEvents struct {
	// marks は銘柄 → 外す種類の印（窓の中で最後の開示）。
	marks map[string]dtplan.CorpEvent
	// notes は銘柄 → 記録だけの種類（一部報道・買集め・自己株式の公開買付）の最後の開示。
	notes map[string]dtplan.CorpEvent
	// lastFetched は記録簿がこの時刻まで揃っているか（news.Store.Fresh。必要な日ごとに見た最も古いもの）。
	// 取れていない日があればゼロ値。
	lastFetched time.Time
	// freshDay は lastFetched を決めた日（YYYY-MM-DD）。取れていない日があればその日。
	freshDay string
}

// staleness は記録簿を使えない理由。使えるなら空文字。
func (ev corpEvents) staleness(now time.Time, maxMinutes int) string {
	age := now.Sub(ev.lastFetched)
	switch {
	case ev.lastFetched.IsZero() && ev.freshDay != "":
		return fmt.Sprintf("%s の取り込みに成功していない", ev.freshDay)
	case ev.lastFetched.IsZero():
		return "取り込みに成功した日が 1 日も無い"
	case age > time.Duration(maxMinutes)*time.Minute:
		return fmt.Sprintf("%s ぶんの最後の取り込みが %s（%d 分前。上限 %d 分）", ev.freshDay,
			clock.ToZone(ev.lastFetched, clock.Tokyo).Format("01-02 15:04"), int(age.Minutes()), maxMinutes)
	}
	return ""
}

// loadCorpEvents は day の判定に使う材料を記録簿から読む。knownAt より後に入った記事は使わない。
// 記録簿が無いときはエラー（OpenStore は無ければ作るので、先に確かめる）。
// closed は休場日の判定（取引カレンダーの Calendar.Closed）。鮮度の判定で休場日の取れていない日を問わない。
func loadCorpEvents(ctx context.Context, m dtconfig.Margin, day, knownAt time.Time, closed news.ClosedFunc) (corpEvents, error) {
	path := appSettings.NewsDBPath()
	if _, err := os.Stat(path); err != nil {
		return corpEvents{}, fmt.Errorf("ニュースの記録簿がありません（%s）: %w", path, err)
	}
	store, err := news.OpenStore(path)
	if err != nil {
		return corpEvents{}, err
	}
	defer func() { _ = store.Close() }()
	from := day.AddDate(0, 0, -m.CorpEventLookbackDays).Format(DateLayout)
	events, err := store.CorporateEvents(ctx, from, day.Format(DateLayout), knownAt)
	if err != nil {
		return corpEvents{}, err
	}
	out := corpEvents{marks: map[string]dtplan.CorpEvent{}, notes: map[string]dtplan.CorpEvent{}}
	for code, es := range events {
		for _, e := range es { // 古い順なので、最後に書いたものが残る
			mark := dtplan.CorpEvent{Kind: e.Kind, Headline: e.Headline, At: e.FeedDate + " " + e.Time}
			if news.Excludes(e.Kind) {
				out.marks[code] = mark
			} else {
				out.notes[code] = mark
			}
		}
	}
	// 鮮度は必要な日（今日から news.FreshDays 日）ごとに見る。前日だけ取り込みに失敗した朝を通さない。
	// 休場日（取引カレンダー。読めなければ土日）の取れていない日は問わない
	if out.lastFetched, out.freshDay, err = store.Fresh(ctx, knownAt, news.FreshDays, closed); err != nil {
		return corpEvents{}, err
	}
	return out, nil
}

// markPlanCorpEvents は plan の候補に材料の印を付け、ショートの対象から落とした銘柄をログに残す。
// 記録簿を読めなければ印を付けずにエラーを返す（見送るかは呼び出し側が決める）。
func markPlanCorpEvents(cfg dtconfig.Config, p *dtplan.Plan, day, now time.Time, closed news.ClosedFunc) (corpEvents, []string, error) {
	ev, err := loadCorpEvents(context.Background(), cfg.Margin, day, now, closed)
	if err != nil {
		return corpEvents{}, nil, err
	}
	dropped := p.MarkCorpEvents(ev.marks, cfg.Margin)
	symbols := make([]string, 0, len(dropped))
	for _, c := range dropped {
		symbols = append(symbols, c.Symbol)
		fmt.Printf("材料でショートから外す: %s %s（%s）%s %s\n", c.Symbol, c.Name, c.CorpEvent, c.CorpEventAt, c.CorpEventHeadline)
		logInfo("daytrade.corp_event", "材料でショートの対象から外す", map[string]any{
			"symbol": c.Symbol, "name": c.Name, "kind": c.CorpEvent, "at": c.CorpEventAt, "headline": c.CorpEventHeadline,
		})
	}
	// 記録だけの種類は、ショートの対象に残っている銘柄の分だけ残す（TOB の前触れの一部報道など）
	for _, c := range p.ShortEligible() {
		if n, ok := ev.notes[c.Symbol]; ok {
			logInfo("daytrade.corp_note", "材料の記録（外さない）", map[string]any{
				"symbol": c.Symbol, "name": c.Name, "kind": n.Kind, "at": n.At, "headline": n.Headline,
			})
		}
	}
	return ev, symbols, nil
}
