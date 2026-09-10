// パネルのキャッシュ。母集団の条件（設定）に依存しない部分だけを Parquet に落とし、
// 2 回目以降の backtest はそこから読む。
//
// なぜ効くか: パネル SQL 22.3 秒のうち、設定が変えるのは最後の述語（eligible /
// short_eligible）だけで、足の窓関数・銘柄一覧の結合・時価総額の分位・決算の突き合わせは
// 設定に依存しない。格子を回すと同じ 20 秒を何度も払う（2026-09-10 は 11 回払った）。
// 実測: 作成 26.5 秒・83MB、読み出し 4.5 秒（研究ノート 2026-09-daytrade-backtest-perf）。
//
// **1 回だけの実行はむしろ遅くなる**（作成ぶん）。格子を回すときだけの道具。

package backtest

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/archsql"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// panelCacheVersion はキャッシュの中身の版。SQL（列・分位の式・決算の突き合わせ）を
// 変えたら上げる。上げれば鍵が変わり、古いキャッシュは使われない。
const panelCacheVersion = 3

// panelCacheDir はキャッシュの置き場（保管庫の下。再生成できるので消してよい）。
func panelCacheDir(arch *archive.Archive) string {
	return filepath.Join(arch.Root, "_panel_cache")
}

// cacheSources はキャッシュを作るときに読む端点。鍵の材料でもある。
func cacheSources(arch *archive.Archive, end time.Time) (panelSources, bool) {
	// 期間はキャッシュの鍵に入れない（読み出し側で切る）ので、常に全期間を読む。
	var zero time.Time
	bars, ok := archsql.Source(arch, universe.EPBars, zero, end)
	if !ok {
		return panelSources{}, false
	}
	master, ok := archsql.Source(arch, universe.EPMaster, zero, end)
	if !ok {
		return panelSources{}, false
	}
	fins, hasFins := archsql.Source(arch, universe.EPFins, zero, end)
	sched, hasSched := archsql.Source(arch, universe.EPEarningsDate, zero, end)
	alert, hasAlert := archsql.Source(arch, universe.EPMarginAlert, zero, end)
	return panelSources{
		bars: bars, master: master,
		fins: fins, hasFins: hasFins,
		sched: sched, hasSched: hasSched,
		alert: alert, hasAlert: hasAlert,
	}, true
}

// panelCacheKey は「同じ鍵なら中身が同じ」ことを保証する材料のハッシュ。
//
// 材料は (1) 版、(2) 設定のうちキャッシュの中身を変えるもの、(3) 読む Parquet の
// 一覧とその大きさ・更新時刻。日次の sync でファイルが変われば鍵も変わって作り直す。
func panelCacheKey(arch *archive.Archive, cfg config.Config, end time.Time) (string, error) {
	src, ok := cacheSources(arch, end)
	if !ok {
		return "", fmt.Errorf("キャッシュの元になる足・銘柄一覧がありません")
	}
	minTurnover, _ := cfg.Universe.MinTurnover.Float64()
	marginTurnover, _ := cfg.Margin.MinTurnover.Float64()
	h := sha256.New()
	fmt.Fprintf(h, "v%d|turnover_days=%d|vol_days=%d|min_turnover=%f|margin_min_turnover=%f|margin_enabled=%v|product=%s\n",
		panelCacheVersion, cfg.Universe.TurnoverDays, universe.VolDays,
		minTurnover, marginTurnover, cfg.Margin.Enabled, universe.StockProduct)
	for _, path := range sourceFiles(src) {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		fmt.Fprintf(h, "%s|%d|%d\n", path, info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// sourceFiles は read_parquet([...]) の中のパスを取り出して並べる。
func sourceFiles(src panelSources) []string {
	var out []string
	for _, expr := range []string{src.bars, src.master, src.fins, src.sched, src.alert} {
		if expr == "" {
			continue
		}
		for _, part := range strings.Split(expr, "'") {
			if strings.HasSuffix(part, ".parquet") {
				out = append(out, part)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ensurePanelCache はキャッシュが無ければ作る。作った（または既にあった）場所を返す。
//
// 書き込みは一時ファイル → rename。途中で落ちても壊れたファイルが残らない（保管庫と同じ）。
func ensurePanelCache(db *sql.DB, arch *archive.Archive, cfg config.Config, end time.Time) (string, error) {
	key, err := panelCacheKey(arch, cfg, end)
	if err != nil {
		return "", err
	}
	dir := panelCacheDir(arch)
	path := filepath.Join(dir, "panel-"+key+".parquet")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	src, ok := cacheSources(arch, end)
	if !ok {
		return "", fmt.Errorf("キャッシュの元になる足・銘柄一覧がありません")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	query := fmt.Sprintf("COPY (%s) TO %s (FORMAT PARQUET, COMPRESSION ZSTD)",
		buildCacheQuery(src, end, cfg), archsql.LitString(tmp))
	if _, err := db.Exec(query); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("パネルのキャッシュ作成に失敗しました: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	prunePanelCache(dir, path)
	return path, nil
}

// prunePanelCache は今使うもの以外の古いキャッシュを消す（1 本 80MB 台なので溜めない）。
func prunePanelCache(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if path == keep || !strings.HasPrefix(e.Name(), "panel-") {
			continue
		}
		os.Remove(path)
	}
}
