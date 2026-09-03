package settings

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestSettings() *AppSettings {
	return &AppSettings{
		Env:      EnvUAT,
		DataDir:  "data",
		StateDir: "state",
		LogDir:   "",
	}
}

func TestHistoryDirs(t *testing.T) {
	s := newTestSettings()
	cases := map[string]string{
		"daytrade": filepath.Join("state", "daytrade", "history"),
		"accum":    filepath.Join("state", "accum", "history"),
		"wbjp":     filepath.Join("state", "wbjp", "history"),
		// 知らないアプリはスイング売買の置き場に寄せる（Python 版と同じ）
		"unknown": filepath.Join("state", "wbjp", "history"),
	}
	for app, want := range cases {
		if got := s.HistoryDir(app); got != want {
			t.Errorf("HistoryDir(%q) = %s, want %s", app, got, want)
		}
	}
	if s.DaytradeHistoryDir() != cases["daytrade"] {
		t.Errorf("DaytradeHistoryDir = %s", s.DaytradeHistoryDir())
	}
}

// 台帳・ログ・バックアップは state_dir 側（ホスト固有の記録）に置くこと。
func TestStatefulPathsLiveUnderStateDir(t *testing.T) {
	s := newTestSettings()
	for _, path := range []string{s.BackupDir(), s.DigestDir(), s.LogFile("accum"), s.ResolvedLogDir()} {
		if !strings.HasPrefix(path, "state") {
			t.Errorf("state_dir の外にある: %s", path)
		}
	}
	if s.LogFile("accum") != filepath.Join("state", "logs", "accum-uat.jsonl") {
		t.Errorf("LogFile = %s", s.LogFile("accum"))
	}
}

func TestResolvedLogDirHonorsOverride(t *testing.T) {
	s := newTestSettings()
	s.LogDir = "/var/log/wbjp"
	if s.ResolvedLogDir() != "/var/log/wbjp" {
		t.Errorf("ResolvedLogDir = %s", s.ResolvedLogDir())
	}
	if s.LogFile("wbjp") != filepath.Join("/var/log/wbjp", "wbjp-uat.jsonl") {
		t.Errorf("LogFile = %s", s.LogFile("wbjp"))
	}
}

// 口座と発注の可否は別の話なので、必ず両方を並べて示す。
func TestDescribeModeShowsAccountAndOrders(t *testing.T) {
	s := newTestSettings()
	line := s.DescribeMode(false, false)
	if !strings.Contains(line, "テスト口座") || !strings.Contains(line, "発注: しない") {
		t.Errorf("uat / --live なし = %s", line)
	}

	s.Env = EnvProd
	if line := s.DescribeMode(true, false); !strings.Contains(line, "本番口座") || !strings.Contains(line, "発注: する") {
		t.Errorf("prod / --live あり = %s", line)
	}
	// キルスイッチはすべてに優先する
	if line := s.DescribeMode(true, true); !strings.Contains(line, "発注: しない") || !strings.Contains(line, "緊急停止") {
		t.Errorf("キルスイッチ = %s", line)
	}
}
