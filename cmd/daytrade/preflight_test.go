package main

import (
	"errors"
	"strings"
	"testing"
)

// 1 つ落ちても残りの点検は続ける。問題は人向けの行になる。
func TestPreflightProblemsRunsEveryCheck(t *testing.T) {
	ran := 0
	ok := func() error { ran++; return nil }
	bad := func() error { ran++; return errors.New("unknown field \"x\"") }

	problems := preflightProblems("2026-09-24", bad, ok, bad, ok)
	if ran != 4 {
		t.Errorf("回った点検 = %d, want 4（途中で止めない）", ran)
	}
	if len(problems) != 2 || !strings.Contains(problems[0], "deploy/build.sh") || !strings.Contains(problems[1], "台帳") {
		t.Errorf("problems = %v", problems)
	}
	if got := preflightProblems("2026-09-24", ok, ok, ok, ok); len(got) != 0 {
		t.Errorf("問題なしなのに %v", got)
	}
}

func TestCheckFreeSpace(t *testing.T) {
	dir := t.TempDir()
	if err := checkFreeSpace(dir, 1); err != nil {
		t.Errorf("空きがあるのに: %v", err)
	}
	if err := checkFreeSpace(dir, 1<<62); err == nil {
		t.Error("空きが足りないのに通った")
	}
	if err := checkFreeSpace(dir+"/無い", 1); err == nil {
		t.Error("無いディレクトリが通った")
	}
}
