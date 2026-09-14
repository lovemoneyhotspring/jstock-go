package rate

import "testing"

func TestRatingDirectionUnderperformAndHyphens(t *testing.T) {
	cases := []struct {
		from, to, want string
	}{
		// "UP" は Underperform の略（CLSA・CS・ML・マッコーリーで実データにある。
		// 例: CLSA の "BUY→UP" 31 行、ML の "UP→買い" 66 行）。格上げの意味ではない
		{"Buy", "UP", DirectionDown},
		{"UP", "OP", DirectionUp},
		{"Neutral", "UP", DirectionDown},
		{"UP", "UP", DirectionKeep},
		// 語の中のハイフンは区切り。minus にしない
		{"Equal-weight", "Overweight", DirectionUp},
		{"Over-weight", "Equal-weight", DirectionDown},
		{"Market-Perform", "Underperform", DirectionDown},
		// 末尾の "-" は半段階
		{"B-", "B", DirectionUp},
		{"B", "B-", DirectionDown},
		{"2-", "2", DirectionUp},
		{"2-(中立)", "2", DirectionUp},
		{"2−", "2", DirectionUp}, // U+2212
		// 前後を 1 語にしたような表記は序列に当たらない。keep に倒さない
		{"Buy-Hold", "Buy", DirectionUnknown},
	}
	for _, c := range cases {
		if got := RatingDirection(c.from, c.to); got != c.want {
			t.Errorf("RatingDirection(%q, %q) = %s, 欲しいのは %s（正規化 %q → %q）",
				c.from, c.to, got, c.want, normalizeRating(c.from), normalizeRating(c.to))
		}
	}
}
