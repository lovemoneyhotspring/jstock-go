package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

// Client は取り込みが必要とする API の口。HTTP の実装（wbcore/data）に
// 依存しないことで、試験ではスタブを差せる。
type Client interface {
	// GetAll は pagination_key を辿って data の全行を集める。
	GetAll(path string, params map[string]string) ([]map[string]any, error)
	// BulkList は一括ダウンロードできるファイルの一覧（Key / Size / LastModified）。
	BulkList(endpoint string) ([]map[string]any, error)
	// BulkDownload は署名付き URL 経由で csv.gz の中身をそのまま返す。
	BulkDownload(key string) ([]byte, error)
}

// Ingest は 1 回の取り込みの結果。
type Ingest struct {
	Endpoint string
	Target   string
	Source   string
	Rows     int
	Changed  int
}

// Failure は失敗した取り込み 1 つぶん。
type Failure struct {
	Endpoint string
	Target   string
	Error    string
}

// SyncResult は 1 回の sync / backfill の結果。
type SyncResult struct {
	Ingests  []Ingest
	Failures []Failure
}

// Job は「今やるべき取り込み」1 つぶん（plan の返り値）。
type Job struct {
	Endpoint Endpoint
	Target   string
	Params   map[string]string
}

// Ingestor は API / 一括ファイルから取り、保管庫と台帳に書く。
type Ingestor struct {
	Client  Client
	Archive *Archive
	Ledger  *Ledger
	RunID   string
	Log     *logging.Logger

	warnedNoCalendar bool
}

// NewIngestor は取り込み役を組み立てる。client は nil でもよい
// （status / check のように手元だけ見る用途）。
func NewIngestor(client Client, arch *Archive, ledger *Ledger, runID string, log *logging.Logger) *Ingestor {
	return &Ingestor{Client: client, Archive: arch, Ledger: ledger, RunID: runID, Log: log}
}

func (i *Ingestor) info(code, msg string, extra map[string]any) {
	if i.Log != nil {
		i.Log.Info(code, msg, extra)
	}
}

func (i *Ingestor) warn(code, msg string, extra map[string]any) {
	if i.Log != nil {
		i.Log.Warn(code, msg, extra)
	}
}

func (i *Ingestor) errorLog(code, msg string, extra map[string]any) {
	if i.Log != nil {
		i.Log.Error(code, msg, extra)
	}
}

// -- 1 回ぶん -----------------------------------------------------------

// Ingest は 1 リクエストぶんを取り、保管庫と台帳に書く。
func (i *Ingestor) Ingest(ep Endpoint, target string, params map[string]string) (Ingest, error) {
	if i.Client == nil {
		return Ingest{}, fmt.Errorf("API クライアントがありません（%s）", ep.Path)
	}
	rows, err := i.Client.GetAll(ep.Path, params)
	if err != nil {
		return Ingest{}, err
	}
	frame := &Frame{}
	if len(rows) > 0 {
		frame, err = RowsToFrame(rows, ep)
		if err != nil {
			return Ingest{}, err
		}
	}
	return i.store(ep, target, frame, "api")
}

// IngestDate は 1 日ぶんを取る。
func (i *Ingestor) IngestDate(ep Endpoint, day time.Time) (Ingest, error) {
	iso := day.Format(dateLayout)
	return i.Ingest(ep, iso, map[string]string{ep.DateParam: iso})
}

// IngestRange は from / to の範囲を 1 リクエストで取る。対象は終端日。
func (i *Ingestor) IngestRange(ep Endpoint, start, end time.Time) (Ingest, error) {
	return i.Ingest(ep, end.Format(dateLayout), map[string]string{
		"from": start.Format(dateLayout),
		"to":   end.Format(dateLayout),
	})
}

// IngestAll は引数無しで全件取る。対象は取得日。
func (i *Ingestor) IngestAll(ep Endpoint, today time.Time) (Ingest, error) {
	return i.Ingest(ep, today.Format(dateLayout), map[string]string{})
}

func (i *Ingestor) store(ep Endpoint, target string, frame *Frame, source string) (Ingest, error) {
	changed := 0
	if frame.Height() > 0 {
		n, err := i.Archive.Upsert(ep, frame)
		if err != nil {
			return Ingest{}, err
		}
		changed = n
	}
	if err := i.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: target, Source: source,
		Rows: frame.Height(), Changed: changed, Digest: DigestOf(frame), RunID: i.RunID,
	}); err != nil {
		return Ingest{}, err
	}
	i.info("jquants.ingest", fmt.Sprintf("取り込み %s %s", ep.Path, target), map[string]any{
		"endpoint": ep.Path, "target": target, "source": source,
		"rows": frame.Height(), "changed": changed,
	})
	return Ingest{ep.Path, target, source, frame.Height(), changed}, nil
}

// -- 一括（初回） ---------------------------------------------------------

// Backfill は一括ダウンロードで全期間を取り込む。since は "YYYY-MM"。
//
// ファイル名に年月が入っている（equities_bars_daily_202501.csv.gz）ので、それで絞る。
// 台帳に同じ Key と同じ LastModified があれば飛ばす。1 ファイルの失敗（壊れた
// CSV 等）で残りを止めない。失敗したファイルは台帳に書かれないので、再実行
// すればそこだけ取り直す。
func (i *Ingestor) Backfill(ep Endpoint, since string, keepRaw bool) (*SyncResult, error) {
	if !ep.Bulk {
		return nil, fmt.Errorf("%s は一括ダウンロードに無い。`sync --days N` で遡ってください", ep.Path)
	}
	if i.Client == nil {
		return nil, fmt.Errorf("API クライアントがありません（%s）", ep.Path)
	}
	items, err := i.Client.BulkList(ep.Path)
	if err != nil {
		return nil, err
	}
	result := &SyncResult{}
	for _, item := range items {
		key := asString(item["Key"])
		if key == "" {
			continue
		}
		month := monthIn(key)
		if since != "" && month != "" && month < since {
			continue
		}
		target := "bulk:" + key
		stamp := asString(item["LastModified"])
		previous, err := i.Ledger.Last(ep, target)
		if err != nil {
			return nil, err
		}
		if previous != nil && previous.Digest == stamp {
			continue
		}
		if err := i.backfillOne(ep, key, target, stamp, keepRaw, result); err != nil {
			i.errorLog("jquants.ingest_failed", fmt.Sprintf("一括取り込みに失敗 %s %s", ep.Path, target),
				map[string]any{"endpoint": ep.Path, "target": target, "error": err.Error()})
			result.Failures = append(result.Failures, Failure{ep.Path, target, err.Error()})
		}
	}
	return result, nil
}

func (i *Ingestor) backfillOne(ep Endpoint, key, target, stamp string, keepRaw bool, into *SyncResult) error {
	payload, err := i.Client.BulkDownload(key)
	if err != nil {
		return err
	}
	if keepRaw {
		raw := filepath.Join(i.Archive.RawDir(ep), filepath.Base(key))
		if err := os.MkdirAll(filepath.Dir(raw), 0o755); err != nil {
			return fmt.Errorf("生ファイルの保存先を作れません: %w", err)
		}
		if err := os.WriteFile(raw, payload, 0o644); err != nil {
			return fmt.Errorf("生ファイルを保存できません %s: %w", raw, err)
		}
	}
	frame, err := CSVToFrame(payload, ep)
	if err != nil {
		return err
	}
	changed, err := i.Archive.Upsert(ep, frame)
	if err != nil {
		return err
	}
	// 一括は LastModified を digest に入れ、変わらなければ次回飛ばす
	if err := i.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: target, Source: "bulk",
		Rows: frame.Height(), Changed: changed, Digest: stamp, RunID: i.RunID,
	}); err != nil {
		return err
	}
	i.info("jquants.ingest", fmt.Sprintf("一括取り込み %s %s", ep.Path, target), map[string]any{
		"endpoint": ep.Path, "target": target, "source": "bulk",
		"rows": frame.Height(), "changed": changed,
	})
	into.Ingests = append(into.Ingests, Ingest{ep.Path, target, "bulk", frame.Height(), changed})
	return nil
}

// -- 日次（増分） --------------------------------------------------------

// TradingDays は保存済みの取引カレンダーから営業日を引く。無ければ平日で代用する。
func (i *Ingestor) TradingDays(start, end time.Time) ([]time.Time, error) {
	cal := CalendarEndpoint()
	frame, err := i.Archive.Read(cal, start, end)
	if err != nil {
		return nil, err
	}
	if frame.Height() > 0 && frame.HasColumn("HolDiv") {
		seen := map[string]bool{}
		var days []time.Time
		divIdx, dateIdx := frame.col("HolDiv"), frame.col("Date")
		for _, row := range frame.Rows {
			div, date := cell(row, divIdx), cell(row, dateIdx)
			if div == nil || date == nil || !TradingDayDivisions[*div] || seen[*date] {
				continue
			}
			seen[*date] = true
			if d, err := time.Parse(dateLayout, *date); err == nil {
				days = append(days, d)
			}
		}
		sort.Slice(days, func(a, b int) bool { return days[a].Before(days[b]) })
		return days, nil
	}
	if !i.warnedNoCalendar {
		i.warnedNoCalendar = true
		i.warn("jquants.no_calendar", "取引カレンダーが無いので平日で代用します", nil)
	}
	return weekdays(start, end), nil
}

// Plan は今やるべき取り込みを列挙する（実行はしない）。
//
// 端点ごとに、対象日 D を「D の AvailableAt（JST）を過ぎている」かつ
// 「まだ取っていない、または訂正の猶予（SettleDays）内で前回から
// MinIntervalHours 過ぎた」なら対象にする。
//
// lookbackDays が負なら端点の SettleDays を使う（Python の None 相当）。
func (i *Ingestor) Plan(now time.Time, lookbackDays int) ([]Job, error) {
	now = now.UTC()
	today := truncateDay(now.In(clock.Tokyo))
	var jobs []Job
	for _, ep := range ActiveEndpoints() {
		switch ep.Mode {
		case ModeAll:
			target := today.Format(dateLayout)
			due, err := i.due(ep, today, now, target, false)
			if err != nil {
				return nil, err
			}
			never, err := i.neverFetched(ep)
			if err != nil {
				return nil, err
			}
			if due || never {
				jobs = append(jobs, Job{ep, target, map[string]string{}})
			}
		case ModeRange:
			target := today.Format(dateLayout)
			start := today.AddDate(0, 0, -ep.RangeDays)
			due, err := i.due(ep, today, now, target, false)
			if err != nil {
				return nil, err
			}
			never, err := i.neverFetched(ep)
			if err != nil {
				return nil, err
			}
			if due || never {
				jobs = append(jobs, Job{ep, target, map[string]string{
					"from": start.Format(dateLayout), "to": target,
				}})
			}
		default:
			back := ep.SettleDays
			if lookbackDays >= 0 {
				back = lookbackDays
			}
			first := today.AddDate(0, 0, -back)
			var days []time.Time
			var err error
			if ep.TradingDaysOnly {
				days, err = i.TradingDays(first, today)
				if err != nil {
					return nil, err
				}
			} else {
				days = weekdays(first, today)
			}
			backfilling := lookbackDays >= 0
			covered := map[string]bool{}
			if backfilling {
				covered, err = i.BulkMonths(ep)
				if err != nil {
					return nil, err
				}
			}
			for _, day := range days {
				// 一括で取り込み済みの月を日付で叩き直さない
				if backfilling && covered[day.Format("2006-01")] {
					continue
				}
				target := day.Format(dateLayout)
				due, err := i.due(ep, day, now, target, backfilling)
				if err != nil {
					return nil, err
				}
				if due {
					jobs = append(jobs, Job{ep, target, map[string]string{ep.DateParam: target}})
				}
			}
		}
	}
	return jobs, nil
}

// neverFetched はこの端点をまだ一度も取っていないか。初回だけは公開時刻を待たずに取る
// （公開待ちは「当日ぶんが乗る時刻」の話で、過去ぶんしか無い初回には関係ない）。
func (i *Ingestor) neverFetched(ep Endpoint) (bool, error) {
	targets, err := i.Ledger.Targets(ep)
	if err != nil {
		return false, err
	}
	return len(targets) == 0, nil
}

func (i *Ingestor) due(ep Endpoint, day, now time.Time, target string, backfilling bool) (bool, error) {
	available := ep.AvailableAt.On(day, clock.Tokyo).UTC()
	if now.Before(available) {
		return false, nil
	}
	last, err := i.Ledger.Last(ep, target)
	if err != nil {
		return false, err
	}
	if last == nil {
		return true, nil
	}
	if backfilling {
		return false, nil // 遡りは「一度も取っていない日」だけ
	}
	final := available.AddDate(0, 0, ep.SettleDays)
	if !last.FetchedUTC.Before(final) {
		return false, nil
	}
	return now.Sub(last.FetchedUTC) >= time.Duration(ep.MinIntervalHours)*time.Hour, nil
}

// Sync はやるべき取り込みを順に実行する。冪等。
//
// 1 端点の失敗で全体を止めない。失敗は集めて返し、呼び出し側が非 0 で終了する。
// lookbackDays が負なら端点ごとの SettleDays を使う。
func (i *Ingestor) Sync(now time.Time, lookbackDays int, only []string) (*SyncResult, error) {
	wanted := map[string]bool{}
	for _, name := range only {
		ep, err := LookupEndpoint(name)
		if err != nil {
			return nil, err
		}
		wanted[ep.Path] = true
	}
	result := &SyncResult{}
	// 取引カレンダーは他の端点の「営業日」判定に使うので、期限が来ていれば先に取る
	cal := CalendarEndpoint()
	today := truncateDay(now.In(clock.Tokyo))
	if len(wanted) == 0 || wanted[cal.Path] {
		due, err := i.due(cal, today, now.UTC(), today.Format(dateLayout), false)
		if err != nil {
			return nil, err
		}
		if due {
			i.try(result, cal, today.Format(dateLayout), map[string]string{})
		}
	}
	jobs, err := i.Plan(now, lookbackDays)
	if err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if job.Endpoint.Path == cal.Path {
			continue // 上で取った
		}
		if len(wanted) > 0 && !wanted[job.Endpoint.Path] {
			continue
		}
		i.try(result, job.Endpoint, job.Target, job.Params)
	}
	return result, nil
}

func (i *Ingestor) try(result *SyncResult, ep Endpoint, target string, params map[string]string) {
	ingest, err := i.Ingest(ep, target, params)
	if err != nil {
		i.errorLog("jquants.ingest_failed", fmt.Sprintf("取り込みに失敗 %s %s", ep.Path, target),
			map[string]any{"endpoint": ep.Path, "target": target, "error": err.Error()})
		result.Failures = append(result.Failures, Failure{ep.Path, target, err.Error()})
		return
	}
	result.Ingests = append(result.Ingests, ingest)
}

// -- 確認 ---------------------------------------------------------------

// BulkMonths は一括ダウンロードで取り込み済みの月（YYYY-MM）。
//
// 一括の月ファイルはその月の全営業日を含むので、この月に属する日は日次の
// 取り込み記録が無くても「取得済み」とみなす。遡り（--days）と欠け判定が、
// 一括で埋まった 10 年ぶんを日付で叩き直すのを防ぐ。
//
// 日分割の端点（分足）では常に空を返す。一括ファイルが月次か日次かは
// リファレンスに書かれておらず、日次なら「1 日ぶんしか無いのに月全体を取得済み」
// と誤って判定してしまう。空にすると日付で叩き直す候補が増えるだけで、
// due() が台帳を見るので二重取得にはならない（安全側）。
func (i *Ingestor) BulkMonths(ep Endpoint) (map[string]bool, error) {
	if ep.Split == SplitDay {
		return map[string]bool{}, nil
	}
	targets, err := i.Ledger.Targets(ep)
	if err != nil {
		return nil, err
	}
	months := map[string]bool{}
	for _, target := range targets {
		if len(target) > 5 && target[:5] == "bulk:" {
			if m := monthIn(target); m != "" {
				months[m] = true
			}
		}
	}
	return months, nil
}

// Gaps は期間内の営業日のうち、取れているはずなのに無い日。
//
// 「無い」の判定は 2 段:
//   - データにその日付の行がある（bars のような毎日必ず行があるもの）か、
//   - 台帳にその日の取り込み記録がある（EDINET のように提出が無い日は行が
//     0 件のもの。取ったが空だったのは欠けではない）。
//
// 公開時刻（AvailableAt）がまだ来ていない日は数えない。
func (i *Ingestor) Gaps(ep Endpoint, start, end time.Time, now time.Time) ([]time.Time, error) {
	if ep.Mode != ModeDate {
		return nil, nil
	}
	if now.IsZero() {
		now = clock.NowUTC()
	}
	now = now.UTC()
	dates, err := i.Archive.Dates(ep)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, d := range dates {
		have[d.Format(dateLayout)] = true
	}
	targets, err := i.Ledger.Targets(ep)
	if err != nil {
		return nil, err
	}
	fetched := map[string]bool{}
	for _, t := range targets {
		fetched[t] = true
	}
	covered, err := i.BulkMonths(ep)
	if err != nil {
		return nil, err
	}
	var days []time.Time
	if ep.TradingDaysOnly {
		days, err = i.TradingDays(start, end)
		if err != nil {
			return nil, err
		}
	} else {
		days = weekdays(start, end)
	}
	var missing []time.Time
	for _, d := range days {
		iso := d.Format(dateLayout)
		if have[iso] || fetched[iso] || covered[d.Format("2006-01")] {
			continue
		}
		if now.Before(ep.AvailableAt.On(d, clock.Tokyo).UTC()) {
			continue
		}
		missing = append(missing, d)
	}
	return missing, nil
}

// -- 小物 ---------------------------------------------------------------

func truncateDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// weekdays は期間内の平日（月〜金）。取引カレンダーが無いときの代用。
func weekdays(start, end time.Time) []time.Time {
	var out []time.Time
	for d := truncateDay(start); !d.After(truncateDay(end)); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		out = append(out, d)
	}
	return out
}

var monthPattern = regexp.MustCompile(`(20\d{2})(\d{2})`)

// monthIn は "…_202501.csv.gz" から "2025-01" を取り出す。無ければ空文字。
func monthIn(key string) string {
	m := monthPattern.FindStringSubmatch(filepath.Base(key))
	if m == nil {
		return ""
	}
	return m[1] + "-" + m[2]
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
