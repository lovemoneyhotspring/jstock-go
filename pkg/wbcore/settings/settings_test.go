package settings

import (
	"os"
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

// notify.Alert など AppSettings を経由せず os.Getenv を直接見るコードが
// .env の値を拾えるように、LoadAppSettings はプロセスの環境変数にも
// 反映する（cron は .env を source しないので、ここでしか読めない）。
func TestLoadAppSettingsExportsDotenvToProcessEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("WBJP_TEST_ONLY_KEY=from-dotenv\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WBJP_ENV_FILE", envPath)
	os.Unsetenv("WBJP_TEST_ONLY_KEY")
	t.Cleanup(func() { os.Unsetenv("WBJP_TEST_ONLY_KEY") })

	LoadAppSettings()

	if got := os.Getenv("WBJP_TEST_ONLY_KEY"); got != "from-dotenv" {
		t.Errorf("os.Getenv が .env の値を読めない: got %q, want %q", got, "from-dotenv")
	}
}

// 環境変数がすでにあれば .env で上書きしない（環境変数が優先という
// lookup と同じ優先順位を、プロセス全体でも保つ）。
func TestLoadAppSettingsDoesNotOverrideExistingEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("WBJP_TEST_ONLY_KEY2=from-dotenv\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WBJP_ENV_FILE", envPath)
	t.Setenv("WBJP_TEST_ONLY_KEY2", "from-os-env")

	LoadAppSettings()

	if got := os.Getenv("WBJP_TEST_ONLY_KEY2"); got != "from-os-env" {
		t.Errorf("既存の環境変数が .env で上書きされた: got %q", got)
	}
}

// 相対パスは .env のある場所から解く。作業ディレクトリから解くと、別の場所から動かした実行が
// 別の state を作り、本番のセッションを切る。
func TestLoadAppSettingsResolvesRelativePathsFromDotenvDir(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("WBJP_STATE_DIR=state\nWBJP_DATA_DIR=/srv/data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WBJP_ENV_FILE", envPath)
	for _, key := range []string{"WBJP_STATE_DIR", "WBJP_DATA_DIR", "WBJP_CONFIG_DIR", "WBJP_LOG_DIR"} {
		t.Setenv(key, "") // 空は「無い」と同じ扱い。.env の値が使われる
	}

	app := LoadAppSettings()

	if want := filepath.Join(dir, "state"); app.StateDir != want {
		t.Errorf("StateDir = %q, want %q（.env の場所から）", app.StateDir, want)
	}
	if app.DataDir != "/srv/data" {
		t.Errorf("DataDir = %q, want 絶対パスはそのまま", app.DataDir)
	}
	if want := filepath.Join(dir, "state", "logs"); app.LogDir != want {
		t.Errorf("LogDir = %q, want %q", app.LogDir, want)
	}
	if want := filepath.Join(dir, "config"); app.ConfigDir != want {
		t.Errorf("ConfigDir = %q, want %q", app.ConfigDir, want)
	}
}

// .env が無ければ作業ディレクトリから解く（絶対パスにはする）。
func TestLoadAppSettingsWithoutDotenvUsesWorkingDir(t *testing.T) {
	t.Setenv("WBJP_ENV_FILE", filepath.Join(t.TempDir(), "無い.env"))
	t.Setenv("WBJP_STATE_DIR", "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if app := LoadAppSettings(); app.StateDir != filepath.Join(cwd, "state") {
		t.Errorf("StateDir = %q, want %q", app.StateDir, filepath.Join(cwd, "state"))
	}
}
