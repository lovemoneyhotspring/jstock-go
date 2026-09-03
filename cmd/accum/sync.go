package main

import (
	"fmt"
	"sort"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/spf13/cobra"
)

// defaultSyncDays は既定の取得期間（約30年）。
//
// 200日移動平均を使ううえ、増額が効くのは暴落局面なので、売買用の銘柄より
// ずっと長い履歴が要る。上昇局面しか含まない短い期間で検証すると、増額の
// 効果が実際より小さく出る。
const defaultSyncDays = 10_950

// runSyncDays は run が判断の直前に足を更新するときの期間。
//
// 判断に要るのは直近の足だけで、長い履歴は既に保存済み。毎回 30 年ぶん
// 取り直すと発注時間帯（14:00〜15:00）を食い潰す。
const runSyncDays = 30

func newSyncCmd() *cobra.Command {
	var days int
	var force bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "設定銘柄（日本株および判定用指数）の日足を同期する",
		Long: "既に保存済みの銘柄は「最終日より後」しか取りに行かない。あとから期間を\n" +
			"伸ばしたいときは --force を付けて取り直す。",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			if days < 1 {
				return fmt.Errorf("--days は 1 以上で指定してください: %d", days)
			}

			logger, err := newRunLogger("sync")
			if err != nil {
				return err
			}

			total, failures := syncAccumBars(cfg, logger, days, force, true)

			digest.Note(map[string]any{"fetched": total, "failures": failures, "days": days, "force": force})
			if failures > 0 {
				digest.Anomaly("accum.sync_failed", fmt.Sprintf("%d 銘柄の同期に失敗しました", failures))
				return fmt.Errorf("%d 銘柄の同期に失敗しました", failures)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", defaultSyncDays, "何日ぶん遡って取得するか")
	cmd.Flags().BoolVar(&force, "force", false, "保存済みを無視して取り直す")
	return cmd
}

// accumSyncSymbols は同期する銘柄を市場ごとにまとめる。
//
// 取得は市場ごとに分ける。ティッカーの変換も取得元も市場で違う。
// 買う銘柄だけでなく判定用の指数・バスケットの基準銘柄も含める——
// これらが古いままだと倍率が前日以前の値で決まる。
func accumSyncSymbols(cfg *accumcfg.AccumConfig) map[domain.Market][]string {
	grouped := map[domain.Market][]string{}
	add := func(market domain.Market, symbol string) {
		for _, s := range grouped[market] {
			if s == symbol {
				return
			}
		}
		grouped[market] = append(grouped[market], symbol)
	}
	for _, entry := range cfg.Active() {
		for _, symbol := range entry.Symbols {
			add(entry.MarketResolved(), symbol)
		}
		if entry.SignalSymbol != "" {
			add(entry.SignalMarketResolved(), entry.SignalSymbol)
		}
	}
	for i := range cfg.ActiveBaskets() {
		basket := cfg.ActiveBaskets()[i]
		for _, symbol := range basket.Symbols() {
			add(basket.MarketResolved(), symbol)
		}
		if basket.Benchmark != "" {
			add(basket.MarketResolved(), basket.Benchmark)
		}
	}
	return grouped
}

// syncAccumBars は積立対象（判定用指数・バスケットの基準銘柄を含む）の足を更新する。
//
// 返すのは取得できた本数と失敗した銘柄数。失敗しても止めない——1 銘柄が
// 取れないだけで積立を丸ごと諦める理由はなく、古い足のままなら
// PlanOrders が stale として警告する。
func syncAccumBars(
	cfg *accumcfg.AccumConfig,
	logger *logging.Logger,
	days int,
	force bool,
	verbose bool,
) (total int, failures int) {
	today := clock.TodayUTC()
	end := today.Format("2006-01-02")
	start := today.AddDate(0, 0, -days).Format("2006-01-02")
	barStore := data.NewBarStore(appSettings.BarsDir())

	grouped := accumSyncSymbols(cfg)
	markets := make([]domain.Market, 0, len(grouped))
	for market := range grouped {
		markets = append(markets, market)
	}
	sort.Slice(markets, func(i, j int) bool { return markets[i] < markets[j] })

	for _, market := range markets {
		symbols := grouped[market]
		sort.Strings(symbols)
		name := cfg.DataProvider
		if name == "" {
			name = data.DefaultProvider(market)
		}
		provider, err := data.Connect(name, data.ProviderParams{
			Env: appSettings.Env, Market: market, Settings: appSettings,
		})
		if err != nil {
			logger.Error("sync.provider", fmt.Sprintf("%s の取得元 %s に接続できません: %v", market, name, err))
			failures += len(symbols)
			continue
		}

		for _, symbol := range symbols {
			from := start
			if !force {
				// 増分同期。最終日の翌日から取り直す（当日の足は取引所側で
				// 後から訂正されうるので、最終日そのものも取り直す）
				if lastDay, err := barStore.LastDate(symbol); err == nil && lastDay != "" && lastDay > start {
					from = lastDay
				}
			}
			fetched, err := provider.FetchBars([]string{symbol}, from, end)
			if err != nil {
				logger.Error("sync.failed", fmt.Sprintf("%s: %v", symbol, err))
				failures++
				continue
			}
			bars := fetched[symbol]
			if len(bars) == 0 {
				logger.Info("sync.empty", fmt.Sprintf("%s: 新しい足はありません", symbol))
				continue
			}
			count, err := barStore.Upsert(symbol, bars)
			if err != nil {
				logger.Error("sync.write_failed", fmt.Sprintf("%s: %v", symbol, err))
				failures++
				continue
			}
			total += len(bars)
			if verbose {
				fmt.Printf("  %s: %d 本（保存後 %d 本）\n", symbol, len(bars), count)
			}
		}
	}
	return total, failures
}
