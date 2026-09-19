package main

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/usmarket"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

// recentPnL は資産曲線ゲートの入力——**ロング側**の直近 N 営業日の実現損益。
//
// ショートは含めない（バックテストと同じ定義）。本発注の履歴が 1 日も無ければ nil
// （始めたばかりの口座を「負けている」と誤読して縮めないため。実機検証の注文は数えない）。
// 確定していない日は除いて合計する（dtledger.Ledger.RecentPnL）。
func recentPnL(cfg dtconfig.Config, day time.Time, led *dtledger.Ledger) (*float64, error) {
	cal := calendar.FromArchive(openArchive())
	days, err := cal.PreviousTradingDays(day, cfg.Regime.EquityCurveDays)
	if err != nil {
		return nil, err
	}
	total, incomplete, err := led.RecentPnL(days, "long")
	if err != nil {
		return nil, err
	}
	if len(incomplete) > 0 {
		logWarn("daytrade.pnl_incomplete",
			"実現損益が確定していない日があり、その日を除いて資産曲線を評価",
			map[string]any{"days": incomplete})
	}
	return total, nil
}

// usmarketLatest は前夜の米国セッション（S&P500・VIX）。キャッシュ（data/daytrade/us.json）を
// 先に見て、無い日だけ Cboe → Yahoo → FRED の順に取りに行く。source は cache / fetched / cache_fallback。
// FRED は前夜の値が 9:10 JST ごろまで出ないので、寄付では Cboe と Yahoo が両方落ちた朝の最後の手。
//
// timeout は 1 リクエストの待ち時間。寄付の判断（open）は短く、朝の温め（warm-us）は長く。
// budget は取得の合計の上限（0 なら無し）。過ぎたら残りの取得元には繋がない（usmarket.Until）。
func usmarketLatest(day time.Time, timeout, budget time.Duration) (*usmarket.Session, string, error) {
	sources := []usmarket.Fetcher{usmarket.NewCboeFetcher(timeout), usmarket.NewYahooFetcher(timeout),
		usmarket.NewFredFetcherWithTimeout(timeout)}
	if budget > 0 {
		sources = usmarket.Until(clock.NowUTC().Add(budget), sources...)
	}
	return usmarket.LatestBeforeCached(usmarket.FirstOf(sources...), usmarket.DefaultCachePath(appSettings.DataDir), day)
}

// usmarketNeeded は米国の信号を使う設定か（休むゲートかショック日の判定のどちらか）。
func usmarketNeeded(cfg dtconfig.Config) bool {
	return cfg.Regime.UsSkipHigh != nil || cfg.Regime.ShockUsRet != nil
}

// warmUsmarket は米国のキャッシュを温める（open が寄付の判断の途中で外へ取りに行かないため）。
// 失敗しても呼び出し元は成功——open に取りに行く手が残っている。
//
// 返り値は「day の判定に使える行（前夜のセッション）が VIX つきでキャッシュに入ったか」。
// **20:30 の plan から呼んでも真にはならない**——その夜の米国市場はまだ開いていない。朝の判定に
// 要る行を入れるのは `daytrade warm-us`（2026-09-19。それまでは 4 朝のうち 3 朝、初回の open が
// 9:00 台に取りに行っていた）。
func warmUsmarket(cfg dtconfig.Config, day time.Time) bool {
	if !usmarketNeeded(cfg) {
		return true
	}
	session, source, err := usmarketLatest(day, 15*time.Second, 0)
	fields := map[string]any{"day": day.Format(DateLayout), "source": source}
	if err != nil {
		fields["error"] = err.Error()
		logWarn("daytrade.us_warm", "米国市場のキャッシュを温められない（朝に取りに行く）", fields)
		return false
	}
	ready := false
	if session != nil {
		fields["session"] = session.Describe()
		ready = usmarket.IsFresh(session, day) && session.Vix > 0
	}
	fields["ready"] = ready
	logInfo("daytrade.us_warm", "米国市場のキャッシュを温めた", fields)
	return ready
}

// newWarmUsCmd は朝に米国市場のキャッシュを温める。ブローカーには繋がないので
// /tmp/daytrade.lock は取らない（cron は別のロックで回す）。
//
// 前夜のセッションが VIX つきで入っていれば何も取りに行かない（冪等）。何度か回しておけば、
// 取得元の反映が遅い朝でも後の回が埋める。
func newWarmUsCmd() *cobra.Command {
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "warm-us",
		Short: "前夜の米国市場（S&P500・VIX）をキャッシュに焼く（朝に cron で回す）。open はこれを読む",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if !cfg.Capital.Enabled {
				fmt.Println("jp_gap_fade は無効（capital.enabled = false）。何もしません")
				logInfo("daytrade.skip", "戦略が無効", map[string]any{"reason": "disabled"})
				return nil
			}
			day, err := dayOrToday(dateFlag, clock.NowUTC())
			if err != nil {
				return err
			}
			if skipHoliday(day, "warm-us") {
				return nil
			}
			if !warmUsmarket(cfg, day) {
				want := usmarket.ExpectedSession(day).Format(DateLayout)
				fmt.Printf("前夜（%s）の米国市場がまだ揃っていません（次の回か open が取りに行きます）\n", want)
				logWarn("daytrade.us_warm", "前夜の米国市場がまだ揃っていない", map[string]any{
					"day": day.Format(DateLayout), "want": want})
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}
