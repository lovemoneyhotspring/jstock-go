package broker

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// 立花証券 e支店 API（e_api_v4r10）の応答を厳密に扱う道具。
//
// 方針は 1 つだけ——**「行が無い」と「応答の形が分からない」を必ず区別する。**
//
// 未検証の実装で怖いのは、項目名が違ったときに空の結果が返ることではなく、
// 空の結果が「注文はありません」「約定は 0 株です」として通ってしまうこと。
// 手仕舞いの数量が 0 になれば建玉は持ち越され、注文履歴が空になれば
// 二重買付のガードは素通りする。どちらも黙って起きる。
//
// そこで配列が入るはずのキーが**応答に存在しない**場合はエラーにし、
// キーはあるが要素が 0 件のときだけ「行が無い」として扱う。実機の項目名が
// 違えば最初の 1 回で明示的に落ちるので、名前を直す場所がすぐ分かる。

// ErrUnverifiedResponse は応答の形が想定と違うときのエラー。
//
// 実機（demo-kabuka / kabuka）で 1 度も検証できていない電文があるので、
// 項目名の食い違いはこの形で運用者に見せる。
type ErrUnverifiedResponse struct {
	CLMID    string
	Expected string
	Got      []string
	// GotType は期待した形と違ったときの実際の Go の型（項目名は合っているのに
	// 形が違う場合に、何が返ったかが分かるように）。
	GotType string
}

func (e *ErrUnverifiedResponse) Error() string {
	suffix := ""
	if e.GotType != "" {
		suffix = fmt.Sprintf("（実際の型: %s）", e.GotType)
	}
	return fmt.Sprintf(
		"%s の応答に %q がありません%s（実機で項目名を確認してください）。返ってきたキー: %s",
		e.CLMID, e.Expected, suffix, strings.Join(e.Got, ", "))
}

// checkResult は sResultCode を見る。"0" 以外は業務エラー。
//
// sResultCode 自体が無い応答は、電文が届いていない・形が違うということなので
// 成功とはみなさない。
func checkResult(res map[string]any, clmID string) error {
	code, ok := res["sResultCode"].(string)
	if !ok {
		return &ErrUnverifiedResponse{CLMID: clmID, Expected: "sResultCode", Got: keysOf(res)}
	}
	if strings.TrimSpace(code) != "0" {
		text, _ := res["sResultText"].(string)
		return fmt.Errorf("%s が失敗しました [%s]: %s", clmID, strings.TrimSpace(code), strings.TrimSpace(text))
	}
	return nil
}

// rowsOf は応答から配列を取り出す。
//
// キーが無ければ ErrUnverifiedResponse。キーはあるが空（または null）なら
// 空スライスと nil を返す——「該当なし」は正常な答え。
func rowsOf(res map[string]any, key, clmID string) ([]map[string]any, error) {
	raw, present := res[key]
	if !present {
		return nil, &ErrUnverifiedResponse{CLMID: clmID, Expected: key, Got: keysOf(res)}
	}
	if raw == nil {
		return nil, nil
	}
	// 該当が 0 件のとき、立花は配列ではなく**空文字**を返す（実機で確認。
	// 現物建玉の無い口座の aGenbutuKabuList が ""）。空白だけの文字列は「該当なし」
	// として通す——中身のある文字列は形が違うということなので、今までどおり弾く。
	if str, isStr := raw.(string); isStr && strings.TrimSpace(str) == "" {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, &ErrUnverifiedResponse{CLMID: clmID, Expected: key + "（配列）",
			Got: keysOf(res), GotType: fmt.Sprintf("%T", raw)}
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// keysOf は応答のキーを並べる（エラーメッセージ用）。
func keysOf(res map[string]any) []string {
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// field は行から文字列を取る。数値で返ってくることもあるので両方受ける。
func field(row map[string]any, key string) string {
	switch v := row[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return decimal.NewFromFloat(v).String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// fieldAny は候補の項目名を順に見て、最初に値のあったものを返す。
//
// 立花の電文は似た内容でも電文ごとに項目名が違う（約定数量が sOrderYakuzyouSuryo
// だったり sYakuzyouSuryo だったり）。実機で確かめるまでは候補を並べておく。
func fieldAny(row map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := field(row, k); v != "" {
			return v
		}
	}
	return ""
}

// fieldDecimal は行から数値を取る。空・解釈不能は 0。
func fieldDecimal(row map[string]any, keys ...string) decimal.Decimal {
	text := fieldAny(row, keys...)
	if text == "" {
		return decimal.Zero
	}
	// 桁区切りが入ることがある
	text = strings.ReplaceAll(text, ",", "")
	value, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Zero
	}
	return value
}

// fieldDecimalOK は fieldDecimal に「値があったか」を添えて返す。
// 0 と「項目が無い」を区別しないと、約定 0 株と未取得を取り違える。
func fieldDecimalOK(row map[string]any, keys ...string) (decimal.Decimal, bool) {
	text := fieldAny(row, keys...)
	if text == "" {
		return decimal.Zero, false
	}
	value, err := decimal.NewFromString(strings.ReplaceAll(text, ",", ""))
	if err != nil {
		return decimal.Zero, false
	}
	return value, true
}

// text は任意の値を文字列にする（応答の項目は文字列とは限らない）。
func text(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}
