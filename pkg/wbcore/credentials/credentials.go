package credentials

import (
	"fmt"
	"os"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/zalando/go-keyring"
)

type TachibanaCredentials struct {
	AuthID         string
	PrivateKeyFile string
	OrderPassword  string
}

// ResolveCredential は指定されたキーを (1) 環境変数, (2) .env マップ, (3) OS Keychain の優先順位で解決する。
func ResolveCredential(env settings.Environment, namespace, field string, dotenvMap map[string]string) string {
	envVarName := fmt.Sprintf("%s_%s_%s", strings.ToUpper(namespace), strings.ToUpper(string(env)), strings.ToUpper(field))

	// 1. 環境変数
	if val, ok := os.LookupEnv(envVarName); ok && val != "" {
		logging.RegisterSecret(val)
		return val
	}

	// 2. .env マップ
	if val, ok := dotenvMap[envVarName]; ok && val != "" {
		logging.RegisterSecret(val)
		return val
	}

	// 3. OS Keychain
	service := fmt.Sprintf("%s/%s", strings.ToLower(namespace), string(env))
	if val, err := keyring.Get(service, field); err == nil && val != "" {
		logging.RegisterSecret(val)
		return val
	}

	return ""
}

// LoadTachibanaCredentials は立花証券の認証情報を解決する。
func LoadTachibanaCredentials(env settings.Environment, dotenvMap map[string]string) (*TachibanaCredentials, error) {
	authID := ResolveCredential(env, "TACHIBANA", "AUTH_ID", dotenvMap)
	privKey := ResolveCredential(env, "TACHIBANA", "PRIVATE_KEY_FILE", dotenvMap)
	orderPass := ResolveCredential(env, "TACHIBANA", "ORDER_PASSWORD", dotenvMap)

	var missing []string
	if authID == "" {
		missing = append(missing, "AUTH_ID")
	}
	if privKey == "" {
		missing = append(missing, "PRIVATE_KEY_FILE")
	}
	if orderPass == "" {
		missing = append(missing, "ORDER_PASSWORD")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("立花証券（%s）の認証情報が不足しています: %s", env, strings.Join(missing, ", "))
	}

	return &TachibanaCredentials{
		AuthID:         authID,
		PrivateKeyFile: privKey,
		OrderPassword:  orderPass,
	}, nil
}

// LoadAPIKey は J-Quants などの API キーを解決する。
func LoadAPIKey(keyName string, dotenvMap map[string]string) (string, error) {
	if val, ok := os.LookupEnv(keyName); ok && val != "" {
		logging.RegisterSecret(val)
		return val, nil
	}
	if val, ok := dotenvMap[keyName]; ok && val != "" {
		logging.RegisterSecret(val)
		return val, nil
	}
	return "", fmt.Errorf("APIキーが見つかりません: %s", keyName)
}
