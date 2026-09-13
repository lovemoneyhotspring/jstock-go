package broker

// 一般信用（無期限・短期）の売建可否と在庫が API で取れるかを実地で見るための調べもの。
// 優待クロスを自動化できるかは、これが取れるかで決まる（docs/BROKER_VERIFY.md）。
// 参照系だけを送る。発注はしない。
//
//	TACHIBANA_MARGIN_PROBE=7203,9factory go test ./pkg/wbcore/broker -run TestMarginProbe -v
import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

// marginWords は一般信用・貸借・売建に関わる項目名を拾うための語。
var marginWords = []string{"Sinyou", "Shinyou", "Sinyo", "Taisyaku", "Taishaku",
	"Uri", "Ippan", "Zan", "Kasi", "Kashi", "Hosyo", "Seido"}

func TestMarginProbe(t *testing.T) {
	codes := strings.Split(os.Getenv("TACHIBANA_MARGIN_PROBE"), ",")
	if codes[0] == "" {
		t.Skip("TACHIBANA_MARGIN_PROBE=7203[,9432...] を立てたときだけ動かす")
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

	// 1. 銘柄マスタ: 貸借区分・一般信用の可否がここに載っているか
	for _, code := range codes {
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}
		res, err := b.postTo(interfaceMaster, clmStockMaster, map[string]any{"sIssueCode": code})
		if err != nil {
			t.Errorf("%s %s: %v", clmStockMaster, code, err)
			continue
		}
		rows, _ := res[stockMasterKey].([]any)
		t.Logf("=== %s %s: %d 行", clmStockMaster, code, len(rows))
		if len(rows) == 0 {
			t.Logf("  返った鍵: %s", strings.Join(sortedKeys(res), ", "))
			continue
		}
		row, _ := rows[0].(map[string]any)
		for _, k := range sortedKeys(row) {
			t.Logf("  %-32s = %q", k, text(row[k]))
		}
		t.Logf("  （全 %d 項目）", len(row))
	}

	// 2. 余力: 売建可能額などが取れるか
	res, err := b.postTo(interfaceRequest, clmBalanceSummary, map[string]any{})
	if err != nil {
		t.Errorf("%s: %v", clmBalanceSummary, err)
	} else {
		t.Logf("=== %s の項目（全 %d）", clmBalanceSummary, len(res))
		t.Logf("  %s", strings.Join(sortedKeys(res), " "))
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func matchesAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
