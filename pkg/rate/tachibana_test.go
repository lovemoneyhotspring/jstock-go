package rate

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
)

func TestEventsFromNews(t *testing.T) {
	feed := time.Date(2026, 9, 10, 7, 10, 0, 0, jst)
	items := []broker.NewsItem{
		{
			ID: "a", Time: "0710", Genres: []string{broker.GenreRatingDown},
			Codes:    []string{"1803", "1812"},
			Headline: "<AI市況>【株価レーティング】引き下げ（9/9）：清水建、鹿島",
		},
		{
			ID: "b", Time: "0710", Genres: []string{broker.GenreTargetDown},
			Codes:    []string{"1803", "4263"},
			Headline: "<AI市況>【目標株価】引き下げ（9/9）：サスメドなど",
		},
		// レーティングに関わらないニュースは落ちる
		{ID: "c", Time: "1256", Genres: []string{"3001"}, Codes: []string{"6752"}, Headline: "◇＜東証＞パナＨＤが5.3％高"},
	}

	got := EventsFromNews(feed, items)
	if len(got) != 4 {
		t.Fatalf("件数 = %d, 欲しいのは 4: %+v", len(got), got)
	}
	first := got[0]
	if first.FeedDate != "2026-09-10" || first.Code != "1803" ||
		first.Kind != "rating" || first.Direction != "down" {
		t.Errorf("1 件目が違う: %+v", first)
	}
	// 発表日は見出しから取る（配信日とずれる）
	if first.PubLabel != "9/9" {
		t.Errorf("発表日ラベル = %q, 欲しいのは 9/9", first.PubLabel)
	}
	// 同じ銘柄でも種別が違えば別の行になる（1803 は投資判断と目標株価の両方）
	kinds := map[string]int{}
	for _, e := range got {
		if e.Code == "1803" {
			kinds[e.Kind]++
		}
	}
	if kinds["rating"] != 1 || kinds["target"] != 1 {
		t.Errorf("1803 の内訳 = %v, 欲しいのは rating 1 / target 1", kinds)
	}
}
