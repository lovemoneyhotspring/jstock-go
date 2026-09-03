package notify

import (
	"strings"
	"testing"
)

// 改行の位置で切る。上限を超えない限り 1 通にまとめる。
func TestChunksSplitsOnLineBreaks(t *testing.T) {
	text := "1行目\n2行目\n3行目\n"
	if got := Chunks(text, 100); len(got) != 1 || got[0] != text {
		t.Errorf("短い本文が分割されました: %q", got)
	}

	long := strings.Repeat("あいうえお\n", 10) // 1 行 6 文字 × 10
	pages := Chunks(long, 20)
	if len(pages) < 2 {
		t.Fatalf("分割されていません: %d 通", len(pages))
	}
	for i, p := range pages {
		if n := len([]rune(p)); n > 20 {
			t.Errorf("%d 通目が上限超過: %d 文字", i+1, n)
		}
	}
	if strings.Join(pages, "") != long {
		t.Error("分割で本文が変わりました")
	}
}

// 1 行が上限を超える異常な入力は、行の途中で切る（捨てない）。
func TestChunksSplitsOverlongLine(t *testing.T) {
	line := strings.Repeat("x", 55)
	pages := Chunks(line, 20)
	if len(pages) != 3 {
		t.Fatalf("通数 = %d, want 3", len(pages))
	}
	if strings.Join(pages, "") != line {
		t.Error("分割で本文が変わりました")
	}
}

func TestChunksEmpty(t *testing.T) {
	if got := Chunks("", 20); len(got) != 1 || got[0] != "" {
		t.Errorf("空文字 → %q", got)
	}
}
