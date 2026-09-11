package rate

import (
	"testing"
	"time"
)

var jst = time.FixedZone("JST", 9*60*60)

const sample = `
<table><tr><td>日付</td><td>コード</td><td>銘柄名</td><td>証券会社</td><td>レーティング</td><td>目標株価</td></tr>
<tr align="center"><td>09/10</td><td>4021</td><td>日産化学</td><td>BofA</td><td>買い継続</td><td>9300円→9500円</td></tr>
<tr align="center"><td>09/10</td><td>3763</td><td>プロシップ</td><td>東海東京</td><td>新規OP</td><td>3000円</td></tr>
<tr align="center"><td>12/28</td><td>6273</td><td>SMC</td><td>SMBC日興</td><td>2→1格上げ</td><td>69,200円→86,100円</td></tr>
<tr align="center"><td>09/09</td><td>1803</td><td>清水建設</td><td>CLSA</td><td>OP→Hold格下げ</td><td>3100円→2600円</td></tr>
<tr><td colspan="6">広告</td></tr></table>`

func TestParse(t *testing.T) {
	now := time.Date(2026, 9, 11, 7, 30, 0, 0, jst)
	got, err := Parse(sample, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("行数 = %d, 欲しいのは 4", len(got))
	}

	if e := got[0]; e.PubDate != "2026-09-10" || e.Code != "4021" || e.Firm != "BofA" ||
		e.Action != "keep" || e.RatingTo != "買い" || e.TargetFrom != 9300 || e.TargetTo != 9500 {
		t.Errorf("継続の行が違う: %+v", e)
	}
	if e := got[1]; e.Action != "new" || e.RatingTo != "OP" || e.TargetFrom != 3000 || e.TargetTo != 3000 {
		t.Errorf("新規の行が違う: %+v", e)
	}
	// 12/28 は取得日より先なので前年になる
	if e := got[2]; e.PubDate != "2025-12-28" || e.Action != "up" || e.RatingFrom != "2" || e.RatingTo != "1" ||
		e.TargetFrom != 69200 || e.TargetTo != 86100 {
		t.Errorf("格上げ・年またぎの行が違う: %+v", e)
	}
	if e := got[3]; e.Action != "down" || e.RatingFrom != "OP" || e.RatingTo != "Hold" {
		t.Errorf("格下げの行が違う: %+v", e)
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse("<html><body>お知らせ</body></html>", time.Now()); err == nil {
		t.Fatal("行が無いときは失敗させたい（ページの作りが変わったのを黙って通さない）")
	}
}
