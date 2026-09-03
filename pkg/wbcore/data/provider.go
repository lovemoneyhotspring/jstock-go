package data

import (
	"errors"
	"fmt"
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 市場データ取得の抽象。
//
// なぜブローカーと分けるのか:
// 証券会社の市場データ API は、扱える市場が発注できる市場と一致するとは限らない
// （日本株を発注できても足は返さない、など）。つまり「発注は証券会社、価格は別ソース」
// という構成が避けられない。そこでこの層を抽象化しておくと、データ源を差し替えても
// 戦略・エンジン・ブローカーには一切影響が出ない。
//
// 扱うのは日足だけ。J-Quants も FRED も日足しか返さず、立花証券の API にも
// 足の履歴は無い。
//
// データ源を足すには:
//  1. MarketDataProvider を満たす型を書く（Name / FetchBars）
//  2. 設定から選べるようにするなら Providers に登録する（registry.go）

// ErrMarketData は市場データの取得に失敗したことを表す。
// 個別の失敗はこれを包んで返す（errors.Is で判別できる）。
var ErrMarketData = errors.New("市場データの取得に失敗しました")

// MarketDataError は取得失敗のエラー。
type MarketDataError struct {
	Provider string
	Msg      string
	Err      error
}

func (e *MarketDataError) Error() string {
	text := e.Msg
	if e.Provider != "" {
		text = e.Provider + ": " + text
	}
	if e.Err != nil {
		return text + ": " + e.Err.Error()
	}
	return text
}

func (e *MarketDataError) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	return ErrMarketData
}

// Is は errors.Is(err, ErrMarketData) を通す。
func (e *MarketDataError) Is(target error) bool { return target == ErrMarketData }

// NewMarketDataError は取得失敗のエラーを作る。
func NewMarketDataError(provider, msg string, err error) *MarketDataError {
	return &MarketDataError{Provider: provider, Msg: msg, Err: err}
}

// BarColumns は日足の正規スキーマ。どの実装もこの形（domain.Bar）で返す。
var BarColumns = []string{"date", "open", "high", "low", "close", "volume"}

// MarketDataProvider は足を供給するもの。
type MarketDataProvider interface {
	// Name は設定（universe.data_provider）とログで使う識別子。
	Name() string

	// FetchBars は日足を取得する。
	//
	// symbols は銘柄コード（日本株は "7203" のような 4 桁）。start / end は
	// "YYYY-MM-DD" で、両端を含む。
	//
	// 返すのは 銘柄コード → 日付昇順の足。取得できなかった銘柄はキーごと省く
	// （空のスライスではなく不在にすることで、「データが無い」と「値動きが無い」を
	// 区別できる）。
	FetchBars(symbols []string, start, end string) (map[string][]domain.Bar, error)
}

// EmptyBars は正規スキーマの空の足。
func EmptyBars() []domain.Bar { return []domain.Bar{} }

// NormalizeBars は任意の足を正規スキーマに揃える。
//
//   - 日付昇順に並べる
//   - 同じ日は後勝ちで 1 本にまとめる
//   - 価格が欠けている行を落とす
//
// 重複除去が要るのは、増分取得したデータと既存データを継ぎ足すと境界が二重に
// なるため。放置すると指標の窓がずれる。
func NormalizeBars(bars []domain.Bar) ([]domain.Bar, error) {
	byDate := make(map[string]domain.Bar, len(bars))
	for _, bar := range bars {
		if bar.Date == "" {
			continue
		}
		if !hasPrices(bar) {
			// 価格の無い行は指標を壊すので落とす（出来高 0 は落とさない——薄商いは事実）
			continue
		}
		// 後勝ち: 訂正された足が既存を上書きする
		byDate[bar.Date] = bar
	}
	dates := make([]string, 0, len(byDate))
	for date := range byDate {
		dates = append(dates, date)
	}
	// 日付は ISO 8601（YYYY-MM-DD）なので、文字列の昇順＝時系列の昇順
	sort.Strings(dates)

	out := make([]domain.Bar, 0, len(dates))
	for _, date := range dates {
		bar := byDate[date]
		if bar.High.LessThan(bar.Low) {
			return nil, fmt.Errorf("%s %s: high < low (%s < %s)", bar.Symbol, bar.Date, bar.High, bar.Low)
		}
		out = append(out, bar)
	}
	return out, nil
}

func hasPrices(bar domain.Bar) bool {
	for _, price := range []decimal.Decimal{bar.Open, bar.High, bar.Low, bar.Close} {
		if price.LessThanOrEqual(decimal.Zero) {
			return false
		}
	}
	return true
}

// FilterBars は期間（両端を含む）で足を絞る。空文字は「制限なし」。
func FilterBars(bars []domain.Bar, start, end string) []domain.Bar {
	out := make([]domain.Bar, 0, len(bars))
	for _, bar := range bars {
		if start != "" && bar.Date < start {
			continue
		}
		if end != "" && bar.Date > end {
			continue
		}
		out = append(out, bar)
	}
	return out
}
