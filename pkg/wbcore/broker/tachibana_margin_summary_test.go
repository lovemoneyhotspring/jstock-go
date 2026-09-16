package broker

// 委託保証金の内訳（CLMZanKaiKanougakuSuii）の読み取り。
// 行の項目名と値は 2026-09-16 に本番口座で確かめた応答から写してある。

import "testing"

// marginSuiiRowFixture は可能額の推移の 1 行。
func marginSuiiRowFixture(day, ukeire, genkin, daiyou, sinkidate, sonota string) map[string]any {
	return map[string]any{
		fieldSuiiDay: day, fieldSuiiUkeire: ukeire, fieldSuiiGenkin: genkin,
		fieldSuiiDaiyou: daiyou, fieldSuiiSinkidate: sinkidate,
		fieldSuiiSonota: sonota, fieldSuiiFusoku: "0",
	}
}

func TestMarginSummariesReadsBreakdown(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmMarginSuii] = okResponse(map[string]any{
		marginSuiiKey: []any{
			marginSuiiRowFixture("20260916", "3823546", "2055696", "1720000", "11586503", "0"),
			marginSuiiRowFixture("20260918", "3813827", "2093827", "1720000", "11557051", "3211"),
		},
	})

	rows, err := b.MarginSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("行数 %d, want 2", len(rows))
	}
	if got := rows[0].Date; got != "20260916" {
		t.Errorf("日付 %q, want 20260916", got)
	}
	// 受入保証金 = 現金 + 代用（掛目適用後）。実機で一致することを確かめてある
	sum := rows[1].GenkinHosyoukin.Add(rows[1].DaiyouHyoukagaku)
	if !sum.Equal(rows[1].UkeireHosyoukin) {
		t.Errorf("現金 + 代用 = %s, 受入保証金 = %s（一致すべき）", sum, rows[1].UkeireHosyoukin)
	}
	if got := rows[1].SonotaKousokukin.String(); got != "3211" {
		t.Errorf("その他拘束金 %s, want 3211", got)
	}
	if got := rows[1].SinyouSinkidate.String(); got != "11557051" {
		t.Errorf("信用新規建可能額 %s, want 11557051", got)
	}
}

// 不足額（追証）と拘束金は**項目が無いことを記録する**。fieldDecimal は欠けを 0 と読むので、
// この電文での項目名が実機と違っていれば「追証なし」と同じ見え方になり、
// 「追証の日は建てない」（margincap.Apply）が一度も発火しないまま気づけない。
// docs/BROKER_VERIFY.md の実機確認の一覧に不足額は入っていない（2026-09-17 のレビュー）。
func TestMarginSummariesRecordsMissingFields(t *testing.T) {
	b, fake := newFixtureBroker(t)
	row := marginSuiiRowFixture("20260916", "3823546", "2055696", "1720000", "11586503", "0")
	delete(row, fieldSuiiFusoku) // 項目名が違う口座・電文を模す
	fake.responses[clmMarginSuii] = okResponse(map[string]any{marginSuiiKey: []any{row}})

	rows, err := b.MarginSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 %d, want 1（不足額が無くても行は落とさない——建可能額は読めている）", len(rows))
	}
	if !rows[0].Fusokugaku.IsZero() {
		t.Errorf("不足額 %s, want 0（欠損は 0 として扱うが、欠損だったことを残す）", rows[0].Fusokugaku)
	}
	if len(rows[0].Missing) != 1 || rows[0].Missing[0] != fieldSuiiFusoku {
		t.Errorf("欠けた項目 = %v, want [%s]", rows[0].Missing, fieldSuiiFusoku)
	}
	// 揃っている行は Missing を持たない（毎朝の警告で狼少年にしない）
	full := marginSuiiRowFixture("20260918", "3813827", "2093827", "1720000", "11557051", "3211")
	fake.responses[clmMarginSuii] = okResponse(map[string]any{marginSuiiKey: []any{full}})
	rows, err = b.MarginSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[0].Missing) != 0 {
		t.Errorf("揃っている行に欠けが出ている: %v", rows[0].Missing)
	}
}

// 該当が 0 件のとき立花は配列ではなく空文字を返す（rowsOf が「該当なし」として通す）。
func TestMarginSummariesEmptyIsNotAnError(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmMarginSuii] = okResponse(map[string]any{marginSuiiKey: ""})

	rows, err := b.MarginSummaries()
	if err != nil {
		t.Fatalf("空の応答でエラーになった: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("行数 %d, want 0", len(rows))
	}
}

// 配列の鍵が無い応答は「形が違う」として弾く——読めないまま 0 円と解釈して
// 建玉を縮めたり広げたりしない。
func TestMarginSummariesRejectsUnknownShape(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmMarginSuii] = okResponse(map[string]any{"sSomethingElse": "1"})

	if _, err := b.MarginSummaries(); err == nil {
		t.Fatal("鍵の無い応答を通してしまった")
	}
}

// 日付の無い行は数えない（並びの末尾に空の枠が返ることがある）。
func TestMarginSummariesSkipsRowsWithoutDay(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmMarginSuii] = okResponse(map[string]any{
		marginSuiiKey: []any{
			marginSuiiRowFixture("20260916", "3823546", "2055696", "1720000", "11586503", "0"),
			marginSuiiRowFixture("", "", "", "", "", ""),
		},
	})

	rows, err := b.MarginSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 %d, want 1（日付の無い行は落とす）", len(rows))
	}
}

// 建可能額の項目が空の行も落とす。**欠損を 0 と読んではいけない**——margincap が
// 「建てられる額 0」と検算し、max_capital = 0（Validate を通る）に縮めるので、
// 設定の値で建てるはずの朝がまるごとノートレードになる。
// 建玉ゼロだと全項目が空文字で返る電文は実在する（docs/BROKER_VERIFY.md）。
func TestMarginSummariesSkipsRowsWithoutAvailableAmount(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmMarginSuii] = okResponse(map[string]any{
		marginSuiiKey: []any{
			marginSuiiRowFixture("20260916", "3823546", "2055696", "1720000", "11586503", "0"),
			marginSuiiRowFixture("20260918", "", "", "", "", ""),
		},
	})

	rows, err := b.MarginSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 %d, want 1（建可能額の無い行は落とす）", len(rows))
	}
	if got := rows[0].SinyouSinkidate.String(); got != "11586503" {
		t.Errorf("信用新規建可能額 %s, want 11586503", got)
	}
}
