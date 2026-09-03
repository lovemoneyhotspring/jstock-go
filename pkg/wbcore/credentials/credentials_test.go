package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

func keyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private.pem")
	if err := os.WriteFile(path, []byte("-----BEGIN PRIVATE KEY-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScopedNamesPrefersEnvSpecific(t *testing.T) {
	names := ScopedNames(settings.EnvProd, "TACHIBANA", "auth_id")
	if names[0] != "TACHIBANA_PROD_AUTH_ID" || names[1] != "TACHIBANA_AUTH_ID" {
		t.Fatalf("names = %v", names)
	}
	if KeyringService(settings.EnvProd, "TACHIBANA") != "tachibana/prod" {
		t.Errorf("KeyringService = %s", KeyringService(settings.EnvProd, "TACHIBANA"))
	}
}

func TestResolvePrefersEnvVarOverDotenv(t *testing.T) {
	t.Setenv("TACHIBANA_UAT_AUTH_ID", "from-environment")
	dotenv := map[string]string{"TACHIBANA_UAT_AUTH_ID": "from-dotenv"}

	value, source := ResolveCredentialWithSource(settings.EnvUAT, "TACHIBANA", "AUTH_ID", dotenv)
	if value != "from-environment" || source != SourceEnvVar {
		t.Fatalf("value = %q, source = %q", value, source)
	}
}

func TestResolveFallsBackToDotenvAndLegacyName(t *testing.T) {
	dotenv := map[string]string{"TACHIBANA_AUTH_ID": "legacy-value"}
	value, source := ResolveCredentialWithSource(settings.EnvUAT, "TACHIBANA", "AUTH_ID", dotenv)
	if value != "legacy-value" || source != SourceDotenv {
		t.Fatalf("後方互換の名前を見ていない: %q / %q", value, source)
	}
}

func TestCredentialSourceReportsMissing(t *testing.T) {
	got := CredentialSource(settings.EnvUAT, "TACHIBANA", TachibanaCredentialFields, nil)
	if !strings.Contains(got, "解決できません") || !strings.Contains(got, "AUTH_ID") {
		t.Fatalf("診断 = %s", got)
	}
}

func TestCredentialSourceReportsMixedOrigins(t *testing.T) {
	key := keyFile(t)
	t.Setenv("TACHIBANA_UAT_AUTH_ID", "aaa")
	dotenv := map[string]string{
		"TACHIBANA_UAT_PRIVATE_KEY_FILE": key,
		"TACHIBANA_UAT_ORDER_PASSWORD":   "bbb",
	}
	got := CredentialSource(settings.EnvUAT, "TACHIBANA", TachibanaCredentialFields, dotenv)
	// 項目ごとにソースが違うのは事故のもとなので、どれがどこから来たかを出す
	if !strings.Contains(got, "AUTH_ID="+SourceEnvVar) || !strings.Contains(got, "ORDER_PASSWORD="+SourceDotenv) {
		t.Fatalf("診断 = %s", got)
	}

	// すべて同じソースなら 1 語で答える
	t.Setenv("TACHIBANA_UAT_PRIVATE_KEY_FILE", key)
	t.Setenv("TACHIBANA_UAT_ORDER_PASSWORD", "ccc")
	if got := CredentialSource(settings.EnvUAT, "TACHIBANA", TachibanaCredentialFields, nil); got != SourceEnvVar {
		t.Fatalf("診断 = %s", got)
	}
}

func TestLoadTachibanaCredentials(t *testing.T) {
	key := keyFile(t)
	t.Setenv("TACHIBANA_UAT_AUTH_ID", "auth-1234")
	t.Setenv("TACHIBANA_UAT_PRIVATE_KEY_FILE", key)
	t.Setenv("TACHIBANA_UAT_ORDER_PASSWORD", "pass-1234")

	creds, err := LoadTachibanaCredentials(settings.EnvUAT, nil)
	if err != nil {
		t.Fatal(err)
	}
	if creds.AuthID != "auth-1234" || creds.PrivateKeyFile != key {
		t.Fatalf("creds = %+v", creds)
	}
	// うっかりログや例外に出しても秘密が漏れないこと
	if strings.Contains(creds.String(), "auth-1234") || strings.Contains(creds.String(), "pass-1234") {
		t.Errorf("String に秘密が出ている: %s", creds.String())
	}
}

func TestMissingCredentialsErrorExplainsSetup(t *testing.T) {
	_, err := LoadTachibanaCredentials(settings.EnvProd, nil)
	var missing *MissingCredentialsError
	if err == nil {
		t.Fatal("未設定なのに成功した")
	}
	if !asMissing(err, &missing) {
		t.Fatalf("MissingCredentialsError であるべき: %T", err)
	}
	if len(missing.Missing) != 3 {
		t.Errorf("不足項目 = %v", missing.Missing)
	}
	// 文面がそのまま手順書になること
	if !strings.Contains(err.Error(), "TACHIBANA_PROD_AUTH_ID") {
		t.Errorf("設定方法の案内が無い: %s", err)
	}
}

func TestMissingPrivateKeyFileIsReported(t *testing.T) {
	t.Setenv("TACHIBANA_UAT_AUTH_ID", "auth-1234")
	t.Setenv("TACHIBANA_UAT_PRIVATE_KEY_FILE", filepath.Join(t.TempDir(), "absent.pem"))
	t.Setenv("TACHIBANA_UAT_ORDER_PASSWORD", "pass-1234")

	_, err := LoadTachibanaCredentials(settings.EnvUAT, nil)
	if err == nil || !strings.Contains(err.Error(), "秘密鍵ファイルがありません") {
		t.Fatalf("err = %v", err)
	}
}

func TestWarnIfReadableByOthers(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open.env")
	if err := os.WriteFile(open, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if WarnIfReadableByOthers(open) == "" {
		t.Error("他ユーザーから読めるのに警告が無い")
	}

	tight := filepath.Join(dir, "tight.env")
	if err := os.WriteFile(tight, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := WarnIfReadableByOthers(tight); got != "" {
		t.Errorf("chmod 600 なのに警告: %s", got)
	}
	if got := WarnIfReadableByOthers(filepath.Join(dir, "absent")); got != "" {
		t.Errorf("存在しないファイルに警告: %s", got)
	}
}

func TestPermissionWarningsCoversEnvFileAndKey(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "private.pem")
	if err := os.WriteFile(key, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvFileOverrideVar, envFile)
	t.Setenv("TACHIBANA_UAT_PRIVATE_KEY_FILE", key)

	if EnvFilePath() != envFile {
		t.Fatalf("EnvFilePath = %s", EnvFilePath())
	}
	warnings := PermissionWarnings(settings.EnvUAT, nil)
	if len(warnings) != 2 {
		t.Fatalf("警告 = %v", warnings)
	}
}

func TestLoadAPIKey(t *testing.T) {
	t.Setenv("WBJP_JQUANTS_API_KEY", "key-from-env")
	if got, err := LoadAPIKey("WBJP_JQUANTS_API_KEY", nil); err != nil || got != "key-from-env" {
		t.Fatalf("got = %q, err = %v", got, err)
	}
	if got, err := LoadAPIKey("WBJP_OTHER_KEY", map[string]string{"WBJP_OTHER_KEY": "from-dotenv"}); err != nil || got != "from-dotenv" {
		t.Fatalf("got = %q, err = %v", got, err)
	}
	if _, err := LoadAPIKey("WBJP_ABSENT_KEY", nil); err == nil {
		t.Fatal("未設定なのに成功した")
	}
}

func asMissing(err error, target **MissingCredentialsError) bool {
	if e, ok := err.(*MissingCredentialsError); ok {
		*target = e
		return true
	}
	return false
}
