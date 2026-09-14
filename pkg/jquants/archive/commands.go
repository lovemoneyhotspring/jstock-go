package archive

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// このファイルは cmd/jquants の各コマンドが使う組み立て部品。
// コマンド側は表示と終了コードだけを持ち、判断はここに置いて試験する。

// LookupEndpoints は --only を端点に解決する。空なら日次で回す全端点（ActiveEndpoints）。
func LookupEndpoints(only []string) ([]Endpoint, error) {
	if len(only) == 0 {
		return ActiveEndpoints(), nil
	}
	out := make([]Endpoint, 0, len(only))
	for _, name := range only {
		ep, err := LookupEndpoint(name)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, nil
}

// PlanLine は `sync --dry-run` の 1 行（端点・対象・引数）。
type PlanLine struct {
	Endpoint string
	Target   string
	Params   string
}

// PlanLines は Plan の結果を --only で絞り、表示用の行にする。
// 引数は名前順に並べる（map の走査順は不定）。API の無い端点（BulkOnly）は
// 一括の一覧を見るまで対象が分からないので、端点ごとに「bulk」の 1 行を足す。
func PlanLines(jobs []Job, only []string) ([]PlanLine, error) {
	wanted := map[string]bool{}
	if len(only) > 0 {
		eps, err := LookupEndpoints(only)
		if err != nil {
			return nil, err
		}
		for _, ep := range eps {
			wanted[ep.Path] = true
		}
	}
	var lines []PlanLine
	for _, job := range jobs {
		if len(wanted) > 0 && !wanted[job.Endpoint.Path] {
			continue
		}
		keys := make([]string, 0, len(job.Params))
		for k := range job.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		params := make([]string, 0, len(keys))
		for _, k := range keys {
			params = append(params, fmt.Sprintf("%s=%s", k, job.Params[k]))
		}
		lines = append(lines, PlanLine{job.Endpoint.Path, job.Target, strings.Join(params, ", ")})
	}
	for _, ep := range ActiveEndpoints() {
		if ep.BulkOnly && (len(wanted) == 0 || wanted[ep.Path]) {
			lines = append(lines, PlanLine{ep.Path, "bulk", "一括の日次ファイルで、台帳に無いか更新されたもの"})
		}
	}
	return lines, nil
}

// CheckRange は check / repair の確認範囲 [start, end]。
//
// end は --date（YYYY-MM-DD、JST の暦日）か、無ければ JST の今日。
// UTC の今日を使うと 00:00〜09:00 JST に 1 日早く終わり、Plan / Sync（JST）と食い違う。
func CheckRange(date string, days int) (start, end time.Time, err error) {
	end = clock.TodayJST()
	if date != "" {
		end, err = clock.ParseDateJST(date)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--date は YYYY-MM-DD で指定してください: %w", err)
		}
	}
	if days < 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("--days は 0 以上で指定してください: %d", days)
	}
	return end.AddDate(0, 0, -days), end, nil
}

// BackfillStep は BackfillAll の端点 1 つぶんの経過。
type BackfillStep struct {
	Endpoint Endpoint
	// Skipped は一括に無い端点で、何もしなかった。
	Skipped bool
	// Result は取り込みの結果（Err があれば nil）。
	Result *SyncResult
	// Err は一覧の取得に失敗した等、端点ごと進められなかったとき。
	Err error
}

// BackfillAll は端点を順に一括取り込みする。一括に無い端点は飛ばし、
// 1 端点の失敗（一覧の取得など）で残りを止めない。each は端点ごとに呼ばれる（nil 可）。
func (i *Ingestor) BackfillAll(eps []Endpoint, since string, keepRaw bool, each func(BackfillStep)) *SyncResult {
	total := &SyncResult{}
	report := func(step BackfillStep) {
		if each != nil {
			each(step)
		}
	}
	for _, ep := range eps {
		if !ep.Bulk {
			report(BackfillStep{Endpoint: ep, Skipped: true})
			continue
		}
		result, err := i.Backfill(ep, since, keepRaw)
		if err != nil {
			i.errorLog("jquants.ingest_failed", fmt.Sprintf("一括の一覧の取得に失敗 %s", ep.Path),
				map[string]any{"endpoint": ep.Path, "target": "bulk:list", "error": err.Error()})
			total.Failures = append(total.Failures, Failure{Endpoint: ep.Path, Target: "bulk:list", Error: err.Error()})
			report(BackfillStep{Endpoint: ep, Err: err})
			continue
		}
		total.Ingests = append(total.Ingests, result.Ingests...)
		total.Failures = append(total.Failures, result.Failures...)
		report(BackfillStep{Endpoint: ep, Result: result})
	}
	return total
}

// ResolveWindows は prune の時間帯。--windows があればそれ、無ければ端点の環境変数。
func ResolveWindows(ep Endpoint, spec string) (Windows, error) {
	if strings.TrimSpace(spec) != "" {
		return ParseWindows(spec)
	}
	return ep.Windows()
}

// JoinDays は日付を先頭 8 つまで並べ、多ければ合計を添える。
func JoinDays(days []time.Time) string {
	const shownMax = 8
	shown := make([]string, 0, min(len(days), shownMax))
	for i, d := range days {
		if i >= shownMax {
			break
		}
		shown = append(shown, d.Format(dateLayout))
	}
	text := strings.Join(shown, ", ")
	if len(days) > shownMax {
		text += fmt.Sprintf(" …（計 %d）", len(days))
	}
	return text
}

// RegisterViews は保管庫の端点ディレクトリを DuckDB のビューとして登録する（query コマンド用）。
//
// union_by_name で列が増えた月とも一緒に読める。ビューを作れなかった端点
// （壊れた Parquet など）があっても残りは登録し、失敗はまとめて返す。
// パスの ' と識別子の " はエスケープする（保管庫の場所に ' が入っていても壊れない）。
func RegisterViews(db *sql.DB, arch *Archive) error {
	var errs []error
	for _, name := range arch.ExistingParquetDirs() {
		glob := filepath.Join(arch.Root, name, "*.parquet")
		stmt := fmt.Sprintf("CREATE OR REPLACE VIEW %s AS SELECT * FROM read_parquet(%s, union_by_name=true)",
			quoteIdent(name), quoteLiteral(glob))
		if _, err := db.Exec(stmt); err != nil {
			errs = append(errs, fmt.Errorf("ビュー %s を作れません: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// quoteIdent は SQL の識別子を " で囲む。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral は SQL の文字列リテラルを ' で囲む。
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
