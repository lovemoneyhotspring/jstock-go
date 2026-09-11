package rate

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// 投資判断の向き。
const (
	DirectionUp      = "up"
	DirectionDown    = "down"
	DirectionKeep    = "keep"
	DirectionNew     = "new"
	DirectionUnknown = "unknown"
)

// ratingWordRank は投資判断の語の強さ。大きいほど強気。
//
// 証券会社ごとに語彙が違うので、同じ強さのものを同じ値に寄せている。
// 段階数の違い（3 段階と 5 段階）は吸収しきれないが、**上がったか下がったか**を
// 見るだけならこれで足りる。
var ratingWordRank = map[string]int{
	// 最上位
	"strongbuy": 5, "強力買い": 5, "最上位": 5,
	// 買い
	"buy": 4, "買い": 4, "強気": 4, "outperform": 4, "op": 4, "overweight": 4, "ow": 4,
	"over": 4, "アウトパフォーム": 4, "a": 4, "add": 4, "accumulate": 4, "positive": 4,
	"買い推奨": 4, "オーバー": 4,
	"aplus": 5, // A+ は A の上
	// やや強気
	"bplus": 3, "bp": 3,
	// 中立
	"neutral": 2, "中立": 2, "hold": 2, "equalweight": 2, "equal": 2, "ew": 2, "e": 2,
	"eqaul":         2, // 出所の誤記
	"marketperform": 2, "b": 2, "n": 2,
	// やや弱気
	"bminus": 1,
	// 売り
	"sell": 0, "売り": 0, "弱気": 0, "underperform": 0, "up": 0, "underweight": 0, "uw": 0,
	"under": 0, "アンダーパフォーム": 0, "c": 0, "reduce": 0, "u": 0, "negative": 0,
	"underr": 0, "売り推奨": 0, "アンダー": 0,
}

var (
	// 数値の判断。"2" "2+" "2(中立)" を拾う。+ / - は半段階として扱う
	ratingNumRe = regexp.MustCompile(`^([1-5])(plus|minus)?`)
	// 英字で始まる判断の末尾に付く数字は UBS 形式のリスク区分（"Buy2" の 2）。
	// 判断の強さとは関係が無いので落とす
	ratingRiskRe  = regexp.MustCompile(`^([a-z\p{Han}\p{Hiragana}\p{Katakana}]+)[1-9]$`)
	ratingCleanRe = regexp.MustCompile(`[\s()（）・,，.。/／]`)
)

// RatingDirection は投資判断が上がったか下がったかを返す。
//
// トレーダーズ・ウェブの表記には「格上げ」「格下げ」の語が無く、"Buy→Hold" のように
// 前後が並ぶだけなので、序列に当てて比べる必要がある。
// 序列を決められない語（社独自の記号など）は unknown を返す——
// 勝手に keep に倒すと、判定できなかったことが数字の中に埋もれる。
func RatingDirection(from, to string) string {
	if strings.TrimSpace(to) == "" {
		return DirectionUnknown
	}
	if strings.TrimSpace(from) == "" {
		return DirectionNew
	}
	if normalizeRating(from) == normalizeRating(to) {
		return DirectionKeep
	}

	// 数値の判断（1 が最上位）は数の大小だけで決まる。
	// 段階数が社ごとに違っても、同じ社の中での前後比較なので問題にならない
	fromNum, fromIsNum := ratingNumber(from)
	toNum, toIsNum := ratingNumber(to)
	if fromIsNum && toIsNum {
		switch {
		case toNum < fromNum:
			return DirectionUp
		case toNum > fromNum:
			return DirectionDown
		default:
			return DirectionKeep
		}
	}
	// 片方だけが数値のときは比べようがない（"Buy→2" など社の方式が変わった行）
	if fromIsNum != toIsNum {
		return DirectionUnknown
	}

	fromRank, fromOK := ratingWordRank[normalizeRating(from)]
	toRank, toOK := ratingWordRank[normalizeRating(to)]
	if !fromOK || !toOK {
		return DirectionUnknown
	}
	switch {
	case toRank > fromRank:
		return DirectionUp
	case toRank < fromRank:
		return DirectionDown
	default:
		return DirectionKeep
	}
}

// ratingNumber は "2" "2+" "2(中立)" から段階の数を取る。
// "+" は半段階上（数が小さいほど強気なので -0.5）、"-" は半段階下。
func ratingNumber(s string) (float64, bool) {
	m := ratingNumRe.FindStringSubmatch(normalizeRating(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "plus":
		n -= 0.5
	case "minus":
		n += 0.5
	}
	return n, true
}

// normalizeRating は表記の揺れを寄せる。全角・大文字小文字・記号・かっこ書きを落とし、
// "B+" "B-" は語として扱えるよう bplus / bminus に直す。
func normalizeRating(s string) string {
	s = strings.ToLower(strings.TrimSpace(toHalfWidth(s)))
	s = strings.ReplaceAll(s, "+", "plus")
	s = strings.ReplaceAll(s, "-", "minus")
	s = strings.ReplaceAll(s, "−", "minus")
	s = ratingCleanRe.ReplaceAllString(s, "")
	// "Buy2" のリスク区分を落とす。"2plus" のような数値の判断は残す
	if m := ratingRiskRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// toHalfWidth は全角英数字・記号を半角にする。
func toHalfWidth(s string) string {
	return norm.NFKC.String(s)
}
