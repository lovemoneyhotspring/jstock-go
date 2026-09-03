package data

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// IndexCodes は指数の別名 → J-Quants の指数コード。
//
// 4 桁の指数コードは `^0028` のように直接書いてもよく、この表は
// よく使うものに読める名前を与えているだけ。
var IndexCodes = map[string]string{
	"^TOPIX": "0000",
}

// 東証の銘柄コード（4 桁。452A / 5A29 のように 2・4 桁目は英字も可）と
// J-Quants の 5 桁コード（72030）を受け付ける。
var equityCodePattern = regexp.MustCompile(`^[0-9][0-9A-Z]{3}[0-9]?$`)

// ToJQuantsCode は銘柄コードを J-Quants の code にする。
//
// 7203 / 7203.T / 452A.T のような株式はそのまま（.T は落とす）、
// ^TOPIX / ^0028 のような指数は 4 桁の指数コードに直し、isIndex に true を返す。
// 東証の銘柄コードでも指数でもなければエラー——米国の指数（^GSPC 等）が
// 混ざったときに、黙って株式として問い合わせないため。
func ToJQuantsCode(symbol string) (code string, isIndex bool, err error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return "", false, fmt.Errorf("銘柄コードが空です")
	}

	if strings.HasPrefix(symbol, "^") {
		code, ok := IndexCodes[symbol]
		if !ok {
			code = symbol[1:]
		}
		if !isFourDigits(code) {
			names := make([]string, 0, len(IndexCodes))
			for name := range IndexCodes {
				names = append(names, name)
			}
			sort.Strings(names)
			return "", false, fmt.Errorf(
				"J-Quants で取れない指数です: %s（利用可能: %s または ^ ＋ 4 桁の指数コード）",
				symbol, strings.Join(names, ", "))
		}
		return code, true, nil
	}

	code = strings.TrimSuffix(symbol, ".T")
	if !equityCodePattern.MatchString(code) {
		return "", false, fmt.Errorf("東証の銘柄コードではありません: %s", symbol)
	}
	return code, false, nil
}

// JQuantsDailyPath は日足のエンドポイント。指数と株式でパスが違う。
func JQuantsDailyPath(isIndex bool) string {
	if isIndex {
		return "/indices/bars/daily"
	}
	return "/equities/bars/daily"
}

func isFourDigits(code string) bool {
	if len(code) != 4 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
