// Package quotes は 9:00 の気配・現在値の取得元。
//
// 取得元は差し替え可能。`daytrade quotes 7203 9984` で疎通を確かめる。
//
//   - tachibana: 立花証券 e支店 API の時価問合（寄付後は始値、無ければ現在値）。本命
//   - csv: symbol,price[,at] のファイル。手で入れた気配や、別経路で取った値を流す
//     （検証・dry-run 用）
package quotes

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

// ErrQuote は気配を取れなかった。
var ErrQuote = errors.New("気配を取得できません")

// Source は気配の取得元。
type Source interface {
	// Name は設定とログで使う識別子。
	Name() string
	// Fetch は銘柄 → 気配。取れなかった銘柄は含めない。
	Fetch(symbols []string) (map[string]selection.Quote, error)
}

// CSV は symbol,price[,at] の CSV。at は ISO 8601（無ければ今）。
type CSV struct{ Path string }

// Name は取得元の識別子。
func (c *CSV) Name() string { return "csv" }

// Fetch は CSV から気配を読む。
func (c *CSV) Fetch(symbols []string) (map[string]selection.Quote, error) {
	wanted := make(map[string]struct{}, len(symbols))
	for _, s := range symbols {
		wanted[s] = struct{}{}
	}
	handle, err := os.Open(c.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: 気配の CSV がありません: %s", ErrQuote, c.Path)
	}
	defer handle.Close()

	reader := csv.NewReader(handle)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("%w: 気配の CSV を読めません: %v", ErrQuote, err)
	}
	index := map[string]int{}
	for i, name := range header {
		index[strings.TrimSpace(name)] = i
	}

	found := map[string]selection.Quote{}
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: 気配の CSV を読めません: %v", ErrQuote, err)
		}
		symbol := field(record, index, "symbol")
		price, ok := parsePrice(field(record, index, "price"))
		if !ok {
			continue
		}
		if _, want := wanted[symbol]; !want {
			continue
		}
		at := clock.NowUTC()
		if raw := field(record, index, "at"); raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				at = clock.EnsureUTC(parsed)
			}
		}
		found[symbol] = selection.Quote{Symbol: symbol, Price: price, At: at, Source: c.Name()}
	}
	return found, nil
}

// Tachibana は立花証券 e支店 API の時価問合（CLMMfdsGetMarketPrice）。
//
// 寄付後は始値（pDOP）、無ければ現在値（pDPP）。ブローカーの接続をそのまま使う。
type Tachibana struct{ Broker *broker.TachibanaBroker }

// Name は取得元の識別子。
func (t *Tachibana) Name() string { return "tachibana" }

// Connect は資格情報から立花証券に繋いだ取得元を作る。
func Connect(env settings.Environment, dotenv map[string]string, stateDir string) (*Tachibana, error) {
	creds, err := credentials.LoadTachibanaCredentials(env, dotenv)
	if err != nil {
		return nil, err
	}
	b, err := broker.NewTachibanaBroker(env, creds, stateDir)
	if err != nil {
		return nil, err
	}
	return &Tachibana{Broker: b}, nil
}

// Fetch は時価問合で気配を取る。
func (t *Tachibana) Fetch(symbols []string) (map[string]selection.Quote, error) {
	rows, err := t.Broker.MarketPrices(symbols)
	if err != nil {
		return nil, fmt.Errorf("%w: 立花証券の時価取得に失敗: %v", ErrQuote, err)
	}
	found := make(map[string]selection.Quote, len(rows))
	for symbol, row := range rows {
		// 寄付後は始値を使う。まだ寄っていなければ現在値（気配値）
		price := row.Open
		if price.LessThanOrEqual(decimal.Zero) {
			price = row.Last
		}
		if price.LessThanOrEqual(decimal.Zero) {
			continue
		}
		found[symbol] = selection.Quote{Symbol: symbol, Price: price, At: row.At, Source: t.Name()}
	}
	return found, nil
}

// Params は取得元を組み立てるのに必要な環境。
type Params struct {
	Env       settings.Environment
	Dotenv    map[string]string
	StateDir  string
	QuoteFile string
}

// New は設定の名前から取得元を組み立てる。
func New(name string, params Params) (Source, error) {
	switch name {
	case "tachibana":
		return Connect(params.Env, params.Dotenv, params.StateDir)
	case "csv":
		if params.QuoteFile == "" {
			return nil, fmt.Errorf(`quote_source = "csv" には quote_file が必要です`)
		}
		return &CSV{Path: params.QuoteFile}, nil
	default:
		return nil, fmt.Errorf("未知の quote_source: %q（tachibana / csv）", name)
	}
}

// Fresh は古い気配・遅延の気配を落とす。
//
// 寄付の成行は「今の板」に当たる。数分前の気配で順位を付けると、実際には条件を
// 満たしていない銘柄を買いに行く。allowDelayed は検証用の逃げ道。
func Fresh(received map[string]selection.Quote, maxAgeSeconds int, now time.Time, allowDelayed bool) (kept map[string]selection.Quote, stale, delayed []string) {
	limit := time.Duration(maxAgeSeconds) * time.Second
	kept = make(map[string]selection.Quote, len(received))
	for symbol, quote := range received {
		if quote.Delayed && !allowDelayed {
			delayed = append(delayed, symbol)
			continue
		}
		if now.Sub(quote.At) > limit && !allowDelayed {
			stale = append(stale, symbol)
			continue
		}
		kept[symbol] = quote
	}
	return kept, stale, delayed
}

func field(record []string, index map[string]int, name string) string {
	i, ok := index[name]
	if !ok || i >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[i])
}

func parsePrice(text string) (decimal.Decimal, bool) {
	if text == "" || text == "null" {
		return decimal.Zero, false
	}
	d, err := decimal.NewFromString(text)
	if err != nil || d.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, false
	}
	return d, true
}
