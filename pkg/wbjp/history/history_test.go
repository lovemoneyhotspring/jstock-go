package history

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func combined(symbol string, direction float64) domain.CombinedSignal {
	return domain.CombinedSignal{Symbol: symbol, Direction: direction, Reason: "合成 " + symbol}
}

func TestScreenFrameMarksPassedAndAdopted(t *testing.T) {
	frame := ScreenFrame(
		[]domain.CombinedSignal{combined("A", 0.2), combined("B", 0.9), combined("C", 0.6)},
		ScreenOptions{Threshold: 0.5, MaxPositions: 1, Combiner: "weighted"},
	)
	if frame.Height() != 3 {
		t.Fatalf("閾値未満も残すはず: %d 行", frame.Height())
	}

	want := []struct {
		symbol  string
		rank    int64
		passed  bool
		adopted bool
	}{
		{"B", 1, true, true},
		{"C", 2, true, false}, // 閾値は超えたが採用枠から溢れた
		{"A", 3, false, false},
	}
	for i, w := range want {
		row := frame.Rows[i]
		if row["symbol"] != w.symbol || row["rank"] != w.rank ||
			row["passed"] != w.passed || row["adopted"] != w.adopted {
			t.Errorf("%d 行目が想定と違う: %+v", i, row)
		}
	}
}

func TestScreenFrameOrdersTiesBySymbol(t *testing.T) {
	// 同点の順位が実行ごとに入れ替わると履歴の差分が読めない
	frame := ScreenFrame(
		[]domain.CombinedSignal{combined("Z", 0.5), combined("A", 0.5)},
		ScreenOptions{Threshold: 0.5, MaxPositions: 2},
	)
	if frame.Rows[0]["symbol"] != "A" || frame.Rows[1]["symbol"] != "Z" {
		t.Errorf("同点は銘柄コード順のはず: %v", frame.Rows)
	}
}

func TestScreenFrameCloseAndMeta(t *testing.T) {
	frame := ScreenFrame(
		[]domain.CombinedSignal{combined("A", 0.7), combined("B", 0.6)},
		ScreenOptions{
			Threshold:    0.5,
			MaxPositions: 2,
			Meta:         map[string]map[string]any{"A": {"close": 1234.5, "rs": 0.8}},
			Reasons:      map[string]string{"A": "押し目"},
			Close: func(symbol string) decimal.Decimal {
				return decimal.NewFromInt(999)
			},
		},
	)
	if frame.Rows[0]["close"] != 1234.5 {
		t.Errorf("meta の close を優先するはず: %v", frame.Rows[0]["close"])
	}
	if frame.Rows[0]["rs"] != 0.8 {
		t.Errorf("meta の数値を写すはず: %v", frame.Rows[0]["rs"])
	}
	if frame.Rows[0]["reason"] != "押し目" {
		t.Errorf("reasons を優先するはず: %v", frame.Rows[0]["reason"])
	}
	// meta が無い銘柄は引き当て関数に落ちる
	if frame.Rows[1]["close"] != float64(999) {
		t.Errorf("meta が無ければ close() を使うはず: %v", frame.Rows[1]["close"])
	}
	if frame.Rows[1]["rs"] != nil {
		t.Errorf("meta が無い数値は null のはず: %v", frame.Rows[1]["rs"])
	}
}

func TestScreenColumnsCoverMetaKeys(t *testing.T) {
	frame := ScreenFrame(nil, ScreenOptions{})
	for _, key := range MetaKeys {
		if !frame.Has(key) {
			t.Errorf("列が足りない: %s", key)
		}
	}
}
