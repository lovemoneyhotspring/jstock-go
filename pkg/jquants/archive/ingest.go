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
	windows, err := ep.Windows()
	if err != nil {
		return Ingest{}, err
	}
	trimWindows(frame, ep, windows)
	changed := 0
	if frame.Height() > 0 {
		n, err := i.upsertWindowed(ep, frame, windows)
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

// upsertWindowed は時間帯で絞った塊を書く。
//
// 窓があって日分割の端点なら、既存ファイルの**窓の外の行は残して**合流する
// （UpsertKeeping）。JQUANTS_TICKS_WINDOWS を後から狭めて backfill / repair すると、
// 新しい塊は窓の中しか持たないので、丸ごと差し替えると窓の外に溜めた行が黙って
// 消える。消すのは明示の `jquants prune` だけにする。窓の中の行は新しい塊で差し替わる。
func (i *Ingestor) upsertWindowed(ep Endpoint, frame *Frame, windows Windows) (int, error) {
	if len(windows) == 0 || ep.Split != SplitDay || ep.TimeColumn == "" {
		return i.Archive.Upsert(ep, frame)
	}
	return i.Archive.UpsertKeeping(ep, frame, func(row RowView) bool {
		return !windows.Keep(row.Text(ep.TimeColumn))
	})
}

// -- 一括（初回） ---------------------------------------------------------

// Backfill は一括ダウンロードで全期間を取り込む。since は "YYYY-MM"。
//
// ファイル名に年月が入っている（equities_bars_daily_202501.csv.gz）ので、それで絞る。
// 台帳に同じ Key と同じ LastModified があれば飛ばす。1 ファイルの失敗（壊れた
// CSV 等）で残りを止めない。失敗したファイルは台帳に書かれないので、再実行
// すればそこだけ取り直す。
func (i *Ingestor) Backfill(ep Endpoint, since string, keepRaw bool) (*SyncResult, error) {
	return i.backfill(ep, since, keepRaw, false)
}

// backfill は Backfill の本体。skipFilled なら、日次ファイルで全営業日が埋まっている月の
// 月次ファイルを取らない（日次の sync・repair の経路）。
//
// 月が締まると、日次で取り込み済みの月にも月次ファイル（historical/）が現れる。Backfill は
// 鍵の違うファイルを「新しい」とみなすので、放っておくと中身の同じ月を丸ごと取り直す。
// ティックなら 4.2GB・9 分で、20:30 の plan と重なる（2026-09-24 のレビュー）。
// 手動の `jquants backfill` は明示の取り直しなので従来どおり取る。
func (i *Ingestor) backfill(ep Endpoint, since string, keepRaw, skipFilled bool) (*SyncResult, error) {
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
	filled := map[string]bool{} // 月 → 日次で埋まっているか（調べた月だけ）
	for _, item := range items {
		key := asString(item["Key"])
		if key == "" {
			continue
		}
		month := monthIn(key)
		if since != "" && month != "" && month < since {
			continue
		}
		if skipFilled && len(coverageIn(key)) == len("2006-01") {
			done, ok := filled[month]
			if !ok {
				if done, err = i.filledByDaily(ep, month); err != nil {
					return nil, err
				}
				filled[month] = done
			}
			if done {
				i.info("jquants.bulk_month_skipped", fmt.Sprintf("日次で埋まっている月の月次一括は取らない %s %s", ep.Path, key),
					map[string]any{"endpoint": ep.Path, "target": "bulk:" + key, "month": month})
				continue
			}
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
		if err := i.backfillOne(ep, key, target, stamp, keepRaw && !ep.NoRaw, result); err != nil {
			i.errorLog("jquants.ingest_failed", fmt.Sprintf("一括取り込みに失敗 %s %s", ep.Path, target),
				map[string]any{"endpoint": ep.Path, "target": target, "error": err.Error()})
			result.Failures = append(result.Failures, Failure{ep.Path, target, err.Error()})
		}
	}
	return result, nil
}

// filledByDaily は月（"2006-01"）の全営業日を、一括の日次ファイルで取り込み済みか。
// 営業日は取引カレンダーから引く（無ければ平日で代用するので、祝日が埋まらず「埋まっていない」
// 側に倒れる＝月次を取る）。月がまだ締まっていなければ先の日が埋まっていないので偽。
func (i *Ingestor) filledByDaily(ep Endpoint, month string) (bool, error) {
	first, err := time.Parse("2006-01", month)
	if err != nil {
		return false, nil
	}
	days, err := i.TradingDays(first, first.AddDate(0, 1, -1))
	if err != nil {
		return false, err
	}
	if len(days) == 0 {
		return false, nil
	}
	covered, err := i.BulkCoverage(ep)
	if err != nil {
		return false, err
	}
	for _, d := range days {
		if !covered[d.Format(dateLayout)] {
			return false, nil
		}
	}
	return true, nil
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
	windows, err := ep.Windows()
	if err != nil {
		return err
	}
	rows, changed := 0, 0
	if ep.Split == SplitDay {
		// 一括の月次ファイルは 1 か月 960 万行あり、丸ごと Frame に載せると 3.8GB になる。
		// 日付順に並んでいるので、日ごとに区切って書けば常駐は 1 日ぶんで済む
		err = CSVToFramesByDay(payload, ep, func(f *Frame) error {
			trimWindows(f, ep, windows)
			n, err := i.upsertWindowed(ep, f, windows)
			if err != nil {
				return err
			}
			rows += f.Height()
			changed += n
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		frame, err := CSVToFrame(payload, ep)
		if err != nil {
			return err
		}
		trimWindows(frame, ep, windows)
		if changed, err = i.upsertWindowed(ep, frame, windows); err != nil {
			return err
		}
		rows = frame.Height()
	}
	// 一括は LastModified を digest に入れ、変わらなければ次回飛ばす
	if err := i.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: target, Source: "bulk",
		Rows: rows, Changed: changed, Digest: stamp, RunID: i.RunID,
	}); err != nil {
		return err
	}
	i.info("jquants.ingest", fmt.Sprintf("一括取り込み %s %s", ep.Path, target), map[string]any{
		"endpoint": ep.Path, "target": target, "source": "bulk",
		"rows": rows, "changed": changed,
	})
	into.Ingests = append(into.Ingests, Ingest{ep.Path, target, "bulk", rows, changed})
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
			if ep.BulkOnly {
				continue // API が無い。Sync が一括の日次ファイルで取る（SyncBulk）
			}
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
				covered, err = i.BulkCoverage(ep)
				if err != nil {
					return nil, err
				}
			}
			for _, day := range days {
				// 一括で取り込み済みの月を日付で叩き直さない
				if backfilling && covers(covered, day) {
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
	interval := time.Duration(ep.MinIntervalHours) * time.Hour
	// 毎営業日行があるはずの端点で前回 0 行だった日は、公開が遅れているだけのことが多い
	// （日経 225 オプションは 16:43・20:00 に 0 行、翌 0:11 に行があった。2026-09-15）。
	// 最短間隔（20 時間）を待つと翌日の昼まで取りに行かず、朝の open の IV ゲートに間に合わない。
	// 0 行の日もある端点（RetryEmpty。日々公表銘柄・決算短信）も、最初の 0 行を 20 時間放置しない
	if last.Rows == 0 && ep.retriesEmpty() {
		interval = EmptyRetryInterval
	}
	return now.Sub(last.FetchedUTC) >= interval, nil
}

// retriesEmpty は 0 行を掴んだ日を EmptyRetryInterval で取り直す端点か。
func (e Endpoint) retriesEmpty() bool {
	return e.TradingDaysOnly && (e.RowsEveryTradingDay || e.RetryEmpty)
}

// emptyIsGap は、台帳の最新が 0 行（fetched に取った）の日を欠けと数えるか。
//   - 毎営業日行があるはずの端点（RowsEveryTradingDay）は常に欠け
//   - 0 行の日もある端点（RetryEmpty）は、訂正の猶予が明けるより前に取った 0 行だけ欠け。
//     明けた後に取り直してまだ 0 行なら、本当に 0 行の日とみなす（毎晩の repair で誤報を繰り返さない）
//   - それ以外（信用残高・EDINET など）は欠けではない
func (e Endpoint) emptyIsGap(day, fetched time.Time) bool {
	if !e.TradingDaysOnly {
		return false
	}
	if e.RowsEveryTradingDay {
		return true
	}
	if !e.RetryEmpty {
		return false
	}
	final := e.AvailableAt.On(day, clock.Tokyo).UTC().AddDate(0, 0, e.SettleDays)
	return fetched.Before(final)
}

// EmptyRetryInterval は、毎営業日行があるはずの端点（と RetryEmpty の端点）で 0 行を掴んだ日を取り直す間隔。
// cron（30 分おき）で 1 時間に 1 回だけ叩き直す。
//
// **1 時間ちょうどにしてはいけない（実効 90 分になる）。** 上の判定は
// 「起動時刻 − 前回の*完了*時刻 ≥ interval」で、完了の時刻は起動の数秒〜十数秒後に打たれる
// （Ledger.Record）。cron は :13 / :43 なので、ちょうど 1 時間後の回は必ず数秒足りずに落ち、
// 次の 30 分後まで待つことになる。台帳の 20 時間の実績 50 例がすべて 20:29:58〜20:30:08 で、
// 20 時間ちょうどが 1 度も無いのが同じ機構の裏付け。0:13 に 0 行を掴んだ日は
// 07:43 の次が 09:13 になり、9:01 の open をまたいでしまう（2026-09-17 のレビュー）
const EmptyRetryInterval = 50 * time.Minute

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
	// API の無い端点（ティック）は一括の日次ファイルで増分を取る
	for _, ep := range ActiveEndpoints() {
		if !ep.BulkOnly || (len(wanted) > 0 && !wanted[ep.Path]) {
			continue
		}
		bulk, err := i.SyncBulk(ep, now, lookbackDays)
		if err != nil {
			i.errorLog("jquants.ingest_failed", fmt.Sprintf("一括の一覧の取得に失敗 %s", ep.Path),
				map[string]any{"endpoint": ep.Path, "target": "bulk:list", "error": err.Error()})
			result.Failures = append(result.Failures, Failure{ep.Path, "bulk:list", err.Error()})
			continue
		}
		result.Ingests = append(result.Ingests, bulk.Ingests...)
		result.Failures = append(result.Failures, bulk.Failures...)
	}
	return result, nil
}

// SyncBulk は API の無い端点（BulkOnly）の増分を一括の日次ファイルで取る。
//
// J-Quants の一括は当月ぶんが日次ファイル（live/）で毎営業日の夕方に増え、
// 月が締まると月次ファイル（historical/）に置き換わる。Backfill は台帳に同じ
// Key と同じ LastModified があれば飛ばすので、「遡る月から先の全ファイル」を
// 対象にしても、実際に取るのは新しく現れた日次ファイルと訂正で LastModified が
// 変わったものだけになる。月が締まって現れる月次ファイルは、日次で埋まっていれば取らない
// （backfill の skipFilled）。lookbackDays が負なら端点の SettleDays を使う。
func (i *Ingestor) SyncBulk(ep Endpoint, now time.Time, lookbackDays int) (*SyncResult, error) {
	back := ep.SettleDays
	if lookbackDays >= 0 {
		back = lookbackDays
	}
	today := truncateDay(now.In(clock.Tokyo))
	since := today.AddDate(0, 0, -back).Format("2006-01")
	return i.backfill(ep, since, true, true)
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

// BulkCoverage は一括ダウンロードで取り込み済みの範囲。
//
// J-Quants の一括は**過去が月次、当月が日次**で配られる（実機で確認、2026-09）:
//
//	equities/bars/minute/historical/2026/equities_bars_minute_202608.csv.gz   月次
//	equities/bars/minute/live/equities_bars_minute_20260904.csv.gz            日次
//
// 月次ファイルはその月の全営業日を含むので "2026-08"（月）を、日次ファイルは
// "2026-09-04"（日）を返す。日次ファイルを月として扱うと「1 日ぶんしか無いのに
// 月全体が取得済み」になり、当月の欠けを検出できなくなる。
//
// これで遡り（--days）と欠け判定は、一括で埋まったぶんを日付で叩き直さずに済む。
func (i *Ingestor) BulkCoverage(ep Endpoint) (map[string]bool, error) {
	targets, err := i.Ledger.Targets(ep)
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	for _, target := range targets {
		if len(target) > 5 && target[:5] == "bulk:" {
			if c := coverageIn(target); c != "" {
				covered[c] = true
			}
		}
	}
	return covered, nil
}

// covers は一括で取り込み済みの範囲に day が入っているか（月ファイルでも日ファイルでも）。
func covers(covered map[string]bool, day time.Time) bool {
	return covered[day.Format("2006-01")] || covered[day.Format(dateLayout)]
}

// Gaps は期間内の営業日のうち、取れているはずなのに無い日。
//
// 「無い」の判定は 2 段:
//   - データにその日付の行がある（bars のような毎日必ず行があるもの）か、
//   - 台帳にその日の取り込み記録がある（EDINET のように提出が無い日は行が
//     0 件のもの。取ったが空だったのは欠けではない）。
//
// 公開時刻（AvailableAt）がまだ来ていない日は数えない。
//
// ただし営業日なら必ず行がある端点（RowsEveryTradingDay）では、台帳に 0 行とだけ
// 残っている日は「取れていない」とみなして欠けに数える。0 行を掴んだ日は訂正の猶予を
// 過ぎると Plan が見直さないので、ここで拾わないと永久に空のまま残る。
// 0 行の日もある端点（RetryEmpty）は、猶予の明ける前に取った 0 行だけを欠けに数える（emptyIsGap）。
//
// 日付モード以外（取引カレンダーなど）は日の欠けの概念が無いので nil。
// そちらの鮮度は Stale で見る。
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
	latest, err := i.Ledger.Latest(ep)
	if err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(latest))
	fetched := map[string]bool{}
	for t, rec := range latest {
		targets = append(targets, t)
		fetched[t] = true
		if rec.Rows > 0 {
			continue
		}
		// 0 行を欠けと数える端点なら、取ったことにしない
		if day, err := time.Parse(dateLayout, t); err == nil && ep.emptyIsGap(day, rec.FetchedUTC) {
			fetched[t] = false
		}
	}
	sort.Strings(targets)
	covered, err := i.BulkCoverage(ep)
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
	// 最初に持っている日より前は数えない。端点によってデータの始まりが違う
	// （EDINET は 2016-09、アドオンは直近 2 年）ので、そこより前を「欠け」と
	// 呼んでも埋めようが無い。何も持っていなければ全期間が対象
	first := firstKnown(dates, targets)
	var missing []time.Time
	for _, d := range days {
		iso := d.Format(dateLayout)
		if first != "" && iso < first {
			continue
		}
		if have[iso] || fetched[iso] || covers(covered, d) {
			continue
		}
		if now.Before(ep.AvailableAt.On(d, clock.Tokyo).UTC()) {
			continue
		}
		missing = append(missing, d)
	}
	return missing, nil
}

// DefaultStaleDays は日付モード以外の端点を「古い」とみなす既定の日数。
// 取引カレンダーのように週 1 回しか取らない端点は、取得間隔の 2 倍の方を使う。
const DefaultStaleDays = 7

// Stale は最終取得が古すぎる端点 1 つぶん。LastFetched がゼロ値なら一度も取っていない。
type Stale struct {
	Endpoint    Endpoint
	LastFetched time.Time
	Limit       time.Duration
}

// StaleLimit は端点を古いとみなす経過時間。staleDays 日と取得間隔の 2 倍の長い方。
// staleDays が 0 以下なら DefaultStaleDays。
func StaleLimit(ep Endpoint, staleDays int) time.Duration {
	if staleDays <= 0 {
		staleDays = DefaultStaleDays
	}
	limit := time.Duration(staleDays) * 24 * time.Hour
	if cadence := 2 * time.Duration(ep.MinIntervalHours) * time.Hour; cadence > limit {
		limit = cadence
	}
	return limit
}

// Stale は日付モード以外（全件・範囲。取引カレンダー・TOPIX・決算予定・投資部門別）で、
// 最後に取れた時刻が StaleLimit より古い端点を返す。
//
// これらは Gaps が日の欠けを数えないので、sync が黙って失敗し続けても check に
// 出てこなかった。台帳は成功した取り込みしか書かないので、最新の記録が「最後に取れた時刻」。
// 一度も取っていない端点も古いとして返す。
func (i *Ingestor) Stale(eps []Endpoint, now time.Time, staleDays int) ([]Stale, error) {
	if now.IsZero() {
		now = clock.NowUTC()
	}
	var out []Stale
	for _, ep := range eps {
		if ep.Mode == ModeDate {
			continue
		}
		history, err := i.Ledger.History(ep, 1)
		if err != nil {
			return nil, err
		}
		limit := StaleLimit(ep, staleDays)
		var last time.Time
		if len(history) > 0 {
			last = history[0].FetchedUTC
		}
		if last.IsZero() || now.Sub(last) > limit {
			out = append(out, Stale{Endpoint: ep, LastFetched: last, Limit: limit})
		}
	}
	return out, nil
}

// firstKnown は端点が最初に持っている日（"2006-01-02"）。データの日付と台帳の
// 対象（日付、または一括ファイルの覆う月・日）のうち最も古いもの。無ければ空文字。
// 月次の一括（"2026-08"）はその月の 1 日として扱う。
func firstKnown(dates []time.Time, targets []string) string {
	first := ""
	take := func(iso string) {
		if iso != "" && (first == "" || iso < first) {
			first = iso
		}
	}
	for _, d := range dates {
		take(d.Format(dateLayout))
	}
	for _, t := range targets {
		switch {
		case len(t) > 5 && t[:5] == "bulk:":
			if c := coverageIn(t); len(c) == 7 {
				take(c + "-01")
			} else {
				take(c)
			}
		case len(t) == len(dateLayout):
			take(t)
		}
	}
	return first
}

// RepairPlan は端点 1 つぶんの「埋めるべき日」。
type RepairPlan struct {
	Endpoint Endpoint
	Days     []time.Time
}

// RepairResult は Repair の結果。Remaining は取り直した後にまだ欠けている日。
type RepairResult struct {
	Plans     []RepairPlan
	Ingests   []Ingest
	Failures  []Failure
	Remaining []RepairPlan
}

// PlanRepair は check と同じ判定（Gaps）で、端点ごとの欠けを集める。
func (i *Ingestor) PlanRepair(eps []Endpoint, start, end, now time.Time) ([]RepairPlan, error) {
	var plans []RepairPlan
	for _, ep := range eps {
		if ep.Mode != ModeDate {
			continue
		}
		gaps, err := i.Gaps(ep, start, end, now)
		if err != nil {
			return nil, fmt.Errorf("%s の欠けを調べられません: %w", ep.Path, err)
		}
		if len(gaps) > 0 {
			plans = append(plans, RepairPlan{Endpoint: ep, Days: gaps})
		}
	}
	return plans, nil
}

// Repair は欠けている日を取り直す。
//
// API のある端点は 1 日ずつ date= で取る（0 行でも台帳に残るので、週次のように
// 行の無い日は次から欠けと数えない）。API の無い端点（ティック）は一括の日次
// ファイルを、欠けの最も古い月から取り直す。終わったら同じ範囲をもう一度調べ、
// 埋まらなかった日を Remaining に返す。
func (i *Ingestor) Repair(eps []Endpoint, start, end, now time.Time) (*RepairResult, error) {
	plans, err := i.PlanRepair(eps, start, end, now)
	if err != nil {
		return nil, err
	}
	result := &RepairResult{Plans: plans}
	sync := &SyncResult{}
	for _, plan := range plans {
		ep := plan.Endpoint
		if ep.BulkOnly {
			since := plan.Days[0].Format("2006-01")
			bulk, err := i.backfill(ep, since, true, true)
			if err != nil {
				i.errorLog("jquants.ingest_failed", fmt.Sprintf("一括の一覧の取得に失敗 %s", ep.Path),
					map[string]any{"endpoint": ep.Path, "target": "bulk:list", "error": err.Error()})
				result.Failures = append(result.Failures, Failure{ep.Path, "bulk:list", err.Error()})
				continue
			}
			sync.Ingests = append(sync.Ingests, bulk.Ingests...)
			sync.Failures = append(sync.Failures, bulk.Failures...)
			continue
		}
		for _, d := range plan.Days {
			iso := d.Format(dateLayout)
			i.try(sync, ep, iso, map[string]string{ep.DateParam: iso})
		}
	}
	result.Ingests, result.Failures = sync.Ingests, sync.Failures
	remaining, err := i.PlanRepair(eps, start, end, now)
	if err != nil {
		return result, err
	}
	result.Remaining = remaining
	return result, nil
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
// 日次ファイル（"…_20250107.csv.gz"）でもその月を返す（Backfill の since 比較用）。
func monthIn(key string) string {
	m := monthPattern.FindStringSubmatch(filepath.Base(key))
	if m == nil {
		return ""
	}
	return m[1] + "-" + m[2]
}

// coveragePattern は一括ファイル名の日付。月次は 6 桁（202608）、日次は 8 桁（20260904）。
var coveragePattern = regexp.MustCompile(`_(20\d{2})(\d{2})(\d{2})?\.`)

// coverageIn は一括ファイル 1 つが覆う範囲。月次なら "2026-08"、日次なら "2026-09-04"。
// 読み取れなければ空文字。
func coverageIn(key string) string {
	m := coveragePattern.FindStringSubmatch(filepath.Base(key))
	if m == nil {
		return ""
	}
	if m[3] == "" {
		return m[1] + "-" + m[2]
	}
	return m[1] + "-" + m[2] + "-" + m[3]
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

// -- 刈り込み -----------------------------------------------------------

// Pruned は 1 ファイルぶんの刈り込みの結果。
type Pruned struct {
	Part    string
	Before  int
	After   int
	Bytes   int64 // 刈った後の大きさ（dryRun なら刈る前）
	Written bool
}

// Prune は日分割の端点の保存済みファイルから、時間帯の外の行を落として書き戻す。
//
// 過去 2 年ぶんを全時間帯で溜めて分析し、「効く時間帯」が決まったあとで容量を
// 減らすためのもの。取り込みの絞り込み（WindowEnv）と同じ Windows を渡し、
// 取り込み側の環境変数も同じ値にしておけば、以後の日次も同じ窓で入る。
//
// 落とした行は戻せない（一括を取り直せば 2 年以内なら復元できる。台帳の
// LastModified が同じだと飛ばされるので、そのときは台帳の bulk: 行を消す）。
// dryRun なら数えるだけで書かない。台帳には Source "prune" で残す。
func (i *Ingestor) Prune(ep Endpoint, windows Windows, dryRun bool) ([]Pruned, error) {
	if ep.Split != SplitDay {
		return nil, fmt.Errorf("%s は日分割ではないので刈り込みの対象外です", ep.Path)
	}
	if ep.TimeColumn == "" {
		return nil, fmt.Errorf("%s には時刻の列が無いので時間帯で絞れません", ep.Path)
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("時間帯が空です（全部残す＝何もしない）。%s か --windows で指定してください", ep.WindowEnv)
	}
	var out []Pruned
	for _, part := range i.Archive.Months(ep) {
		res, err := i.Archive.pruneFile(ep, part, windows, dryRun)
		if err != nil {
			return out, fmt.Errorf("%s の刈り込みに失敗しました: %w", part, err)
		}
		if res.Written {
			if err := i.Ledger.Record(IngestRecord{
				Endpoint: ep.Path, Target: part, Source: "prune",
				Rows: res.After, Changed: res.Before - res.After, Digest: windows.String(), RunID: i.RunID,
			}); err != nil {
				return out, err
			}
			i.info("jquants.prune", fmt.Sprintf("刈り込み %s %s", ep.Path, part), map[string]any{
				"endpoint": ep.Path, "target": part, "windows": windows.String(),
				"before": res.Before, "after": res.After, "bytes": res.Bytes,
			})
		}
		out = append(out, res)
	}
	return out, nil
}
