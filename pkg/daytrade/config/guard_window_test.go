package config

import "testing"

// guard_window を書き間違えると guard が毎回「時間帯の外」で黙って何もしなくなる。検査で弾く。
func TestValidateGuardWindow(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("既定の設定が検査を通らない: %v", err)
	}
	for _, bad := range [][]string{{"09:00"}, {"9時", "15:19"}, nil} {
		c := Default()
		c.Execution.GuardWindow = bad
		if err := c.Validate(); err == nil {
			t.Errorf("guard_window = %v を通した", bad)
		}
	}
}
