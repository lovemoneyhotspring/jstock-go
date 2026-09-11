package broker

// ログイン応答（CLMAuthLoginAck）の口座区分を実地で見るための調べもの。
// 信用取引口座が開設されたかは、この電文でしか分からない（docs/BROKER_VERIFY.md）。
//
//	TACHIBANA_KOUZA_PROBE=1 go test ./pkg/wbcore/broker -run TestKouzaProbe -v
import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

func TestKouzaProbe(t *testing.T) {
	if os.Getenv("TACHIBANA_KOUZA_PROBE") == "" {
		t.Skip("TACHIBANA_KOUZA_PROBE=1 を立てたときだけ動かす")
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
	payload := map[string]any{
		"p_no":      "1",
		"p_sd_date": sdDate(),
		"sJsonOfmt": "5",
		"sCLMID":    clmLogin,
		"sAuthId":   creds.AuthID,
	}
	body, _ := json.Marshal(payload)
	resp, err := b.httpClient.Post(b.baseURL+"auth/", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(transform.NewReader(resp.Body, japanese.ShiftJIS.NewDecoder()))
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("JSON でない: %s", snippet(raw))
	}
	keys := make([]string, 0, len(res))
	for k := range res {
		// 仮想URL は暗号文で長いので出さない
		if strings.HasPrefix(k, "sUrl") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-34s %s", k, text(res[k]))
	}
}
