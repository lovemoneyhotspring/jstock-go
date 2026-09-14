package regime

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/internal/fixture"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

func str(v string) *string { return &v }

// topixArchive は TOPIX の日足（寄り 100、引けは closes）を書いたアーカイブ。
func topixArchive(t *testing.T, days []time.Time, closes []float64) *archive.Archive {
	t.Helper()
	arch := archive.NewArchive(t.TempDir())
	frame := &archive.Frame{Columns: []string{"Date", "O", "H", "L", "C"}}
	for i, d := range days {
		c := fmt.Sprintf("%.2f", closes[i])
		frame.AppendRow(map[string]*string{
			"Date": str(d.Format("2006-01-02")), "O": str("100"), "H": str("110"), "L": str("90"), "C": &c,
		})
	}
	if _, err := arch.Upsert(EPTopix, frame); err != nil {
		t.Fatal(err)
	}
	return arch
}

func near(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-9 }

func TestTopixDrift(t *testing.T) {
	days := fixture.BusinessDays(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 5) // 9/1〜9/7
	// 寄り→引け: +1%, +2%, −3%, +4%, 0%
	arch := topixArchive(t, days, []float64{101, 102, 97, 104, 100})

	got, err := TopixDrift(arch, days[4], 3)
	if err != nil {
		t.Fatal(err)
	}
	if !near(got, (-0.03+0.04+0)/3) {
		t.Errorf("9/7 までの 3 日平均 = %v", got)
	}
	// asOf より後の足は使わない（判断の時点で確定している値だけ）
	if got, _ = TopixDrift(arch, days[3], 3); !near(got, (0.02-0.03+0.04)/3) {
		t.Errorf("9/4 までの 3 日平均 = %v", got)
	}
	// 足が足りなければゲートを効かせない
	if got, err = TopixDrift(arch, days[4], 10); err != nil || got != nil {
		t.Errorf("足が足りないのに値を返した: %v %v", got, err)
	}
	if got, _ = TopixDrift(arch, days[4], 0); got != nil {
		t.Errorf("days = 0 で値を返した: %v", *got)
	}
	// アーカイブに TOPIX が無ければ nil（エラーにしない）
	if got, err = TopixDrift(archive.NewArchive(t.TempDir()), days[4], 3); err != nil || got != nil {
		t.Errorf("TOPIX の無いアーカイブ: %v %v", got, err)
	}
}

func TestTopixDriftSeriesUsesPreviousDays(t *testing.T) {
	days := fixture.BusinessDays(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 5)
	arch := topixArchive(t, days, []float64{101, 102, 97, 104, 100})
	series, err := TopixDriftSeries(arch, days[0], days[4], 2)
	if err != nil {
		t.Fatal(err)
	}
	byDay := map[string]*float64{}
	for _, p := range series {
		byDay[p.Date.Format("2006-01-02")] = p.Drift
	}
	// 9/7 のドリフトは**前日まで**の 2 日（9/3 −3%・9/4 +4%）の平均。当日の 0% を含めない
	if !near(byDay["2026-09-07"], 0.005) {
		t.Errorf("9/7 = %v, want 0.005", byDay["2026-09-07"])
	}
	if !near(byDay["2026-09-04"], -0.005) {
		t.Errorf("9/4 = %v, want -0.005", byDay["2026-09-04"])
	}
}

// optionsArchive は日経 225 オプションの日足（日付 → BaseVol の列）を書いたアーカイブ。
// withBaseVol が偽なら BaseVol 列の無い古い形。
func optionsArchive(t *testing.T, vols map[string][]string, withBaseVol bool) *archive.Archive {
	t.Helper()
	arch := archive.NewArchive(t.TempDir())
	columns := []string{"Date", "Code"}
	if withBaseVol {
		columns = append(columns, "BaseVol")
	}
	frame := &archive.Frame{Columns: columns}
	for day, list := range vols {
		for i, v := range list {
			row := map[string]*string{"Date": str(day), "Code": str(fmt.Sprintf("1%04d", i))}
			if withBaseVol {
				row["BaseVol"] = str(v)
			}
			frame.AppendRow(row)
		}
	}
	if _, err := arch.Upsert(EPOptions225, frame); err != nil {
		t.Fatal(err)
	}
	return arch
}

func TestIVOnAndIVByDay(t *testing.T) {
	arch := optionsArchive(t, map[string][]string{
		"2026-09-03": {"20", "25", "30"},
		"2026-09-04": {"18", "22"},
	}, true)
	d3 := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	d4 := d3.AddDate(0, 0, 1)

	if got, err := IVOn(arch, d3); err != nil || !near(got, 25) {
		t.Errorf("9/3 の中央値 = %v（%v）, want 25", got, err)
	}
	if got, _ := IVOn(arch, d4); !near(got, 20) {
		t.Errorf("9/4 の中央値 = %v, want 20", got)
	}
	// 同じ月でもその日の足が無ければ nil（ゲートを効かせない）
	if got, err := IVOn(arch, d4.AddDate(0, 0, 4)); err != nil || got != nil {
		t.Errorf("足の無い日: %v %v", got, err)
	}
	// アーカイブに無い月も nil
	if got, err := IVOn(arch, d3.AddDate(0, -3, 0)); err != nil || got != nil {
		t.Errorf("アーカイブに無い月: %v %v", got, err)
	}

	points, err := IVByDay(arch, d3, d4)
	if err != nil {
		t.Fatal(err)
	}
	byDay := map[string]*float64{}
	for _, p := range points {
		byDay[p.Date.Format("2006-01-02")] = p.IVPrev
	}
	// IVPrev は**前日**の値。9/4 の判断に使うのは 9/3 の 25
	if !near(byDay["2026-09-04"], 25) || byDay["2026-09-03"] != nil {
		t.Errorf("前日値: %v", points)
	}
}

// BaseVol 列が無い古いアーカイブでもゲートを止めない（診断値が欠けるだけ）。
func TestIVOnWithoutBaseVolColumn(t *testing.T) {
	arch := optionsArchive(t, map[string][]string{"2026-09-03": {"", ""}}, false)
	got, err := IVOn(arch, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || got != nil {
		t.Errorf("BaseVol の無いアーカイブ: %v %v", got, err)
	}
}
