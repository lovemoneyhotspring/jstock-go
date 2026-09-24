package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func openStrictTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := OpenLedger(filepath.Join(t.TempDir(), "accum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func recordSubmitted(t *testing.T, l *Ledger, id string, amount int64) {
	t.Helper()
	price := decimal.NewFromInt(1000)
	req, err := domain.NewOrderRequest(id, "1306", domain.SideBuy, domain.OrderTypeLimit,
		decimal.NewFromInt(100), &price, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	month, amt, mkt := "2026-09-01", decimal.NewFromInt(amount), domain.MarketJP
	if err := l.Record(req, string(domain.OrderStatusSubmitted), nil, &month, &amt, &mkt); err != nil {
		t.Fatal(err)
	}
}

// 数値の列が読めない行を 0 や nil に倒さない（2026-09-24 のレビュー A3）。
// 以前は amount が読めない行を「発注済み 0 円」と数え、同じ月の予算をもう一度買う計算になった。
func TestUnreadableRowsAreErrors(t *testing.T) {
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, column := range []string{"amount", "quantity", "filled_quantity", "avg_fill_price"} {
		t.Run(column, func(t *testing.T) {
			l := openStrictTestLedger(t)
			recordSubmitted(t, l, "ok", 100_000)
			recordSubmitted(t, l, "broken", 100_000)
			if _, err := l.db.Exec("UPDATE orders SET " + column + " = 'abc' WHERE client_order_id = 'broken'"); err != nil {
				t.Fatal(err)
			}
			if got, err := l.PlacedAmount("1306", month); err == nil {
				t.Errorf("PlacedAmount = %s, want エラー（読めない行を飛ばしている）", got)
			}
			if got, err := l.OpenOrders(); err == nil {
				t.Errorf("OpenOrders = %d 件, want エラー（読めない行が照会から漏れる）", len(got))
			}
		})
	}
}

// 台帳が読めないときは「無い」と読まない（A3・A4）。
func TestClosedLedgerReadsAreErrors(t *testing.T) {
	l := openStrictTestLedger(t)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := l.PlacedAmount("1306", month); err == nil {
		t.Error("PlacedAmount がエラーにならない")
	}
	if _, err := l.HasOrders("1306", month); err == nil {
		t.Error("HasOrders がエラーにならない（繰り越しが黙って消える）")
	}
	if _, err := l.StartedOn("1306"); err == nil {
		t.Error("StartedOn がエラーにならない（日割りが黙って外れる）")
	}
	if _, err := l.PendingSymbols(); err == nil {
		t.Error("PendingSymbols がエラーにならない（送信結果不明の銘柄に出してしまう）")
	}
}

// 開始日は最初の記録だけが残る。無ければ nil（エラーではない）。
func TestStartedOnKeepsFirstDay(t *testing.T) {
	l := openStrictTestLedger(t)
	if got, err := l.StartedOn("452A"); err != nil || got != nil {
		t.Fatalf("記録前 = %v, %v, want nil, nil", got, err)
	}
	if err := l.MarkStarted("452A", "2026-09-16"); err != nil {
		t.Fatal(err)
	}
	if err := l.MarkStarted("452A", "2026-10-01"); err != nil {
		t.Fatal(err)
	}
	if got, err := l.StartedOn("452A"); err != nil || got == nil || *got != "2026-09-16" {
		t.Errorf("開始日 = %v, %v, want 2026-09-16", got, err)
	}
}

// 開始日の記録が無い銘柄の開始日の代わりに、最初の注文の日（東京の暦日）を返す（A4）。
// dry-run は数えない。注文が無ければ nil。
func TestFirstOrderDay(t *testing.T) {
	l := openStrictTestLedger(t)
	tokyo := time.FixedZone("JST", 9*60*60)
	if got, err := l.FirstOrderDay("1306", tokyo); err != nil || got != nil {
		t.Fatalf("注文なし = %v, %v, want nil, nil", got, err)
	}
	recordSubmitted(t, l, "first", 100_000)
	recordSubmitted(t, l, "second", 100_000)
	// 2026-09-10 23:30 UTC は東京では 9/11
	if _, err := l.db.Exec("UPDATE orders SET placed_at = '2026-09-10T23:30:00Z' WHERE client_order_id = 'first'"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec("UPDATE orders SET placed_at = '2026-09-20T01:00:00Z' WHERE client_order_id = 'second'"); err != nil {
		t.Fatal(err)
	}
	if got, err := l.FirstOrderDay("1306", tokyo); err != nil || got == nil || *got != "2026-09-11" {
		t.Errorf("最初の注文日 = %v, %v, want 2026-09-11", got, err)
	}
	// dry-run だけの銘柄は注文なしと同じ
	if _, err := l.db.Exec("UPDATE orders SET status = ?", DryRunStatus); err != nil {
		t.Fatal(err)
	}
	if got, err := l.FirstOrderDay("1306", tokyo); err != nil || got != nil {
		t.Errorf("dry-run だけ = %v, %v, want nil, nil", got, err)
	}
}
