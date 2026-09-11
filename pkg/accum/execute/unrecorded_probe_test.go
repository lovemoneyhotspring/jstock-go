package execute

// 台帳に無い当月の約定（手で発注したぶん）を本当に拾えるかを実地で見る調べもの。
// 本番の参照系だけを送る。発注はしない。
//
//	WBJP_ENV=prod WBJP_ENV_FILE=$PWD/.env \
//	  TACHIBANA_PROD_PRIVATE_KEY_FILE=$PWD/e_api_private_key.der \
//	  ACCUM_UNRECORDED_PROBE=563A,1629,2559 \
//	  go test ./pkg/accum/execute -run TestUnrecordedProbe -v
import (
	"os"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

func TestUnrecordedProbe(t *testing.T) {
	raw := os.Getenv("ACCUM_UNRECORDED_PROBE")
	if raw == "" {
		t.Skip("ACCUM_UNRECORDED_PROBE=563A[,...] を立てたときだけ動かす")
	}
	symbols := strings.Split(raw, ",")

	app := settings.LoadAppSettings()
	creds, err := credentials.LoadTachibanaCredentials(app.Env, app.DotenvMap)
	if err != nil {
		t.Fatal(err)
	}
	b, err := broker.NewTachibanaBroker(app.Env, creds, app.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	led, err := ledger.OpenLedger(app.AccumDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()

	now := clock.NowUTC()
	found, err := UnrecordedFills(led, b, symbols, now)
	if err != nil {
		t.Fatalf("突き合わせに失敗: %v", err)
	}
	if len(found) == 0 {
		t.Log("台帳に無い当月の約定は見つからなかった")
	}
	for sym, amount := range found {
		t.Logf("台帳に無い約定: %s %s 円", sym, amount.Round(0))
	}

	// 履歴そのものも出す（何が返っているかを見ないと「0 件」の意味が分からない）
	jst := clock.ToZone(now, clock.Tokyo)
	monthStart := jst.AddDate(0, 0, -(jst.Day() - 1))
	history, err := b.GetOrderHistory(monthStart, jst)
	if err != nil {
		t.Fatalf("注文履歴を照会できません: %v", err)
	}
	t.Logf("当月の注文履歴 %d 件", len(history))
	for _, o := range history {
		avg := "—"
		if o.AvgFillPrice != nil {
			avg = o.AvgFillPrice.String()
		}
		t.Logf("  %s %s %s 数量 %s 約定 %s @ %s  状態 %s  ID %q",
			o.Symbol, o.Side, o.Trade, o.Quantity, o.FilledQuantity, avg, o.Status, o.ClientOrderID)
	}
}
