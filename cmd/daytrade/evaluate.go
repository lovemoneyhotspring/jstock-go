package main

import (
	"fmt"
	"strings"
	"time"

	dtevaluate "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/evaluate"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/spf13/cobra"
)

func newEvaluateCmd() *cobra.Command {
	var dateFlag string
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "evaluate",
		Short: "大引後: 朝の候補（選んだ銘柄も次点も）に当日の日足を当て、結果を履歴に残す",
		Long: "大引後: 朝の候補（選んだ銘柄も次点も）に当日の日足を当て、結果を履歴に残す。\n\n" +
			"「建てていたらいくらだったか」を候補の全行について計算し、history/evaluation に\n" +
			"追記する。9:00 の順位表が無い日（発注経路を止めている間）は、前夜の plan と当日の\n" +
			"始値から同じ規則で順位を作り直して評価する（ranking_source = archive_open）。\n" +
			"cron では日足の取り込み後（20:20）に回す。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return crash("候補の評価", "daytrade.crash", runEvaluate(dateFlag, jsonFlag))
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}

func runEvaluate(date string, asJSON bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	now := clock.NowUTC()
	day, err := dayOrToday(date, now)
	if err != nil {
		return err
	}
	logConfig(cfg, "evaluate", map[string]any{"day": day.Format(DateLayout)})
	if skipHoliday(day, "evaluate") {
		return nil
	}
	bars, err := dtevaluate.BarsFor(openArchive(), day)
	if err != nil {
		return err
	}
	if len(bars) == 0 {
		fmt.Printf("%s の日足がまだアーカイブに無いので評価しません（jquants sync の後に）\n", day.Format(DateLayout))
		logWarn("daytrade.skip", "日足が無く評価を見送り", map[string]any{
			"reason": "no_bars", "phase": "evaluate", "day": day.Format(DateLayout)})
		return nil
	}

	store := historyStore()
	frame, err := store.Latest(dthistory.KindRanking, day)
	if err != nil {
		return err
	}
	source := dtevaluate.SourceQuotes
	rows, runIDOfRanking := dtevaluate.RowsFromFrame(frame)
	if len(rows) == 0 {
		p, ok, err := planFor(day)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("%s の順位表も plan も無いので評価できません\n", day.Format(DateLayout))
			logWarn("daytrade.skip", "順位表も plan も無く評価を見送り", map[string]any{
				"reason": "no_plan", "phase": "evaluate", "day": day.Format(DateLayout)})
			return nil
		}
		rows = dtevaluate.ReconstructRanking(p, bars, cfg, now)
		runIDOfRanking = ""
		source = dtevaluate.SourceArchiveOpen
		fmt.Println("9:00 の順位表が無いので、前夜の plan と当日の始値から順位を作り直しました")
	}

	led, err := dtledger.Open(appSettings.DaytradeDBPath())
	if err != nil {
		return err
	}
	orders, err := led.OrdersOn(day, nil)
	_ = led.Close()
	if err != nil {
		return err
	}

	result := dtevaluate.Evaluate(rows, runIDOfRanking, bars, cfg, orders, source)
	path := appendHistory(dthistory.KindEvaluation, result, day)
	summary := dtevaluate.Summarize(result)

	if asJSON {
		return output.EmitJSON(map[string]any{
			"ok": true, "day": day.Format(DateLayout), "source": source, "path": path,
			"summary": output.RowsOf(summary), "rows": output.RowsOf(result),
		})
	}
	printEvaluation(day, result, summary, source)
	if path != "" {
		fmt.Printf("履歴に追記 %s\n", path)
	}
	picked, traded := 0, 0
	for _, row := range result.Rows {
		if v, ok := row["picked"].(bool); ok && v {
			picked++
		}
		if v, ok := row["traded"].(bool); ok && v {
			traded++
		}
	}
	logInfo("daytrade.evaluate", "候補を評価", map[string]any{
		"day": day.Format(DateLayout), "source": source, "rows": result.Height(),
		"picked": picked, "traded": traded, "path": path,
	})
	digest.Note(map[string]any{
		"rows": result.Height(), "picked": picked, "traded": traded, "source": source,
	})
	return nil
}

// planFor はその日の plan。plan-<日付> が無ければ履歴（history/plan）から組み立てる。
func planFor(day time.Time) (dtplan.Plan, bool, error) {
	if p, ok, err := dtplan.Load(appSettings.DaytradeDir(), day); err != nil || ok {
		return p, ok, err
	}
	store := historyStore()
	frame, err := store.Latest(dthistory.KindPlan, day)
	if err != nil || frame.Height() == 0 {
		return dtplan.Plan{}, false, err
	}
	meta, err := store.Latest(dthistory.KindPlanMeta, day)
	if err != nil || meta.Height() == 0 {
		return dtplan.Plan{}, false, err
	}
	m := meta.Rows[0]
	candidates := make([]universe.Candidate, 0, frame.Height())
	for _, row := range frame.Rows {
		candidates = append(candidates, universe.Candidate{
			Code: strOf(row["Code"]), Symbol: strOf(row["symbol"]), Name: strOf(row["name"]),
			Segment: strOf(row["segment"]), PrevClose: fOf(row["prev_close"]),
			TurnoverMed: fOf(row["turnover_med"]), MktCap: fOf(row["mkt_cap"]),
			Vol20: fPtrOf(row["vol20"]), CapTercile: int(iOf(row["cap_tercile"])),
			EarnPrev: bOf(row["earn_prev"]), DiscToday: bOf(row["disc_today"]),
			Alert: bOf(row["alert"]), JsfStop: bOf(row["jsf_stop"]),
			Shortable: bOf(row["shortable"]), Eligible: bOf(row["eligible"]),
			ShortEligible: bOf(row["short_eligible"]),
		})
	}
	prevDay := ""
	if t, ok := m["prev_day"].(time.Time); ok {
		prevDay = t.Format(DateLayout)
	}
	return dtplan.Plan{
		Meta: dtplan.Meta{
			Day: day.Format(DateLayout), PrevDay: prevDay,
			Positions:      int(iOf(m["positions"])),
			BudgetPerOrder: fmt.Sprintf("%.0f", fOf(m["budget_per_order"])),
			IVPrev:         fPtrOf(m["iv_prev"]),
			IVGate:         fmt.Sprintf("%g", fOf(m["iv_gate"])),
			Drift:          fPtrOf(m["drift"]),
			Candidates:     int(iOf(m["candidates"])),
			Eligible:       int(iOf(m["eligible"])),
			CreatedAt:      strOf(m["created_at"]),
			ShortEligible:  int(iOf(m["short_eligible"])),
		},
		Candidates: candidates,
	}, true, nil
}

func printEvaluation(day time.Time, result, summary history.Frame, source string) {
	label := "9:00 の気配"
	if source != dtevaluate.SourceQuotes {
		label = "前夜の plan × 当日の始値（作り直し）"
	}
	fmt.Printf("%s の候補の結果  順位表: %s  候補 %d 銘柄\n", day.Format(DateLayout), label, result.Height())
	if summary.Height() == 0 {
		fmt.Println("日足を当てられる候補がありません")
		return
	}
	fmt.Println("\n脚 × 群（picked=選んだ N / next=次点 / rest=それ以外）")
	fmt.Printf("  %-5s %-8s %6s %12s %8s %14s %6s %14s\n",
		"脚", "群", "件数", "平均 net bp", "勝率", "想定損益", "約定", "実現損益")
	for _, row := range summary.Rows {
		fmt.Printf("  %-5s %-8s %6d %12s %8s %14s %6d %14s\n",
			strOf(row["side"]), strOf(row["rank_group"]), iOf(row["count"]),
			bpText(row["avg_net_bp"]), rateText(row["win_rate"]),
			pnlText(row["hypo_pnl"]), iOf(row["traded"]), pnlText(row["actual_pnl"]))
	}

	fmt.Println("\n選んだ銘柄と次点（想定損益は「建てていたら」）")
	fmt.Printf("  %-5s %-5s %-6s %-10s %9s %9s %9s %9s %9s %12s %6s %12s %s\n",
		"脚", "#", "銘柄", "名称", "順位の gap", "始値", "終値", "gross bp", "net bp",
		"想定損益", "建てた", "実現損益", "備考")
	for _, row := range result.Rows {
		if strOf(row["rank_group"]) == "rest" {
			continue
		}
		var notes []string
		if bOf(row["limit_up_close"]) {
			notes = append(notes, "引けS高")
		}
		if bOf(row["limit_down_close"]) {
			notes = append(notes, "引けS安")
		}
		if row["open"] == nil {
			notes = append(notes, "日足なし")
		}
		mark := ""
		if bOf(row["picked"]) {
			mark = "*"
		}
		traded := ""
		if bOf(row["traded"]) {
			traded = "○"
		}
		fmt.Printf("  %-5s %-5s %-6s %-10s %9s %9s %9s %9s %9s %12s %6s %12s %s\n",
			strOf(row["side"]), fmt.Sprintf("%d%s", iOf(row["rank"]), mark),
			strOf(row["symbol"]), truncate(strOf(row["name"]), 10),
			gapText(row["gap"]), yenOrBlank(row["open"]), yenOrBlank(row["close"]),
			bpText(row["gross_bp"]), bpText(row["net_bp"]), pnlText(row["hypo_pnl"]),
			traded, pnlText(row["actual_pnl"]), strings.Join(notes, "、"))
	}
	fmt.Println("# の * は選んだ銘柄。net bp は費用（滑り・貸株料等の見込み）を引いた後")
}
