package config

import "testing"

// 記録簿の窓と鮮度は、除外（exclude_corp_events）と guard（cancel_on_corp_event）の両方が使う。
// どちらか一方だけ使う設定でも 0 を弾く。
func TestValidateCorpEventWindow(t *testing.T) {
	for _, tc := range []struct {
		name            string
		exclude, cancel bool
		lookback, stale int
		wantErr         bool
	}{
		{"両方使う・既定", true, true, 120, 90, false},
		{"除外だけ・窓 0", true, false, 0, 90, true},
		{"guard だけ・窓 0", false, true, 0, 90, true},
		{"guard だけ・鮮度 0", false, true, 120, 0, true},
		{"guard だけ・正の値", false, true, 120, 90, false},
		{"どちらも使わない・0", false, false, 0, 0, false},
	} {
		c := Default()
		c.Margin.ExcludeCorpEvents = tc.exclude
		c.Margin.CancelOnCorpEvent = tc.cancel
		c.Margin.CorpEventLookbackDays = tc.lookback
		c.Margin.CorpEventMaxStalenessMinutes = tc.stale
		if err := c.Validate(); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", tc.name, err, tc.wantErr)
		}
	}
}
