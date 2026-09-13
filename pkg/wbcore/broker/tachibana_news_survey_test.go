package broker

// ニュース電文に何のジャンルが、いつ、どれだけ流れるかの棚卸し。
// 「機械では読めないが AI なら読める情報」がどこにあるかを見つけるための調べもの。
//
//	TACHIBANA_NEWS_SURVEY=20260901,20260902,... go test ./pkg/wbcore/broker -run TestNewsSurvey -v
import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

type genreStat struct {
	n, withCode, inSession, bodyLen int
	samples                         []string
	codes                           int
}

func TestNewsSurvey(t *testing.T) {
	days := strings.Split(os.Getenv("TACHIBANA_NEWS_SURVEY"), ",")
	if days[0] == "" {
		t.Skip("TACHIBANA_NEWS_SURVEY=YYYYMMDD[,...] を立てたときだけ動かす")
	}
	app := settings.LoadAppSettings()
	creds, err := credentials.LoadTachibanaCredentials(app.Env, app.DotenvMap)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTachibanaBroker(app.Env, creds, app.StateDir)
	if err != nil {
		t.Fatal(err)
	}

	stats := map[string]*genreStat{}
	total, totalDays := 0, 0
	hours := map[int]int{}
	for _, day := range days {
		day = strings.TrimSpace(day)
		if day == "" {
			continue
		}
		res, err := b.postTo(interfaceMaster, "CLMMfdsGetNews", map[string]any{"p_DT": day})
		if err != nil {
			t.Errorf("%s: %v", day, err)
			continue
		}
		list, _ := res["aCLMMfdsNews"].([]any)
		if len(list) == 0 {
			continue
		}
		totalDays++
		total += len(list)
		for _, raw := range list {
			row, _ := raw.(map[string]any)
			head := DecodeNewsText(text(row["p_HDL"]))
			tm := text(row["p_TM"])
			hh, _ := strconv.Atoi(firstN(tm, 2))
			hours[hh]++
			codes := 0
			if isl := text(row["p_ISL"]); isl != "" {
				codes = len(strings.Split(isl, "|"))
			}
			inSession := tm >= "0900" && tm <= "1530"
			for _, g := range strings.Split(text(row["p_GNL"]), "|") {
				if g == "" {
					continue
				}
				s := stats[g]
				if s == nil {
					s = &genreStat{}
					stats[g] = s
				}
				s.n++
				s.codes += codes
				if codes > 0 {
					s.withCode++
				}
				if inSession {
					s.inSession++
				}
				s.bodyLen += len([]rune(DecodeNewsText(text(row["p_TX"]))))
				if len(s.samples) < 2 && head != "" {
					s.samples = append(s.samples, tm+" "+trunc(head, 70))
				}
			}
		}
	}

	t.Logf("=== %d 営業日 / 全 %d 件（1 日あたり %.0f 件）===", totalDays, total, float64(total)/float64(max(totalDays, 1)))
	t.Log("--- 時刻の分布（件数）---")
	var hs []int
	for h := range hours {
		hs = append(hs, h)
	}
	sort.Ints(hs)
	var line []string
	for _, h := range hs {
		line = append(line, fmt.Sprintf("%02d時:%d", h, hours[h]))
	}
	t.Log("  " + strings.Join(line, "  "))

	type kv struct {
		g string
		s *genreStat
	}
	var all []kv
	for g, s := range stats {
		all = append(all, kv{g, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s.n > all[j].s.n })
	t.Logf("--- ジャンル別（上位 40）---")
	t.Logf("  %-8s %6s %7s %8s %8s  %s", "ジャンル", "件数", "銘柄付", "場中", "本文字数", "例")
	for i, e := range all {
		if i >= 40 {
			break
		}
		s := e.s
		t.Logf("  %-8s %6d %6.0f%% %7.0f%% %8d  %s", e.g, s.n,
			float64(s.withCode)/float64(s.n)*100, float64(s.inSession)/float64(s.n)*100,
			s.bodyLen/s.n, strings.Join(s.samples, " ｜ "))
	}
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
