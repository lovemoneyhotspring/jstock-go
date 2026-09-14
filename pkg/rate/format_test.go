package rate

import "testing"

func TestHHMM(t *testing.T) {
	cases := []struct {
		hours float64
		want  string
	}{
		{0, "00:00"},
		{7.0 + 10.0/60, "07:10"},
		{25.5, "25:30"}, // 日またぎは 24 時超えのまま
		{-0.5, "-00:30"},
		{-1.25, "-01:15"},
		{23.9999, "24:00"}, // 分に丸める
	}
	for _, c := range cases {
		if got := HHMM(c.hours); got != c.want {
			t.Errorf("HHMM(%v) = %q, want %q", c.hours, got, c.want)
		}
	}
}

func TestTrimTS(t *testing.T) {
	cases := map[string]string{
		"2026-09-11T07:05:00+09:00": "2026-09-11 07:05",
		"2026-09-11T07:05":          "2026-09-11 07:05",
		"2026-09-11":                "2026-09-11",
		"":                          "",
	}
	for in, want := range cases {
		if got := TrimTS(in); got != want {
			t.Errorf("TrimTS(%q) = %q, want %q", in, got, want)
		}
	}
}
