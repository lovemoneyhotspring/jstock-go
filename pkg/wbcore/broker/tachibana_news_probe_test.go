package broker

// ニュース電文（CLMMfdsGetNews）に何が流れているかを実地で見るための調べもの。
// 本番の参照系を送るので、明示的に環境変数を立てたときだけ動かす。
//
//	TACHIBANA_NEWS_PROBE=20260910,20260909 go test ./pkg/wbcore/broker -run TestNewsProbe -v
//	TACHIBANA_NEWS_GENRE=60221 を足すと、そのジャンルの本文も出す。
import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

// ratingWords はレーティングに関わる見出しを拾うための語。
var ratingWords = []string{"レーティング", "投資判断", "目標株価", "格上げ", "格下げ"}

func TestNewsProbe(t *testing.T) {
	days := strings.Split(os.Getenv("TACHIBANA_NEWS_PROBE"), ",")
	if days[0] == "" {
		t.Skip("TACHIBANA_NEWS_PROBE=YYYYMMDD[,YYYYMMDD...] を立てたときだけ動かす")
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
	wantGenre := os.Getenv("TACHIBANA_NEWS_GENRE")
	words := ratingWords
	if w := os.Getenv("TACHIBANA_NEWS_WORDS"); w != "" {
		words = strings.Split(w, ",")
	}

	for _, day := range days {
		// ニュースは master 口（sUrlMaster）。price 口は「引数エラー」を返す
		res, err := b.postTo(interfaceMaster, "CLMMfdsGetNews", map[string]any{"p_DT": strings.TrimSpace(day)})
		if err != nil {
			t.Errorf("%s: %v", day, err)
			continue
		}
		list, _ := res["aCLMMfdsNews"].([]any)
		genres := map[string]int{}
		var hits []string
		for _, raw := range list {
			row, _ := raw.(map[string]any)
			head := DecodeNewsText(text(row["p_HDL"]))
			for _, g := range strings.Split(text(row["p_GNL"]), "|") {
				genres[g]++
			}
			for _, w := range words {
				if strings.Contains(head, w) {
					hits = append(hits, fmt.Sprintf("%s GN=%-6s 銘柄%2d件 %s",
						text(row["p_TM"]), text(row["p_GNL"]),
						len(strings.Split(text(row["p_ISL"]), "|")), head))
					if os.Getenv("TACHIBANA_NEWS_BODY") != "" {
						body := DecodeNewsText(text(row["p_TX"]))
						if len(body) > 600 {
							body = body[:600] + "…"
						}
						hits = append(hits, "    本文: "+strings.ReplaceAll(body, "\n", " / "))
					}
					break
				}
			}
			if wantGenre != "" && strings.Contains("|"+text(row["p_GNL"])+"|", "|"+wantGenre+"|") {
				t.Logf("=== %s %s\n%s", text(row["p_TM"]), head, DecodeNewsText(text(row["p_TX"])))
			}
		}
		t.Logf("%s: %d 件 / 該当 %d 件（語: %s）", day, len(list), len(hits), strings.Join(words, "|"))
		for _, h := range hits {
			t.Log("  ", h)
		}
		t.Log("  ジャンル(60xxx のみ):", sortedCounts(genres, "60"))
	}
}

func sortedCounts(m map[string]int, prefix string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}
