// dt_orders_today は立花の当日の注文一覧（CLMOrderList）をそのまま並べる。読むだけで発注しない。
//
// 台帳の約定は 15:20 の close まで入らないので、場中に「保険が約定したか」を見るのに使う。
//
//	go run ./test/dt_orders_today
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

func main() {
	b, err := cli.ConnectBroker("tachibana", settings.LoadAppSettings())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	now := time.Now()
	orders, err := b.GetOrderHistory(now, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%-20s %-6s %-5s %-14s %8s %8s %10s %-10s %s\n",
		"注文番号", "銘柄", "売買", "区分", "株数", "約定", "約定単価", "状態", "受付")
	for _, o := range orders {
		id, avg, at := "", "", ""
		if o.BrokerOrderID != nil {
			id = *o.BrokerOrderID
		}
		if o.AvgFillPrice != nil {
			avg = o.AvgFillPrice.String()
		}
		if o.CreatedAt != nil {
			at = o.CreatedAt.In(time.FixedZone("JST", 9*3600)).Format("15:04:05")
		}
		fmt.Printf("%-20s %-6s %-5s %-14s %8s %8s %10s %-10s %s\n",
			id, o.Symbol, o.Side, o.Trade, o.Quantity, o.FilledQuantity, avg, o.Status, at)
	}
}
