// Package plan は前夜の候補生成（daytrade plan）とその保存。
//
// アーカイブから母集団を切り出して universe.Build に渡し、
// state/daytrade/plan-<日付>.parquet と .json（メタ情報）に残す。
// 9:00 の open はこれを読むだけで、アーカイブを開かない。
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/parquet-go/parquet-go"
)

// DateLayout は plan のファイル名と meta の日付表記。
const DateLayout = "2006-01-02"

// Meta は plan 1 回の要約（.json に書く）。
type Meta struct {
	Day            string   `json:"day"`
	PrevDay        string   `json:"prev_day"`
	Positions      int      `json:"positions"`
	BudgetPerOrder string   `json:"budget_per_order"`
	IVPrev         *float64 `json:"iv_prev"`
	IVGate         string   `json:"iv_gate"`
	// Drift は TOPIX の日中ドリフト（前日まで、regime.drift_days 日平均）。
	Drift      *float64 `json:"drift"`
	Candidates int      `json:"candidates"`
	Eligible   int      `json:"eligible"`
	CreatedAt  string   `json:"created_at"`
	// ShortEligible はショート（[margin]）の対象数。古い plan には無い。
	ShortEligible int `json:"short_eligible"`
}

// Plan は候補と要約。
type Plan struct {
	Meta       Meta
	Candidates []universe.Candidate
}

// Day は判定日。
func (p Plan) Day() time.Time {
	d, _ := time.Parse(DateLayout, p.Meta.Day)
	return d
}

// Eligible はロングの対象（条件に合う行）。
func (p Plan) Eligible() []universe.Candidate {
	return filterCandidates(p.Candidates, func(c universe.Candidate) bool { return c.Eligible })
}

// ShortEligible はショートの対象。
func (p Plan) ShortEligible() []universe.Candidate {
	return filterCandidates(p.Candidates, func(c universe.Candidate) bool { return c.ShortEligible })
}

// Symbols は候補の発注用銘柄コード。
func (p Plan) Symbols(rows []universe.Candidate) []string {
	out := make([]string, 0, len(rows))
	for _, c := range rows {
		out = append(out, c.Symbol)
	}
	return out
}

// PrevCloseBySymbol は銘柄 → 前日終値（ギャップの計算に使う）。
func (p Plan) PrevCloseBySymbol() map[string]float64 {
	out := make(map[string]float64, len(p.Candidates))
	for _, c := range p.Candidates {
		out[c.Symbol] = c.PrevClose
	}
	return out
}

func filterCandidates(rows []universe.Candidate, keep func(universe.Candidate) bool) []universe.Candidate {
	out := make([]universe.Candidate, 0, len(rows))
	for _, c := range rows {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// Paths は plan の parquet と json の置き場。
func Paths(directory string, day time.Time) (parquetPath, metaPath string) {
	stem := filepath.Join(directory, "plan-"+day.Format(DateLayout))
	return stem + ".parquet", stem + ".json"
}

// Build は判定日 day の候補を作る。
func Build(arch *archive.Archive, cfg config.Config, day time.Time, cal *calendar.Calendar, now time.Time) (Plan, error) {
	if cal == nil {
		cal = calendar.FromArchive(arch)
	}
	prevDay, err := cal.PreviousTradingDay(day)
	if err != nil {
		return Plan{}, err
	}
	candidates, err := universe.Build(arch, day, prevDay, cfg.Universe, cfg.Margin)
	if err != nil {
		return Plan{}, err
	}
	// IV とドリフトは「前夜に分かる診断値」。取れなくても plan は作る
	// （ゲートを有効にしている場合だけ、朝の open が取り直す）。
	ivPrev, _ := regime.IVOn(arch, prevDay)
	drift, _ := regime.TopixDrift(arch, prevDay, cfg.Regime.DriftDays)
	if now.IsZero() {
		now = clock.NowUTC()
	}
	eligible, shortEligible := 0, 0
	for _, c := range candidates {
		if c.Eligible {
			eligible++
		}
		if c.ShortEligible {
			shortEligible++
		}
	}
	return Plan{
		Meta: Meta{
			Day:            day.Format(DateLayout),
			PrevDay:        prevDay.Format(DateLayout),
			Positions:      cfg.Capital.Positions(),
			BudgetPerOrder: cfg.Capital.BudgetPerOrder().String(),
			IVPrev:         ivPrev,
			IVGate:         cfg.Regime.IVGate.String(),
			Drift:          drift,
			Candidates:     len(candidates),
			Eligible:       eligible,
			CreatedAt:      now.UTC().Format(time.RFC3339),
			ShortEligible:  shortEligible,
		},
		Candidates: candidates,
	}, nil
}

// record は plan の parquet の 1 行。列名は Python 版と同じにして、
// DuckDB や polars から同じ問い合わせが通るようにする。
type record struct {
	Code          string   `parquet:"Code"`
	Symbol        string   `parquet:"symbol"`
	Name          string   `parquet:"name"`
	Segment       string   `parquet:"segment"`
	PrevClose     float64  `parquet:"prev_close"`
	TurnoverMed   float64  `parquet:"turnover_med"`
	MktCap        float64  `parquet:"mkt_cap"`
	Vol20         *float64 `parquet:"vol20,optional"`
	CapTercile    int32    `parquet:"cap_tercile"`
	EarnPrev      bool     `parquet:"earn_prev"`
	DiscToday     bool     `parquet:"disc_today"`
	Alert         bool     `parquet:"alert"`
	JsfStop       bool     `parquet:"jsf_stop"`
	Shortable     bool     `parquet:"shortable"`
	Eligible      bool     `parquet:"eligible"`
	ShortEligible bool     `parquet:"short_eligible"`
}

// Save は「最新」として plan-<日付> に置く（同じ日なら上書き。open はこれを読む）。
func Save(p Plan, directory string) (parquetPath, metaPath string, err error) {
	parquetPath, metaPath = Paths(directory, p.Day())
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", "", err
	}
	rows := make([]record, 0, len(p.Candidates))
	for _, c := range p.Candidates {
		rows = append(rows, record{
			Code: c.Code, Symbol: c.Symbol, Name: c.Name, Segment: c.Segment,
			PrevClose: c.PrevClose, TurnoverMed: c.TurnoverMed, MktCap: c.MktCap,
			Vol20: c.Vol20, CapTercile: int32(c.CapTercile),
			EarnPrev: c.EarnPrev, DiscToday: c.DiscToday, Alert: c.Alert,
			JsfStop: c.JsfStop, Shortable: c.Shortable,
			Eligible: c.Eligible, ShortEligible: c.ShortEligible,
		})
	}
	// 一時ファイルに書いて rename（途中で落ちても壊れた plan を open に読ませない）
	tmp := parquetPath + ".tmp"
	if err := parquet.WriteFile(tmp, rows); err != nil {
		return "", "", fmt.Errorf("候補の書き出しに失敗しました: %w", err)
	}
	if err := os.Rename(tmp, parquetPath); err != nil {
		return "", "", err
	}
	raw, err := json.MarshalIndent(p.Meta, "", " ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(metaPath, raw, 0o644); err != nil {
		return "", "", err
	}
	return parquetPath, metaPath, nil
}

// Load はその日の plan を読む。無ければ ok=false。
func Load(directory string, day time.Time) (Plan, bool, error) {
	parquetPath, metaPath := Paths(directory, day)
	rawMeta, err := os.ReadFile(metaPath)
	if err != nil {
		return Plan{}, false, nil
	}
	var meta Meta
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		return Plan{}, false, fmt.Errorf("plan の meta を読めません %s: %w", metaPath, err)
	}
	rows, err := parquet.ReadFile[record](parquetPath)
	if err != nil {
		return Plan{}, false, fmt.Errorf("plan の候補を読めません %s: %w", parquetPath, err)
	}
	candidates := make([]universe.Candidate, 0, len(rows))
	for _, r := range rows {
		candidates = append(candidates, universe.Candidate{
			Code: r.Code, Symbol: r.Symbol, Name: r.Name, Segment: r.Segment,
			PrevClose: r.PrevClose, TurnoverMed: r.TurnoverMed, MktCap: r.MktCap,
			Vol20: r.Vol20, CapTercile: int(r.CapTercile),
			EarnPrev: r.EarnPrev, DiscToday: r.DiscToday, Alert: r.Alert,
			JsfStop: r.JsfStop, Shortable: r.Shortable,
			Eligible: r.Eligible, ShortEligible: r.ShortEligible,
		})
	}
	return Plan{Meta: meta, Candidates: candidates}, true, nil
}
