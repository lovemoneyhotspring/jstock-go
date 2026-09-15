package plan

import (
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
)

// CorpEvent は銘柄に付ける材料の印（TOB・MBO など。記録簿の判定から cmd が詰める）。
type CorpEvent struct {
	Kind     string
	Headline string
	// At は配信の日時 "YYYY-MM-DD HHMM"。
	At string
}

// MarkCorpEvents は候補に材料の印を付け直し（前の印は消す）、margin.exclude_corp_events なら
// 印の付いた銘柄をショートの対象から落として数え直す。返すのは今回落とした候補。
//
// 保存済みの ShortEligible を**落とす方向にしか動かさない**。ShortFilter.Match を通し直すと、
// plan の後に変えた設定でほかの条件まで動くため。plan の時点で印により落ちた銘柄は、
// 朝に印が消えても戻らない（同じ窓の中の開示なので、朝も印が付く）。
func (p *Plan) MarkCorpEvents(events map[string]CorpEvent, m config.Margin) []universe.Candidate {
	var dropped []universe.Candidate
	shortEligible := 0
	for i := range p.Candidates {
		c := &p.Candidates[i]
		e := events[c.Symbol]
		c.CorpEvent, c.CorpEventHeadline, c.CorpEventAt = e.Kind, e.Headline, e.At
		if m.ExcludeCorpEvents && c.CorpEvent != "" && c.ShortEligible {
			c.ShortEligible = false
			dropped = append(dropped, *c)
		}
		if c.ShortEligible {
			shortEligible++
		}
	}
	p.Meta.ShortEligible = shortEligible
	return dropped
}
