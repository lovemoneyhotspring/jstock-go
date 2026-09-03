package evaluate

import (
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/shopspring/decimal"
)

// fakeBars はテスト用の足の置き場。
type fakeBars map[string][]domain.Bar

func (f fakeBars) Has(symbol string) bool { _, ok := f[symbol]; return ok }

func (f fakeBars) Read(symbol, start, end string) ([]domain.Bar, error) {
	return f[symbol], nil
}

// nearly は浮動小数の誤差を許して比べる（bp は割り算で出すため端数が乗る）。
func nearly(value any, want float64) bool {
	f, ok := value.(float64)
	return ok && math.Abs(f-want) < 1e-6
}

func bar(symbol, date string, close float64) domain.Bar {
	return domain.Bar{Symbol: symbol, Date: date, Close: decimal.NewFromFloat(close)}
}

func day(text string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", text, time.UTC)
	if err != nil {
		panic(err)
	}
	return d
}

// screenFrame は screen 履歴の体裁（鍵の列つき）で 1 日ぶんを作る。
func screenFrame(d time.Time, rows []map[string]any) corehistory.Frame {
	columns := []corehistory.Column{
		{Name: "day", Type: corehistory.TypeDate},
		{Name: "run_id", Type: corehistory.TypeString},
		{Name: "rank", Type: corehistory.TypeInt64},
		{Name: "symbol", Type: corehistory.TypeString},
		{Name: "score", Type: corehistory.TypeFloat64},
		{Name: "passed", Type: corehistory.TypeBool},
		{Name: "adopted", Type: corehistory.TypeBool},
		{Name: "close", Type: corehistory.TypeFloat64},
	}
	for _, row := range rows {
		row["day"] = d
		row["run_id"] = "run-1"
	}
	return corehistory.NewFrame(columns, rows)
}

func TestForwardCloseCountsBars(t *testing.T) {
	// 暦日ではなく足の本数で数える（休場を挟んでも意味が変わらない）
	bars := []domain.Bar{
		bar("A", "2026-01-05", 100),
		bar("A", "2026-01-09", 110), // 3 日空くが 1 本目
		bar("A", "2026-01-13", 120),
	}
	got, price, ok := ForwardClose(bars, day("2026-01-05"), 2)
	if !ok || !got.Equal(day("2026-01-13")) || price != 120 {
		t.Errorf("2 本先が違う: %v %v %v", got, price, ok)
	}
	if _, _, ok := ForwardClose(bars, day("2026-01-05"), 3); ok {
		t.Error("足りなければ ok=false のはず")
	}
}

func TestEvaluateKeepsUnscoredRows(t *testing.T) {
	// 足の届いていない銘柄も行は残す（落とすと集計が偏る）
	screens := screenFrame(day("2026-01-05"), []map[string]any{
		{"rank": int64(1), "symbol": "A", "score": 0.9, "passed": true, "adopted": true, "close": 100.0},
		{"rank": int64(2), "symbol": "B", "score": 0.6, "passed": true, "adopted": false, "close": 200.0},
		{"rank": int64(3), "symbol": "C", "score": 0.1, "passed": false, "adopted": false, "close": 300.0},
	})
	bars := fakeBars{
		"A": {bar("A", "2026-01-05", 100), bar("A", "2026-01-06", 110)},
		"B": {bar("B", "2026-01-05", 200), bar("B", "2026-01-06", 190)},
		// C は足が無い
	}

	result := Evaluate(screens, bars, day("2026-01-05"), 1)
	if result.Height() != 3 {
		t.Fatalf("行は落とさないはず: %d", result.Height())
	}
	if !nearly(result.Rows[0]["ret_bp"], 1000) {
		t.Errorf("+10%% は 1000 bp のはず: %v", result.Rows[0]["ret_bp"])
	}
	if !nearly(result.Rows[1]["ret_bp"], -500) {
		t.Errorf("-5%% は -500 bp のはず: %v", result.Rows[1]["ret_bp"])
	}
	if result.Rows[2]["ret_bp"] != nil {
		t.Errorf("足が無ければ実績は null: %v", result.Rows[2]["ret_bp"])
	}
	if result.Rows[2]["group"] != "rest" {
		t.Errorf("閾値未満は rest: %v", result.Rows[2]["group"])
	}
	if result.Rows[1]["group"] != "passed" {
		t.Errorf("採用枠から溢れたら passed: %v", result.Rows[1]["group"])
	}
	if result.Rows[0]["screen_run_id"] != "run-1" {
		t.Errorf("判断の実行 ID を写すはず: %v", result.Rows[0]["screen_run_id"])
	}
	if Scored(result).Height() != 2 {
		t.Errorf("実績が出たのは 2 行のはず")
	}
}

func TestSummarizeOrdersGroups(t *testing.T) {
	screens := screenFrame(day("2026-01-05"), []map[string]any{
		{"rank": int64(1), "symbol": "A", "score": 0.9, "passed": true, "adopted": true, "close": 100.0},
		{"rank": int64(2), "symbol": "C", "score": 0.1, "passed": false, "adopted": false, "close": 100.0},
	})
	bars := fakeBars{
		"A": {bar("A", "2026-01-05", 100), bar("A", "2026-01-06", 110)},
		"C": {bar("C", "2026-01-05", 100), bar("C", "2026-01-06", 90)},
	}
	summary := Summarize(Evaluate(screens, bars, day("2026-01-05"), 1))
	if summary.Height() != 2 {
		t.Fatalf("群は 2 つ: %d", summary.Height())
	}
	if summary.Rows[0]["group"] != "adopted" || summary.Rows[1]["group"] != "rest" {
		t.Errorf("Groups の順に並ぶはず: %v", summary.Rows)
	}
	if summary.Rows[0]["win_rate"] != 1.0 || summary.Rows[1]["win_rate"] != 0.0 {
		t.Errorf("勝率が違う: %v", summary.Rows)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	// 履歴が 1 件も無いと列自体が無い空フレームが来る
	if got := Summarize(corehistory.NewFrame(nil, nil)); got.Height() != 0 {
		t.Errorf("空なら 0 行: %d", got.Height())
	}
	if got := Review(corehistory.NewFrame(nil, nil)); got.Height() != 0 {
		t.Errorf("空なら 0 行: %d", got.Height())
	}
	if got := ReviewTotals(corehistory.NewFrame(TotalsColumns, nil)); got.Height() != 0 {
		t.Errorf("空なら 0 行: %d", got.Height())
	}
}

// evaluationFrame は evaluate が積んだ履歴の体裁（鍵の列つき）を作る。
func evaluationFrame(rows []map[string]any) corehistory.Frame {
	columns := append([]corehistory.Column{
		{Name: "day", Type: corehistory.TypeDate},
	}, EvaluationColumns...)
	return corehistory.NewFrame(columns, rows)
}

func TestReviewAndTotals(t *testing.T) {
	frame := evaluationFrame([]map[string]any{
		{"day": day("2026-01-06"), "horizon": int64(5), "group": "adopted", "ret_bp": 100.0},
		{"day": day("2026-01-06"), "horizon": int64(5), "group": "adopted", "ret_bp": 300.0},
		{"day": day("2026-01-06"), "horizon": int64(5), "group": "passed", "ret_bp": 50.0},
		{"day": day("2026-01-06"), "horizon": int64(5), "group": "rest", "ret_bp": -100.0},
		{"day": day("2026-01-05"), "horizon": int64(5), "group": "adopted", "ret_bp": -200.0},
		{"day": day("2026-01-05"), "horizon": int64(5), "group": "rest", "ret_bp": 100.0},
	})

	table := Review(frame)
	if table.Height() != 2 {
		t.Fatalf("2 日ぶん: %d", table.Height())
	}
	// 日付の昇順
	if !table.Rows[0]["day"].(time.Time).Equal(day("2026-01-05")) {
		t.Errorf("日付の昇順のはず: %v", table.Rows[0]["day"])
	}
	second := table.Rows[1]
	if second["adopted"] != int64(2) || second["adopted_bp"] != 200.0 || second["rest_bp"] != -100.0 {
		t.Errorf("日別の集計が違う: %v", second)
	}

	totals := ReviewTotals(table)
	row := totals.Rows[0]
	if row["days"] != int64(2) {
		t.Errorf("日数が違う: %v", row["days"])
	}
	// 採用が圏外を上回ったのは 2 日のうち 1 日
	if row["beat_rest_rate"] != 0.5 {
		t.Errorf("beat_rest_rate が違う: %v", row["beat_rest_rate"])
	}
	if row["avg_adopted_bp"] != 0.0 {
		t.Errorf("採用の平均が違う: %v", row["avg_adopted_bp"])
	}
}
