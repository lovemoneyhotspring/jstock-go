package news

import (
	"context"
	"slices"
	"testing"
	"time"

	broker "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
)

// 実例は 2026-06-17〜09-15 の記録簿から（計画ノート daytrade-short-corp-events の表）。
// 買付者側を外さないこと・対象を取りこぼさないことの両方を守る。
func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		headline string
		codes    []string
		kind     string
		targets  []string
	}{
		{"対象会社の賛同", "<TDnet>AI: レオパレス21(8848) 株式会社K891による当社株券等に対する公開買付けに関する賛同の意見表明及び応募推奨に関するお知らせ",
			[]string{"8848"}, KindTOBTarget, []string{"8848"}},
		{"買付者側は証券コードの銘柄", "<TDnet>AI: 光通信(9435) 株式会社K891における株式会社レオパレス21（証券コード：8848）の株券等に対する公開買付けの開始に関するお知らせ",
			[]string{"9435"}, KindTOBTarget, []string{"8848"}},
		{"当社株式に対する", "<TDnet>AI: かどや(2612) 株式会社ITG-Gホールディングスによる当社株式に対する公開買付けに関する賛同の意見表明のお知らせ",
			[]string{"2612"}, KindTOBTarget, []string{"2612"}},
		{"訂正・コロン無しの証券コード", "<TDnet>AI: ムニノバHD(547A) （訂正）「あんしん保証株式会社株券（証券コード7183）に対する公開買付けの開始に関するお知らせ」の一部訂正",
			[]string{"547A"}, KindTOBTarget, []string{"7183"}},
		{"語の間の空白", "<TDnet>AI: ツインバード(6897) 株式会社ジャパネットホールディングスによる当社株式に対する 公開買付けの開始予定に関する反対の意見表明のお知らせ",
			[]string{"6897"}, KindTOBTarget, []string{"6897"}},
		{"対象会社の結果（親会社の異動）", "<TDnet>AI: あん保証(7183) 公開買付けの結果並びに親会社、その他の関係会社、主要株主及び主要株主である筆頭株主の異動に関するお知らせ",
			[]string{"7183"}, KindTOBTarget, []string{"7183"}},
		{"買付者側の結果（子会社の異動）は外さない", "<TDnet>AI: アカツキ(3932) 株式会社サニーサイドアップグループとの経営統合に向けた同社株券等に対する公開買付けの結果及び子会社の異動（特定子会社の異動）に関するお知らせ",
			[]string{"3932"}, "", nil},
		{"応募する株主は外さない", "<TDnet>AI: エステー(4951) 公開買付けへの応募及び特別利益の計上見込に関するお知らせ",
			[]string{"4951"}, "", nil},
		{"ＭＢＯ（全角）", "<TDnet>AI: サツドラHD(3544) ＭＢＯの実施及び応募の推奨に関するお知らせ",
			[]string{"3544"}, KindMBO, []string{"3544"}},
		{"MBO（半角）", "<TDnet>AI: デジハHD(3676) MBOの実施及び応募の推奨のお知らせ",
			[]string{"3676"}, KindMBO, []string{"3676"}},
		{"自己株式の公開買付は記録だけ", "<TDnet>AI: ENEOS(5020) ＪＸ金属株式会社による自己株式の公開買付けへの応募結果",
			[]string{"5020"}, KindSelfTender, []string{"5020"}},
		{"買集めは記録だけ", "<TDnet>AI: eWeLL(5038) 公開買付に準ずる行為として政令で定める買集め行為に関するお知らせ",
			[]string{"5038"}, KindAccumulation, []string{"5038"}},
		{"買収側の簡易株式交換は外さない", "<TDnet>AI: ビズメイツ(9345) 株式会社ヒップスターゲートの株式取得及び簡易株式交換による完全子会社化に関するお知らせ",
			[]string{"9345"}, "", nil},
		{"株式併合", "<TDnet>AI: 日本ドライ(1909) 法定事前開示書類（株式併合）",
			[]string{"1909"}, KindSqueezeOut, []string{"1909"}},
		{"株式等売渡請求", "<TDnet>AI: J.S.B.(3480) Ｕｒｓａ ４株式会社による当社株券等に対する株式等売渡請求を行うことの決定、当該株式等売渡請求に係る承認及び当社株式の上場廃止",
			[]string{"3480"}, KindSqueezeOut, []string{"3480"}},
		{"上場廃止", "<TDnet>AI: 養命酒(2540) 当社株式の上場廃止のお知らせ",
			[]string{"2540"}, KindDelisting, []string{"2540"}},
		{"上場廃止となった子会社は親会社を外さない", "<TDnet>AI: 前澤ＨＤ(575A) 上場廃止となった子会社（前澤工業株式会社）に関するお知らせ",
			[]string{"575A"}, "", nil},
		{"合併で上場廃止となった他社は外さない", "<TDnet>AI: クリレスHD(3387) 合併により上場廃止となったＳＦＰホールディングス株式会社に関する決算開示のお知らせ",
			[]string{"3387"}, "", nil},
		{"一部報道は記録だけ", "<TDnet>AI: かどや(2612) 一部報道について",
			[]string{"2612"}, KindPressReport, []string{"2612"}},
		{"関係の無い開示", "<TDnet>AI: トヨタ(7203) 業績予想の修正に関するお知らせ",
			[]string{"7203"}, "", nil},
	}
	for _, tc := range cases {
		kind, targets := Classify(tc.headline, tc.codes)
		if kind != tc.kind || !slices.Equal(targets, tc.targets) {
			t.Errorf("%s: Classify = (%q, %v), want (%q, %v)", tc.name, kind, targets, tc.kind, tc.targets)
		}
	}
}

func TestExcludes(t *testing.T) {
	for _, k := range []string{KindTOBTarget, KindMBO, KindSqueezeOut, KindShareExchange, KindDelisting} {
		if !Excludes(k) {
			t.Errorf("%s は外す", k)
		}
	}
	for _, k := range []string{KindSelfTender, KindAccumulation, KindPressReport, ""} {
		if Excludes(k) {
			t.Errorf("%q は外さない", k)
		}
	}
}

// 判定の時点より後に記録簿へ入った記事は使わない（その朝に知り得なかった材料で外さない）。
func TestCorporateEventsRespectsKnownAt(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	jst := time.FixedZone("JST", 9*3600)
	tob := []broker.NewsItem{{ID: "1", Time: "2000", Genres: []string{GenreTDnet}, Codes: []string{"8848"},
		Headline: "株式会社K891による当社株券等に対する公開買付けに関する賛同の意見表明"}}
	other := []broker.NewsItem{{ID: "2", Time: "0850", Genres: []string{GenreTDnet}, Codes: []string{"2612"},
		Headline: "株式会社ITG-Gホールディングスによる当社株式に対する公開買付けに関する賛同"}}
	rating := []broker.NewsItem{{ID: "3", Time: "0800", Genres: []string{"3001"}, Codes: []string{"9432"},
		Headline: "当社株式に対する公開買付け"}}
	if _, err := s.Save(ctx, "2026-09-14", tob, time.Date(2026, 9, 15, 6, 20, 0, 0, jst)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "2026-09-15", other, time.Date(2026, 9, 15, 8, 52, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "2026-09-15", rating, time.Date(2026, 9, 15, 6, 20, 0, 0, jst)); err != nil {
		t.Fatal(err)
	}
	knownAt := time.Date(2026, 9, 15, 9, 1, 0, 0, jst)
	got, err := s.CorporateEvents(ctx, "2026-07-17", "2026-09-15", knownAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["8848"]) != 1 || got["8848"][0].Kind != KindTOBTarget {
		t.Errorf("8848 = %+v, want tob_target 1 件", got["8848"])
	}
	// 08:52 UTC（= 17:52 JST）に入った記事は 9:01 JST には知り得ない
	if _, ok := got["2612"]; ok {
		t.Errorf("判定の時点より後に入った記事を使った: %+v", got["2612"])
	}
	if _, ok := got["9432"]; ok {
		t.Error("適時開示でないジャンルを判定した")
	}
	// 窓の外（配信日が from より前）は使わない
	got, err = s.CorporateEvents(ctx, "2026-09-15", "2026-09-15", knownAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["8848"]; ok {
		t.Error("窓の外の開示を使った")
	}
}

// 最後の取り込みは成功した日だけで見る。失敗も fetched_at を更新するので、混ぜると新しく見える。
func TestLastFetchedIgnoresFailures(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, ok, err := s.LastFetched(ctx); err != nil || ok {
		t.Fatalf("空の記録簿: ok=%v err=%v", ok, err)
	}
	jst := time.FixedZone("JST", 9*3600)
	okAt := time.Date(2026, 9, 15, 6, 20, 0, 0, jst)
	if _, err := s.Save(ctx, "2026-09-14", nil, okAt); err != nil {
		t.Fatal(err)
	}
	// UTC で書いた古い成功（文字列の大小では新しく見える）
	if _, err := s.Save(ctx, "2026-09-12", nil, time.Date(2026, 9, 14, 20, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailure(ctx, "2026-09-15", time.Date(2026, 9, 15, 8, 52, 0, 0, jst), "timeout"); err != nil {
		t.Fatal(err)
	}
	at, ok, err := s.LastFetched(ctx)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !at.Equal(okAt) {
		t.Errorf("LastFetched = %v, want %v", at, okAt)
	}
}
