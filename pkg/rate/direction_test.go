package rate

import "testing"

func TestRatingDirection(t *testing.T) {
	cases := []struct {
		from, to, want string
	}{
		// 語の序列
		{"Buy", "Hold", DirectionDown},
		{"Hold", "Buy", DirectionUp},
		{"Neutral", "Overweight", DirectionUp},
		{"Equalweight", "Underweight", DirectionDown},
		{"買い", "中立", DirectionDown},
		{"中立", "買い", DirectionUp},
		{"Outperform", "Hold", DirectionDown},
		{"OP", "Neutral", DirectionDown},
		// 数値は 1 が最上位。小さくなれば格上げ
		{"2", "1", DirectionUp},
		{"2", "3", DirectionDown},
		{"1", "1", DirectionKeep},
		// A / B+ / B / B- / C
		{"B+", "A", DirectionUp},
		{"A", "B-", DirectionDown},
		{"B", "B+", DirectionUp},
		// 新規・据え置き
		{"", "Buy", DirectionNew},
		{"Buy", "Buy", DirectionKeep},
		// 表記ゆれ（全角・大文字小文字・かっこ書き）
		{"ＢＵＹ", "hold", DirectionDown},
		{"2 (中立)", "1 (アウトパフォーム)", DirectionUp},
		// 決められないものは keep に倒さない
		{"Ｚ社独自", "謎", DirectionUnknown},
		{"Buy", "謎", DirectionUnknown},
	}
	for _, c := range cases {
		if got := RatingDirection(c.from, c.to); got != c.want {
			t.Errorf("RatingDirection(%q, %q) = %s, 欲しいのは %s", c.from, c.to, got, c.want)
		}
	}
}

func TestRatingDirectionVariants(t *testing.T) {
	cases := []struct {
		from, to, want string
	}{
		// 略記（実データで多い）
		{"Over", "Neutral", DirectionDown},
		{"Neutral", "Over", DirectionUp},
		{"Equal", "Under", DirectionDown},
		{"Under", "Equal", DirectionUp},
		// UBS 形式。末尾の数字はリスク区分で、判断の強さではない
		{"Buy2", "Neutral2", DirectionDown},
		{"Neutral1", "Buy1", DirectionUp},
		{"Buy2", "Buy1", DirectionKeep},
		{"Reduce2", "Neutral2", DirectionUp},
		// 数値の半段階。1 が最上位なので 2+ は 2 より強気
		{"2", "2+", DirectionUp},
		{"2+", "2", DirectionDown},
		{"2", "2-", DirectionDown},
		{"3", "2+", DirectionUp},
		// 5 段階
		{"4", "5", DirectionDown},
		{"5", "1", DirectionUp},
		// A+ は A の上
		{"A", "A+", DirectionUp},
		{"A+", "B+", DirectionDown},
		// 出所の誤記
		{"Eqaul", "Over", DirectionUp},
		// 方式が変わった行は比べない
		{"Buy", "2", DirectionUnknown},
		{"2", "Buy", DirectionUnknown},
	}
	for _, c := range cases {
		if got := RatingDirection(c.from, c.to); got != c.want {
			t.Errorf("RatingDirection(%q, %q) = %s, 欲しいのは %s", c.from, c.to, got, c.want)
		}
	}
}
