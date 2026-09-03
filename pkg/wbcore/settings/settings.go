package settings

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

type Environment string

const (
	EnvUAT  Environment = "uat"
	EnvProd Environment = "prod"
)

func (e Environment) IsProduction() bool {
	return e == EnvProd
}

// AppSettings は環境変数と .env から読み込まれる設定。
type AppSettings struct {
	Env       Environment
	ConfigDir string
	DataDir   string
	StateDir  string
	LogDir    string
	LogLevel  string
	LogJSON   bool
	Timezone  string
	DotenvMap map[string]string
}

// LoadAppSettings は環境変数と .env から設定を読み込む。
func LoadAppSettings() *AppSettings {
	envFile := os.Getenv("WBJP_ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}

	dotenvMap := make(map[string]string)
	if envs, err := godotenv.Read(envFile); err == nil {
		dotenvMap = envs
	}

	lookup := func(key, defaultVal string) string {
		if val, ok := os.LookupEnv(key); ok && val != "" {
			return val
		}
		if val, ok := dotenvMap[key]; ok && val != "" {
			return val
		}
		return defaultVal
	}

	envStr := strings.ToLower(lookup("WBJP_ENV", "uat"))
	env := EnvUAT
	if envStr == "prod" || envStr == "production" {
		env = EnvProd
	}

	configDir := lookup("WBJP_CONFIG_DIR", "config")
	dataDir := lookup("WBJP_DATA_DIR", "data")
	stateDir := lookup("WBJP_STATE_DIR", "state")
	logDir := lookup("WBJP_LOG_DIR", filepath.Join(stateDir, "logs"))
	logLevel := lookup("WBJP_LOG_LEVEL", "INFO")
	logJSON := lookup("WBJP_LOG_JSON", "false") == "true"
	timezone := lookup("WBJP_TIMEZONE", "UTC")

	return &AppSettings{
		Env:       env,
		ConfigDir: configDir,
		DataDir:   dataDir,
		StateDir:  stateDir,
		LogDir:    logDir,
		LogLevel:  logLevel,
		LogJSON:   logJSON,
		Timezone:  timezone,
		DotenvMap: dotenvMap,
	}
}

func (s *AppSettings) DBPath() string {
	return filepath.Join(s.StateDir, "wbjp-"+string(s.Env)+".db")
}

func (s *AppSettings) AccumDBPath() string {
	return filepath.Join(s.StateDir, "accum-"+string(s.Env)+".db")
}

func (s *AppSettings) DaytradeDBPath() string {
	return filepath.Join(s.StateDir, "daytrade-"+string(s.Env)+".db")
}

func (s *AppSettings) DaytradeDir() string {
	return filepath.Join(s.StateDir, "daytrade")
}

func (s *AppSettings) JQuantsLedgerPath() string {
	return filepath.Join(s.DataDir, "jquants", "ledger.db")
}

func (s *AppSettings) JQuantsArchiveDir() string {
	return filepath.Join(s.DataDir, "jquants")
}

func (s *AppSettings) BarsDir() string {
	return filepath.Join(s.DataDir, "bars")
}

// CanExecuteLive は実発注を行ってよいかを判定する。
// 条件: WBJP_ENV=prod かつ live フラグが真 かつ killSwitch が偽
func (s *AppSettings) CanExecuteLive(liveFlag bool, killSwitch bool) (bool, string) {
	if killSwitch {
		return false, "緊急停止（kill_switch = true）が有効なため発注しません"
	}
	if !liveFlag {
		return false, "--live フラグが無いため発注しません（dry-run）"
	}
	if !s.Env.IsProduction() {
		return false, "テスト環境（WBJP_ENV=" + string(s.Env) + "）のため実発注は行いません"
	}
	return true, "本番発注を行います"
}

// ---------------------------------------------------------------------------
// 履歴・バックアップ・ログの置き場
//
// data_dir（既定 data/）は取得元から再取得できるキャッシュで、ホスト間で丸ごと
// コピーしてよい。state_dir（既定 state/）は発注の台帳・ログ・バックアップ——
// そのホストで起きたことの唯一の記録で、他ホストのファイルで上書きしてはいけない。
// 以下の置き場はすべて state_dir 側に置く。
// ---------------------------------------------------------------------------

// ResolvedLogDir は実際に使うログの置き場。
// LoadAppSettings が WBJP_LOG_DIR を解決済みなので通常はそのまま返すが、
// 構造体を手で組み立てた場合に備えて既定（state_dir/logs）を補う。
func (s *AppSettings) ResolvedLogDir() string {
	if s.LogDir != "" {
		return s.LogDir
	}
	return filepath.Join(s.StateDir, "logs")
}

// LogFile はアプリ（wbjp / accum / daytrade）と環境ごとの JSONL。
// ファイルに残すログはここ 1 箇所だけ——置き場が分散すると障害時に
// どこを見ればよいか分からなくなる。
func (s *AppSettings) LogFile(app string) string {
	return filepath.Join(s.ResolvedLogDir(), app+"-"+string(s.Env)+".jsonl")
}

// DaytradeHistoryDir はデイトレの選定履歴（候補・気配・順位表・実行の要約）。
// plan-<日付>.parquet が「最新」なのに対し、こちらは実行のたびに 1 ファイル足す追記専用。
func (s *AppSettings) DaytradeHistoryDir() string {
	return filepath.Join(s.DaytradeDir(), "history")
}

// WbjpHistoryDir はスイング売買のスクリーニング履歴（wbjp screen）。追記専用の Parquet。
func (s *AppSettings) WbjpHistoryDir() string {
	return filepath.Join(s.StateDir, "wbjp", "history")
}

// AccumHistoryDir は積立の判断履歴（accum plan / run の判断と、その後の実績）。
func (s *AppSettings) AccumHistoryDir() string {
	return filepath.Join(s.StateDir, "accum", "history")
}

// HistoryDir はアプリ名から履歴の置き場を引く。
// 実行品質の記録（execution）が全アプリ共通でこれを使う。
func (s *AppSettings) HistoryDir(app string) string {
	switch app {
	case "daytrade":
		return s.DaytradeHistoryDir()
	case "accum":
		return s.AccumHistoryDir()
	default:
		return s.WbjpHistoryDir()
	}
}

// BackupDir は台帳バックアップの置き場（accum backup）。
func (s *AppSettings) BackupDir() string {
	return filepath.Join(s.StateDir, "backup")
}

// DigestDir は日次ダイジェストの置き場。アプリを分けないのは、
// AI が「今日どう動いたか」を 1 ファイルで読めるようにするため。
func (s *AppSettings) DigestDir() string {
	return filepath.Join(s.StateDir, "digest")
}

// DescribeMode は実行の冒頭に出す 1 行。
//
// 口座（WBJP_ENV）と発注の可否（--live）を混同すると事故になるので、必ず両方を並べて示す。
// 例: 口座: 本番口座（WBJP_ENV=prod）  発注: しない（--live フラグが無いため発注しません（dry-run））
func (s *AppSettings) DescribeMode(liveFlag bool, killSwitch bool) string {
	account := "テスト口座（実弾ではない）"
	if s.Env.IsProduction() {
		account = "本番口座"
	}
	allowed, reason := s.CanExecuteLive(liveFlag, killSwitch)
	orders := "しない"
	if allowed {
		orders = "する"
	}
	return "口座: " + account + "（WBJP_ENV=" + string(s.Env) + "）  発注: " + orders + "（" + reason + "）"
}
