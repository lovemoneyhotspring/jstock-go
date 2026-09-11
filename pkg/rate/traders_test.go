package rate

import "testing"

const tradersSample = `
<table><tbody><tr><th>日付</th><th>銘柄名</th><th>シンクタンク</th><th>レーティング</th><th>ターゲット</th></tr>
<tr class="rating_row  latest_row">
  <td class="">09/10</td>
  <td class="stock_name text-nowrap"><a href="/stocks/1377/">サカタのタネ</a><br>(1377/東P)</td>
  <td class="text-center">野村</td><td class="">Buy継続</td><td class="">6200→6500円</td>
</tr>
<tr class="rating_row">
  <td class="">1/31</td>
  <td class="stock_name text-nowrap"><a href="/stocks/9741/">日立情報システムズ</a><br>(9741/東1)</td>
  <td class="text-center">ドイツ</td><td class="">Buy→Hold</td><td class="">3600→2600円</td>
</tr>
<tr class="rating_row">
  <td class="">1/4</td>
  <td class="stock_name text-nowrap"><a href="/stocks/6355/">住友精密</a><br>(6355/東1)</td>
  <td class="text-center">UFJ</td><td class="">新規Buy2</td><td class="">-</td>
</tr>
</tbody></table>`

func TestParseTraders(t *testing.T) {
	got, err := ParseTraders(tradersSample, "200501")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("行数 = %d, 欲しいのは 3", len(got))
	}

	// 日付はページの年月に行の月日を合わせる（行が 9/10 でも年月が 200501 なら 2005-09-10）
	if e := got[0]; e.PubDate != "2005-09-10" || e.Code != "1377" || e.Name != "サカタのタネ" ||
		e.Market != "東P" || e.Firm != "野村" || e.RatingFrom != "Buy" || e.RatingTo != "Buy" ||
		e.TargetFrom != 6200 || e.TargetTo != 6500 {
		t.Errorf("継続の行が違う: %+v", e)
	}
	if e := got[1]; e.PubDate != "2005-01-31" || e.Code != "9741" || e.Market != "東1" ||
		e.RatingFrom != "Buy" || e.RatingTo != "Hold" || e.TargetFrom != 3600 || e.TargetTo != 2600 {
		t.Errorf("変更の行が違う: %+v", e)
	}
	// 目標株価が "-" の行は 0 のまま（0 円と取り違えないよう、使う側で Target を見る）
	if e := got[2]; e.RatingFrom != "" || e.RatingTo != "Buy2" || e.TargetFrom != 0 || e.TargetTo != 0 {
		t.Errorf("新規の行が違う: %+v", e)
	}
}

func TestParseTradersEmpty(t *testing.T) {
	if _, err := ParseTraders("<html><body>該当するページがありません</body></html>", "199901"); err == nil {
		t.Fatal("行が無いときは失敗させたい")
	}
}
