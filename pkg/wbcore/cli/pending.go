package cli

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/spf13/cobra"
)

// 台帳の修復（AI の自己修復の道具）
//
// 送信結果不明（PENDING）の注文はプログラムが当日の注文一覧で判定するが、
// 同じ銘柄で細部の違う未帰属の注文があると決められない（ambiguous）。
// そのときログに残る候補（candidates）を AI が読み、ここで台帳を直す。
// 3 つの台帳（wbjp / accum / daytrade）は orders 表の主要な列が同じなので、
// 1 つのコマンドで済ませる。
//
//	<app> pending                     # PENDING の一覧（--json で機械向け）
//	<app> pending resolve <id> --attribute <broker_order_id> --status FILLED --filled 100 --price 1234
//	<app> pending resolve <id> --unsent

// PendingOrder は台帳の PENDING 1 件（3 台帳に共通の列だけ）。
type PendingOrder struct {
	ClientOrderID string  `json:"client_order_id"`
	Symbol        string  `json:"symbol"`
	Quantity      string  `json:"quantity"`
	Status        string  `json:"status"`
	PlacedAt      string  `json:"placed_at"`
	BrokerOrderID *string `json:"broker_order_id"`
}

// NewPendingCmd は台帳の PENDING を見る・直すコマンド。dbPath は台帳の置き場。
func NewPendingCmd(app string, dbPath func() string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "pending",
		Short: "送信結果不明（PENDING）の注文を一覧する・直す（AI の自己修復用）",
		Long: "送信結果が分からず PENDING のまま残った注文は、通常は次の実行が当日の注文一覧で自動判定する。\n" +
			"自動で決められなかったもの（ログの *.pending_ambiguous、candidates 付き）は、\n" +
			"`pending resolve` で台帳を直す。直したあとは通常の実行がそのまま続く。",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := storage.OpenSQLite(dbPath())
			if err != nil {
				return err
			}
			defer db.Close()
			pending, err := ListPending(db)
			if err != nil {
				return err
			}
			if asJSON {
				rows := make([]map[string]any, 0, len(pending))
				for _, p := range pending {
					rows = append(rows, map[string]any{
						"client_order_id": p.ClientOrderID, "symbol": p.Symbol, "quantity": p.Quantity,
						"status": p.Status, "placed_at": p.PlacedAt, "broker_order_id": p.BrokerOrderID,
					})
				}
				return output.EmitJSON(map[string]any{"app": app, "ledger": dbPath(), "pending": rows})
			}
			if len(pending) == 0 {
				fmt.Printf("送信結果不明の注文はありません（%s）\n", dbPath())
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "client_order_id\t銘柄\t株数\t状態\t送信")
			for _, p := range pending {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.ClientOrderID, p.Symbol, p.Quantity, p.Status,
					clock.FmtISO(p.PlacedAt, clock.Tokyo))
			}
			w.Flush()
			fmt.Printf("直すには: %s pending resolve <client_order_id> --attribute <注文番号> --status FILLED | --unsent\n", app)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "機械向けに JSON で出す")
	cmd.AddCommand(newPendingResolveCmd(app, dbPath))
	return cmd
}

func newPendingResolveCmd(app string, dbPath func() string) *cobra.Command {
	var (
		brokerOrderID string
		status        string
		filled        string
		price         string
		unsent        bool
	)
	cmd := &cobra.Command{
		Use:   "resolve <client_order_id>",
		Short: "PENDING の注文を、届いていた（--attribute）か届いていない（--unsent）かで確定する",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			var fix Fix
			switch {
			case unsent && brokerOrderID == "":
				fix = Fix{Status: domain.OrderStatusUnsent}
			case !unsent && brokerOrderID != "":
				st := domain.OrderStatus(strings.ToUpper(strings.TrimSpace(status)))
				if st == "" {
					st = domain.OrderStatusSubmitted
				}
				switch st {
				case domain.OrderStatusSubmitted, domain.OrderStatusPartiallyFilled, domain.OrderStatusFilled,
					domain.OrderStatusCancelled, domain.OrderStatusExpired, domain.OrderStatusRejected:
				default:
					return fmt.Errorf("--status は SUBMITTED / PARTIALLY_FILLED / FILLED / CANCELLED / EXPIRED / REJECTED: %q", status)
				}
				fix = Fix{Status: st, BrokerOrderID: brokerOrderID, Filled: filled, Price: price}
			default:
				return fmt.Errorf("--attribute <注文番号> か --unsent のどちらか一方を指定してください")
			}
			db, err := storage.OpenSQLite(dbPath())
			if err != nil {
				return err
			}
			defer db.Close()
			before, err := ResolvePendingOrder(db, id, fix)
			if err != nil {
				return err
			}
			fmt.Printf("%s: %s → %s", id, before, fix.Status)
			if fix.BrokerOrderID != "" {
				fmt.Printf("（注文番号 %s）", fix.BrokerOrderID)
			}
			fmt.Println()
			return nil
		},
	}
	cmd.Flags().StringVar(&brokerOrderID, "attribute", "", "届いていた: ブローカーの注文番号（立花は 番号/営業日）")
	cmd.Flags().StringVar(&status, "status", "SUBMITTED", "--attribute のときの状態（FILLED など。ブローカーの一覧に合わせる）")
	cmd.Flags().StringVar(&filled, "filled", "", "--attribute のときの約定数量（省略で 0）")
	cmd.Flags().StringVar(&price, "price", "", "--attribute のときの約定単価（省略で未設定）")
	cmd.Flags().BoolVar(&unsent, "unsent", false, "届いていなかった: UNSENT にして送り直せるようにする")
	return cmd
}

// ListPending は台帳の PENDING。
func ListPending(db *sql.DB) ([]PendingOrder, error) {
	rows, err := db.Query(`SELECT client_order_id, symbol, quantity, status, placed_at, broker_order_id
		FROM orders WHERE status = ? ORDER BY placed_at`, string(domain.OrderStatusPending))
	if err != nil {
		return nil, fmt.Errorf("台帳を読めません: %w", err)
	}
	defer rows.Close()
	var out []PendingOrder
	for rows.Next() {
		var p PendingOrder
		if err := rows.Scan(&p.ClientOrderID, &p.Symbol, &p.Quantity, &p.Status, &p.PlacedAt, &p.BrokerOrderID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Fix は PENDING をどう確定するか。
type Fix struct {
	Status        domain.OrderStatus
	BrokerOrderID string
	Filled        string
	Price         string
}

// ResolvePendingOrder は PENDING の 1 件を確定する。PENDING でない行は触らない
// （自動判定や約定が先に入っていたら、その結果を壊さない）。前の状態を返す。
func ResolvePendingOrder(db *sql.DB, clientOrderID string, fix Fix) (string, error) {
	var before string
	if err := db.QueryRow("SELECT status FROM orders WHERE client_order_id = ?", clientOrderID).Scan(&before); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("台帳に %s がありません", clientOrderID)
		}
		return "", err
	}
	if before != string(domain.OrderStatusPending) {
		return before, fmt.Errorf("%s は PENDING ではなく %s です（触りません）", clientOrderID, before)
	}
	filled := "0"
	if strings.TrimSpace(fix.Filled) != "" {
		filled = strings.TrimSpace(fix.Filled)
	}
	var brokerID, price *string
	if fix.BrokerOrderID != "" {
		id := fix.BrokerOrderID
		brokerID = &id
	}
	if strings.TrimSpace(fix.Price) != "" {
		p := strings.TrimSpace(fix.Price)
		price = &p
	}
	_, err := db.Exec(`UPDATE orders SET status = ?, filled_quantity = ?,
		avg_fill_price = COALESCE(?, avg_fill_price), broker_order_id = COALESCE(?, broker_order_id),
		updated_at = ? WHERE client_order_id = ? AND status = ?`,
		string(fix.Status), filled, price, brokerID, clock.NowUTC().Format(time.RFC3339),
		clientOrderID, string(domain.OrderStatusPending))
	if err != nil {
		return before, fmt.Errorf("台帳を更新できません: %w", err)
	}
	return before, nil
}
