// Package registry は「名前 → 生成関数」の登録簿。
//
// 設定ファイルに書いた名前（"sma_cross" / "bear_stack"）から実体を引き、
// パラメータを渡して生成する。戦略でもブローカーでもデータソースでも
// この部分は同じなので、共通の部品にしてある。
//
// Python 版はクラスを登録し docstring から説明を拾っていたが、Go には
// クラスも docstring も無い。生成関数と 1 行説明を明示的に登録する形にする。
package registry

import (
	"fmt"
	"sort"
	"strings"
)

// Factory は登録された名前から実体を作る関数。params は設定ファイル由来の
// パラメータで、不要なら無視してよい。
type Factory[T any] func(params map[string]any) (T, error)

type entry[T any] struct {
	factory Factory[T]
	summary string
}

// Registry は name → Factory の対応表。
//
// label はエラーメッセージで対象を指す語（「戦略」など）。
type Registry[T any] struct {
	label string
	items map[string]entry[T]
}

// New は空の登録簿を作る。
func New[T any](label string) *Registry[T] {
	return &Registry[T]{label: label, items: make(map[string]entry[T])}
}

// Label は登録簿の呼び名。
func (r *Registry[T]) Label() string { return r.label }

// Register は生成関数を登録する。summary は一覧表示用の 1 行説明（省略可）。
//
// 同名の二重登録はエラーにする。設定の名前が衝突していると、意図しない実体が
// 選ばれて静かに違う売買をしてしまうため。
func (r *Registry[T]) Register(name, summary string, factory Factory[T]) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%sの登録に name が指定されていません", r.label)
	}
	if factory == nil {
		return fmt.Errorf("%s %q の生成関数が nil です", r.label, name)
	}
	if _, exists := r.items[name]; exists {
		return fmt.Errorf("%s名 %q は既に登録済みです", r.label, name)
	}
	r.items[name] = entry[T]{factory: factory, summary: summary}
	return nil
}

// MustRegister は初期化時（パッケージの init など）に使う Register。
func (r *Registry[T]) MustRegister(name, summary string, factory Factory[T]) {
	if err := r.Register(name, summary, factory); err != nil {
		panic(err)
	}
}

// Available は登録済みの名前を辞書順で返す。
func (r *Registry[T]) Available() []string {
	names := make([]string, 0, len(r.items))
	for name := range r.items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Get は名前から生成関数を引く。未知の名前には候補を添える。
func (r *Registry[T]) Get(name string) (Factory[T], error) {
	item, ok := r.items[name]
	if !ok {
		return nil, fmt.Errorf("未知の%s %q。利用可能: %v", r.label, name, r.Available())
	}
	return item.factory, nil
}

// Create は名前とパラメータから実体を作る。
func (r *Registry[T]) Create(name string, params map[string]any) (T, error) {
	var zero T
	factory, err := r.Get(name)
	if err != nil {
		return zero, err
	}
	value, err := factory(params)
	if err != nil {
		return zero, fmt.Errorf("%s %q のパラメータが不正です: %w", r.label, name, err)
	}
	return value, nil
}

// Contains は登録済みかどうか。
func (r *Registry[T]) Contains(name string) bool {
	_, ok := r.items[name]
	return ok
}

// Len は登録数。
func (r *Registry[T]) Len() int { return len(r.items) }

// SummaryOf は登録時に添えた 1 行説明。ReST の装飾記号は落とす
// （Python の docstring をそのまま移した文字列が来ることがあるため）。
func (r *Registry[T]) SummaryOf(name string) string {
	item, ok := r.items[name]
	if !ok {
		return ""
	}
	return SummaryOf(item.summary)
}

// SummaryOf は複数行の説明文から一覧表示用の 1 行を作る。
func SummaryOf(doc string) string {
	trimmed := strings.TrimSpace(doc)
	if trimmed == "" {
		return ""
	}
	first, _, _ := strings.Cut(trimmed, "\n")
	first = strings.ReplaceAll(first, "``", "")
	first = strings.ReplaceAll(first, "**", "")
	return strings.TrimSpace(first)
}

// Describe は名前と 1 行説明の一覧（名前順）。CLI の list 表示に使う。
func (r *Registry[T]) Describe() []Described {
	out := make([]Described, 0, len(r.items))
	for _, name := range r.Available() {
		out = append(out, Described{Name: name, Summary: r.SummaryOf(name)})
	}
	return out
}

// Described は一覧表示の 1 行。
type Described struct {
	Name    string
	Summary string
}
