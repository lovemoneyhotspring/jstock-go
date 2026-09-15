package plan

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
)

// 印はショートだけを落とし、ロングの対象には触らない。落とす方向にしか動かさない
// （保存済みの判定を ShortFilter で作り直さない）。前の印は付け直すたびに消える。
func TestMarkCorpEvents(t *testing.T) {
	p := Plan{Candidates: []universe.Candidate{
		{Symbol: "8848", Eligible: true, ShortEligible: true},
		{Symbol: "7203", ShortEligible: true},
		{Symbol: "9432", CorpEvent: "mbo"},
	}}
	m := config.Margin{ExcludeCorpEvents: true}
	dropped := p.MarkCorpEvents(map[string]CorpEvent{"8848": {Kind: "tob_target", Headline: "当社株券等に対する公開買付け", At: "2026-09-14 2000"}}, m)
	if len(dropped) != 1 || dropped[0].Symbol != "8848" {
		t.Fatalf("dropped = %+v, want 8848 だけ", dropped)
	}
	c := p.Candidates[0]
	if c.ShortEligible || !c.Eligible || c.CorpEvent != "tob_target" || c.CorpEventAt != "2026-09-14 2000" {
		t.Errorf("8848 = %+v", c)
	}
	if !p.Candidates[1].ShortEligible {
		t.Error("印の無い銘柄はショートに残る")
	}
	if p.Candidates[2].CorpEvent != "" {
		t.Error("前の印は消す")
	}
	if p.Meta.ShortEligible != 1 {
		t.Errorf("Meta.ShortEligible = %d, want 1", p.Meta.ShortEligible)
	}
	if got := p.ShortEligible(); len(got) != 1 || got[0].Symbol != "7203" {
		t.Errorf("ShortEligible() = %+v, want 7203 だけ", got)
	}

	// 設定を切れば印は付けるが落とさない
	q := Plan{Candidates: []universe.Candidate{{Symbol: "8848", ShortEligible: true}}}
	if dropped := q.MarkCorpEvents(map[string]CorpEvent{"8848": {Kind: "tob_target"}}, config.Margin{}); len(dropped) != 0 ||
		!q.Candidates[0].ShortEligible || q.Candidates[0].CorpEvent != "tob_target" {
		t.Errorf("exclude_corp_events = false: dropped=%v c=%+v", dropped, q.Candidates[0])
	}
}
