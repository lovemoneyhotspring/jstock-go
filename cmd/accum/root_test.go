package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
)

func TestSplitLogLine(t *testing.T) {
	stamp, level, code, message := splitLogLine("12:34:56 [warn] [accum.stale] 足が古い")
	if stamp != "12:34:56" || level != "warn" || code != "accum.stale" || message != "足が古い" {
		t.Errorf("got %q %q %q %q", stamp, level, code, message)
	}
	// 形が違う行は落とさない（水準を info とみなす）
	if _, level, _, message := splitLogLine("素の行"); level != "info" || message != "素の行" {
		t.Errorf("got %q %q", level, message)
	}
}

func TestTerminalWriterFiltersByLevel(t *testing.T) {
	var buf bytes.Buffer
	w := &terminalWriter{out: &buf, threshold: levelRank["warn"]}
	w.Write([]byte("12:00:00 [info] [c] 出さない\n12:00:01 [error] [c] 出す\n"))
	out := buf.String()
	if strings.Contains(out, "出さない") {
		t.Errorf("水準の低い行は落とすはず: %q", out)
	}
	if !strings.Contains(out, "出す") {
		t.Errorf("水準の高い行は出すはず: %q", out)
	}
}

func TestTerminalWriterEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	w := &terminalWriter{out: &buf, threshold: levelRank["info"], asJSON: true}
	w.Write([]byte("12:00:00 [info] [accum.sync] 同期しました\n"))
	out := buf.String()
	for _, want := range []string{`"level":"info"`, `"code":"accum.sync"`, `"msg":"同期しました"`} {
		if !strings.Contains(out, want) {
			t.Errorf("%s が無い: %q", want, out)
		}
	}
}

func TestParseDay(t *testing.T) {
	if got, err := parseDay("", "--from"); err != nil || got != "" {
		t.Errorf("省略は空のまま: %q %v", got, err)
	}
	if _, err := parseDay("2026-09-03", "--from"); err != nil {
		t.Errorf("正しい日付が弾かれた: %v", err)
	}
	if _, err := parseDay("2026/09/03", "--from"); err == nil {
		t.Error("形式違いは弾くはず")
	}
}

func TestYenRoundsLargeAmounts(t *testing.T) {
	if got := yen(12_345); got != "12345" {
		t.Errorf("小さい額はそのまま: %q", got)
	}
	if got := yen(1_234_567); got != "123万" {
		t.Errorf("大きい額は万に丸める: %q", got)
	}
}

func TestSyncDaysFor(t *testing.T) {
	// 日本株は J-Quants の配信範囲（約10年）で頭打ち
	if got := syncDaysFor(data.ProviderJQuants, defaultSyncDays); got != jquantsMaxSyncDays {
		t.Errorf("jquants の既定 = %d, want %d", got, jquantsMaxSyncDays)
	}
	// 上限より短い指定はそのまま
	if got := syncDaysFor(data.ProviderJQuants, runSyncDays); got != runSyncDays {
		t.Errorf("jquants の短い指定 = %d, want %d", got, runSyncDays)
	}
	// 30 年遡れる取得元は詰めない
	if got := syncDaysFor(data.ProviderFred, defaultSyncDays); got != defaultSyncDays {
		t.Errorf("fred の既定 = %d, want %d", got, defaultSyncDays)
	}
}
