package tactics

import (
	"reflect"
	"strings"
	"testing"
)

// 一覧・比較はこの並びをそのまま使う。並びが変わると過去の比較表と列がずれる。
func TestAvailableKeepsDefinitionOrder(t *testing.T) {
	want := []string{"constant", "bear_stack", "stack_ladder", "drawdown_ladder"}
	if got := Available(); !reflect.DeepEqual(got, want) {
		t.Errorf("Available = %v, want %v", got, want)
	}
}

func TestSummary(t *testing.T) {
	for _, name := range Available() {
		if Summary(name) == "" {
			t.Errorf("%s の説明が空", name)
		}
	}
	if got := Summary("no_such_tactic"); got != "" {
		t.Errorf("未知の戦略の説明 = %q, want 空", got)
	}
}

func TestCreate(t *testing.T) {
	for _, name := range Available() {
		t.Run(name, func(t *testing.T) {
			tactic, err := Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if tactic.Name() != name {
				t.Errorf("Create(%q).Name() = %q", name, tactic.Name())
			}
		})
	}

	_, err := Create("no_such_tactic")
	if err == nil {
		t.Fatal("未知の戦略でもエラーにならない")
	}
	if !strings.Contains(err.Error(), "no_such_tactic") || !strings.Contains(err.Error(), "constant") {
		t.Errorf("何が未知で何が使えるのか分からない: %v", err)
	}
	if _, err := Create(""); err == nil {
		t.Error("空の名前でもエラーにならない")
	}
}

func TestCreateAll(t *testing.T) {
	all, err := CreateAll()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(all))
	for _, tactic := range all {
		names = append(names, tactic.Name())
	}
	if !reflect.DeepEqual(names, Available()) {
		t.Errorf("CreateAll の並び = %v, want %v", names, Available())
	}
}
