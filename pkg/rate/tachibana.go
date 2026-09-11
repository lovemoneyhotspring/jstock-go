package rate

import (
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
)

// genreEvent はニュースのジャンルとレーティングの動きの対応。
var genreEvent = map[string]struct{ Kind, Direction string }{
	broker.GenreRatingUp:   {"rating", "up"},
	broker.GenreRatingDown: {"rating", "down"},
	broker.GenreRatingNew:  {"rating", "new"},
	broker.GenreTargetUp:   {"target", "up"},
	broker.GenreTargetDown: {"target", "down"},
	broker.GenreTargetNew:  {"target", "new"},
}

// EventsFromNews はその日のニュースからレーティングの動きを取り出す。
//
// まとめ記事の関連銘柄コード（p_ISL）がそのまま対象銘柄なので、本文は読まなくてよい。
// 証券会社名まで要るときは本文を読む必要がある（Event には入れていない）。
func EventsFromNews(feedDate time.Time, items []broker.NewsItem) []Event {
	day := feedDate.Format("2006-01-02")
	var events []Event
	seen := map[string]bool{}
	for _, item := range items {
		for _, genre := range item.Genres {
			kind, ok := genreEvent[genre]
			if !ok {
				continue
			}
			for _, code := range item.Codes {
				key := code + "\t" + kind.Kind + "\t" + kind.Direction
				if seen[key] {
					continue
				}
				seen[key] = true
				events = append(events, Event{
					FeedDate:  day,
					PubLabel:  item.PubDateLabel(),
					Code:      code,
					Kind:      kind.Kind,
					Direction: kind.Direction,
					NewsID:    item.ID,
					NewsTime:  item.Time,
				})
			}
		}
	}
	return events
}
