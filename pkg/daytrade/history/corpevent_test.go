package history

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
)

// 材料（TOB・MBO など）の列はスキーマに無いと書き出しで黙って捨てられる。plan と open_run の両方で残す。
func TestCorpEventColumnsAreKept(t *testing.T) {
	p := plan.Plan{Candidates: []universe.Candidate{{
		Symbol: "8848", CorpEvent: "tob_target", CorpEventHeadline: "当社株券等に対する公開買付け", CorpEventAt: "2026-09-14 2000",
	}}}
	frame, _ := PlanFrames(p)
	want := map[string]string{
		"corp_event": "tob_target", "corp_event_headline": "当社株券等に対する公開買付け", "corp_event_at": "2026-09-14 2000",
	}
	for name, v := range want {
		if got := frame.Rows[0][name]; got != v {
			t.Errorf("plan の %s = %v, want %q", name, got, v)
		}
	}

	run := OpenRunFrame(map[string]any{"corp_excluded": "8848,2612", "news_stale": "最後の取り込みが 173 分前"})
	if got := run.Rows[0]["corp_excluded"]; got != "8848,2612" {
		t.Errorf("open_run の corp_excluded = %v", got)
	}
	if got := run.Rows[0]["news_stale"]; got != "最後の取り込みが 173 分前" {
		t.Errorf("open_run の news_stale = %v", got)
	}
}
