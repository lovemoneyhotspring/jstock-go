// Package output は CLI の出力を「人が読む表」と「AI が読む JSON」に分けるための、
// JSON 側の共通部品。
//
// なぜ分けるか: 罫線・余白・色の制御文字は人には読みやすいが、AI に読ませると
// 意味の無いトークンを大量に食う。表を JSON にすると同じ内容が 3 分の 1 以下に
// なることが多い。そのため --json のときは表を一切出さず、1 個の JSON だけを
// 標準出力に書く（パイプでそのまま jq に渡せるように、警告や補足も混ぜない）。
//
// 金額の型: Decimal は JSON に無いので文字列にする（docs/LOGGING.md の規約と同じ）。
// 浮動小数にすると円未満の誤差が出るため、金額は必ず文字列で渡す。
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Rowser は「行の並びに畳める表」。history.Frame などが満たす。
// 具体型を参照すると import が循環するので、インターフェースで受ける。
type Rowser interface {
	ToMaps() []map[string]any
}

// Encode は JSON に載らない型を載る形に落とす。
//
// Python 版は json.dumps の default フックだったが、Go の encoding/json には
// 「任意の型を後から差し替える」フックが無いので、書き出す前に値を作り替える。
func Encode(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case decimal.Decimal:
		// 金額は誤差を避けるため文字列のまま
		return v.String()
	case *decimal.Decimal:
		if v == nil {
			return nil
		}
		return v.String()
	case time.Time:
		// 時刻が 00:00:00 UTC ちょうどなら「日付」として扱う（day 列など）
		if v.Hour() == 0 && v.Minute() == 0 && v.Second() == 0 && v.Nanosecond() == 0 && v.Location() == time.UTC {
			return v.Format("2006-01-02")
		}
		return v.Format(time.RFC3339Nano)
	case Rowser:
		return RowsOf(v)
	case []byte:
		return string(v)
	case error:
		return v.Error()
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = Encode(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Encode(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Encode(item)
		}
		return out
	case []string:
		return v
	case map[string]struct{}:
		// 集合は順序が無いので、差分を取りやすいよう並べ替えてから出す
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys
	case bool, string, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return v
	}
}

// RowsOf は表を行の並びにする。値の変換は Encode に任せる。
func RowsOf(frame Rowser) []map[string]any {
	if frame == nil {
		return []map[string]any{}
	}
	rows := frame.ToMaps()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		converted := make(map[string]any, len(row))
		for key, value := range row {
			converted[key] = Encode(value)
		}
		out = append(out, converted)
	}
	return out
}

// Dump は JSON の文字列にする。encoding/json はマップの鍵を辞書順に書くので、
// Python 版の sort_keys=True と同じ（差分を取りやすい）。
func Dump(payload map[string]any) (string, error) {
	encoded := Encode(payload)
	buf, err := json.Marshal(encoded)
	if err != nil {
		return "", fmt.Errorf("JSON への変換に失敗しました: %w", err)
	}
	return string(buf), nil
}

// EmitJSONTo は指定した書き出し先に JSON を 1 個だけ書く。
func EmitJSONTo(w io.Writer, payload map[string]any) error {
	text, err := Dump(payload)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, text+"\n")
	return err
}

// EmitJSON は標準出力に JSON を 1 個だけ書く。
func EmitJSON(payload map[string]any) error {
	return EmitJSONTo(os.Stdout, payload)
}

// EmitErrorTo は --json のときの失敗を、成功時と同じ形の JSON で書く。
//
// ok を必ず付けるのは、呼ぶ側が成否を 1 つの鍵で判定できるようにするため。
func EmitErrorTo(w io.Writer, message, code string) error {
	if code == "" {
		code = "error"
	}
	return EmitJSONTo(w, map[string]any{"ok": false, "error": code, "message": message})
}

// EmitError は標準出力に失敗の JSON を書く。
func EmitError(message, code string) error {
	return EmitErrorTo(os.Stdout, message, code)
}
