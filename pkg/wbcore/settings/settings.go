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
	Env        Environment
	ConfigDir  string
	DataDir    string
	StateDir   string
	LogDir     string
	LogLevel   string
	LogJSON    bool
	Timezone   string
	DotenvMap  map[string]string
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
