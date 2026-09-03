// Package credentials は証券会社 API の認証情報。
//
// 方針:
//   - 秘密はリポジトリに置かない。ローカル開発では OS のキーチェーンに保管する。
//     キーチェーンの無いサーバー（ヘッドレスな Linux など）では環境変数か、
//     パーミッションを絞った .env を使う
//   - 環境（uat / prod）ごとに認証情報を完全に分ける。取り違えて本番に発注する
//     事故を、名前空間の分離で防ぐ
//
// 名前空間は証券会社ごとに分ける。立花証券は TACHIBANA——環境変数は
// TACHIBANA_PROD_AUTH_ID、キーチェーンは tachibana/prod。プロジェクト（売買 / 積立）を
// 分けても口座は同じなので、名前空間はプロジェクトではなく証券会社に紐づける。
//
// データ源の鍵（J-Quants など、口座と紐づかないもの）は LoadAPIKey で単体の
// 変数名から引く。
package credentials

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/zalando/go-keyring"
)

// TachibanaNamespace は立花証券 e支店 API の名前空間。
const TachibanaNamespace = "TACHIBANA"

// TachibanaCredentialFields は立花証券 e支店 API（e_api_v4r10）の認証情報を構成する項目。
//
//   - AUTH_ID — ログイン電文 CLMAuthLoginRequest の sAuthId。
//     e支店 Web の「ｅ支店・ＡＰＩ利用設定」で自動生成される認証ID
//   - PRIVATE_KEY_FILE — 同画面で登録した公開鍵と対の秘密鍵（PEM）のファイルパス。
//     ログイン応答の仮想URLは公開鍵で暗号化されて返るので、これで復号する
//   - ORDER_PASSWORD — 新規注文・取消注文の必須パラメータ sSecondPassword（第二暗証番号）
var TachibanaCredentialFields = []string{"AUTH_ID", "PRIVATE_KEY_FILE", "ORDER_PASSWORD"}

// EnvFileOverrideVar は .env の場所を絶対パスで上書きする環境変数。
//
// cron は $HOME で起動するため、相対パスの .env は（しかも黙って）見つからない。
const EnvFileOverrideVar = "WBJP_ENV_FILE"

// DefaultEnvFile は既定の .env の場所（プロセスのカレントディレクトリ）。
const DefaultEnvFile = ".env"

// 取得元のラベル。診断表示（CredentialSource）に出す。
const (
	SourceEnvVar  = "環境変数"
	SourceKeyring = "OS キーチェーン"
	SourceDotenv  = ".env"
)

// MissingCredentialsError は認証情報が見つからないことを表す。
//
// どこを設定すればよいかまで書くのは、この失敗が「運用者が設定を忘れた」
// ときにしか起きないため——例外の文面がそのまま手順書になる。
type MissingCredentialsError struct {
	Env       settings.Environment
	Namespace string
	Missing   []string
	// Detail は個別の事情（秘密鍵ファイルが無い 等）。
	Detail string
}

func (e *MissingCredentialsError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	upper := strings.ToUpper(string(e.Env))
	return fmt.Sprintf(
		"%s 環境の認証情報（%s）が不足しています: %s\n"+
			"  環境変数:     %s_%s_AUTH_ID / _PRIVATE_KEY_FILE / _ORDER_PASSWORD\n"+
			"  または .env に記載: %s_%s_AUTH_ID=... （chmod 600 のこと）",
		e.Env, e.Namespace, strings.Join(e.Missing, ", "),
		e.Namespace, upper, e.Namespace, upper,
	)
}

// TachibanaCredentials は立花証券 e支店 API の認証情報。
//
// String を潰してあるので、うっかりログや例外に出しても秘密が漏れない。
type TachibanaCredentials struct {
	AuthID         string
	PrivateKeyFile string
	OrderPassword  string
}

func (c TachibanaCredentials) String() string {
	tail := c.AuthID
	if len(tail) > 2 {
		tail = tail[len(tail)-2:]
	}
	return "TachibanaCredentials(auth_id='***" + tail + "')"
}

// EnvFilePath は使う .env の場所。WBJP_ENV_FILE があればそれ。
func EnvFilePath() string {
	if override := os.Getenv(EnvFileOverrideVar); override != "" {
		return override
	}
	return DefaultEnvFile
}

// WarnIfReadableByOthers は秘密のファイルが自分以外から読めるなら警告を返す。
// 警告文（空なら問題なし）を返すだけで、実行は止めない。
func WarnIfReadableByOthers(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		// stat が失敗するなら読み込みも失敗する。そちらで報告される
		return ""
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Sprintf("%s が他ユーザーから読める状態です（%04o）。chmod 600 を推奨します", path, mode)
	}
	return ""
}

// ScopedNames は 1 つの項目に対応する環境変数名を優先順に返す。
// TACHIBANA_UAT_AUTH_ID を優先し、TACHIBANA_AUTH_ID を後方互換として見る。
func ScopedNames(env settings.Environment, namespace, field string) []string {
	ns := strings.ToUpper(namespace)
	f := strings.ToUpper(field)
	return []string{
		ns + "_" + strings.ToUpper(string(env)) + "_" + f,
		ns + "_" + f,
	}
}

// KeyringService はキーチェーンのサービス名（tachibana/prod）。
func KeyringService(env settings.Environment, namespace string) string {
	return strings.ToLower(namespace) + "/" + string(env)
}

// ResolveCredentialWithSource は 1 項目を解決し、値とその取得元のラベルを返す。
//
// 優先順位は 環境変数 → .env → OS キーチェーン。診断（CredentialSource）と
// 実際の読み込みが同じ解決を通るようにするための土台——別々に判定すると、
// 使われた認証情報と診断表示がずれる。
func ResolveCredentialWithSource(env settings.Environment, namespace, field string, dotenvMap map[string]string) (string, string) {
	names := ScopedNames(env, namespace, field)

	for _, name := range names {
		if val, ok := os.LookupEnv(name); ok && val != "" {
			logging.RegisterSecret(val)
			return val, SourceEnvVar
		}
	}
	for _, name := range names {
		if val, ok := dotenvMap[name]; ok && val != "" {
			logging.RegisterSecret(val)
			return val, SourceDotenv
		}
	}
	// ヘッドレスな Linux には SecretService も D-Bus も無く、keyring は
	// エラーを返す。ここで落とすと設定方法を案内する MissingCredentialsError に
	// 到達できないので、「見つからなかった」として扱う
	service := KeyringService(env, namespace)
	for _, key := range []string{field, strings.ToLower(field)} {
		if val, err := keyring.Get(service, key); err == nil && val != "" {
			logging.RegisterSecret(val)
			return val, SourceKeyring
		}
	}
	return "", ""
}

// ResolveCredential は指定されたキーを 環境変数 → .env → OS キーチェーン の順で解決する。
func ResolveCredential(env settings.Environment, namespace, field string, dotenvMap map[string]string) string {
	value, _ := ResolveCredentialWithSource(env, namespace, field, dotenvMap)
	return value
}

// CredentialSource は認証情報がどこから来たかを人間向けに説明する。
//
// どこから読まれているか分からないまま本番に発注する事故を防ぐための診断。
// 秘密そのものは返さない。実際に採用されるものを説明すること——項目ごとに
// 取得元が違いうるので、1 項目だけを見て「.env から読んだ」と表示すると嘘になる。
func CredentialSource(env settings.Environment, namespace string, fields []string, dotenvMap map[string]string) string {
	origins := make(map[string]string, len(fields))
	var missing []string
	for _, field := range fields {
		value, origin := ResolveCredentialWithSource(env, namespace, field, dotenvMap)
		if value == "" {
			missing = append(missing, field)
			continue
		}
		origins[field] = origin
	}
	if len(missing) > 0 {
		return fmt.Sprintf("解決できません（%s が未設定）", strings.Join(missing, ", "))
	}

	labels := map[string]struct{}{}
	for _, origin := range origins {
		labels[origin] = struct{}{}
	}
	if len(labels) == 1 {
		for label := range labels {
			return label
		}
	}
	// 項目ごとにソースが違うのは事故のもと。どれがどこから来たかを出す
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, field+"="+origins[field])
	}
	return strings.Join(parts, ", ")
}

// LoadTachibanaCredentials は立花証券の認証情報を解決する。
//
// 秘密鍵は PRIVATE_KEY_FILE のパスから読む（chmod 600 のこと）。
func LoadTachibanaCredentials(env settings.Environment, dotenvMap map[string]string) (*TachibanaCredentials, error) {
	values := make(map[string]string, len(TachibanaCredentialFields))
	var missing []string
	for _, field := range TachibanaCredentialFields {
		value := ResolveCredential(env, TachibanaNamespace, field, dotenvMap)
		if value == "" {
			missing = append(missing, field)
			continue
		}
		values[field] = value
	}
	if len(missing) > 0 {
		return nil, &MissingCredentialsError{Env: env, Namespace: TachibanaNamespace, Missing: missing}
	}

	keyPath := expandUser(values["PRIVATE_KEY_FILE"])
	if info, err := os.Stat(keyPath); err != nil || info.IsDir() {
		return nil, &MissingCredentialsError{
			Env: env, Namespace: TachibanaNamespace, Missing: []string{"PRIVATE_KEY_FILE"},
			Detail: fmt.Sprintf("立花証券の秘密鍵ファイルがありません: %s（%s_%s_PRIVATE_KEY_FILE）",
				keyPath, TachibanaNamespace, strings.ToUpper(string(env))),
		}
	}

	return &TachibanaCredentials{
		AuthID:         values["AUTH_ID"],
		PrivateKeyFile: keyPath,
		OrderPassword:  values["ORDER_PASSWORD"],
	}, nil
}

// TachibanaCredentialSource は立花証券の認証情報の取得元を説明する（credentials check 用）。
func TachibanaCredentialSource(env settings.Environment, dotenvMap map[string]string) string {
	return CredentialSource(env, TachibanaNamespace, TachibanaCredentialFields, dotenvMap)
}

// PermissionWarnings は秘密を含むファイルのうち、他ユーザーから読めるものの警告。
// .env と秘密鍵を見る。運用者に見せるための診断で、実行は止めない。
func PermissionWarnings(env settings.Environment, dotenvMap map[string]string) []string {
	var warnings []string
	if warning := WarnIfReadableByOthers(EnvFilePath()); warning != "" {
		warnings = append(warnings, warning)
	}
	if keyFile := ResolveCredential(env, TachibanaNamespace, "PRIVATE_KEY_FILE", dotenvMap); keyFile != "" {
		if warning := WarnIfReadableByOthers(expandUser(keyFile)); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

// LoadAPIKey は J-Quants などの API キーを解決する。
//
// 環境変数 → .env の順に var の名前で探す。キーチェーンは使わない
// （口座の認証情報と違い、環境ごとに分ける意味が無い）。
func LoadAPIKey(keyName string, dotenvMap map[string]string) (string, error) {
	if val, ok := os.LookupEnv(keyName); ok && val != "" {
		logging.RegisterSecret(val)
		return val, nil
	}
	if val, ok := dotenvMap[keyName]; ok && val != "" {
		logging.RegisterSecret(val)
		return val, nil
	}
	return "", fmt.Errorf("APIキーが見つかりません: %s（環境変数か %s に設定してください）", keyName, EnvFilePath())
}

// expandUser は先頭の ~ をホームディレクトリに展開する。
func expandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
}
