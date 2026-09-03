package universe

import "sort"

// sortStable は安定ソート。同値の順位が入力順で決まることに依存している
// （polars の rank("ordinal") と同じ振る舞いにするため）。
func sortStable[T any](values []T, less func(a, b T) bool) {
	sort.SliceStable(values, func(i, j int) bool { return less(values[i], values[j]) })
}
