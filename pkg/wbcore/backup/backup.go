// Package backup は state/ のバックアップ。
//
// state/ にはそのホストで起きたことの唯一の記録（積立の発注台帳 accum-<env>.db、
// スイング売買の記録 wbjp-<env>.db）があり、ブローカーからは再構築できない。
// 失うと当月を買い直す・履歴を失う。
//
// 方針:
//   - state_dir 直下の *.db をすべて、SQLite の一貫したスナップショットとして複製する
//     （cron が書いている最中でも壊れない。単純なファイルコピーだと書き込み途中を写しうる）
//   - 複製先は <dest>/<元の名前から .db を除いたもの>-YYYYMMDD.db。1 日 1 世代、古い世代から削る
//   - logs/ は対象外。ログは日次ローテーション＋90 日保持で自衛しており、失っても売買は壊れない
//   - 複製先の既定は state/backup/。同じディスクなので、ホストごと失う障害には
//     別ホストへの rsync を併用する（DEPLOY.md 参照）
package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

// Result は 1 回のバックアップの結果。
type Result struct {
	// Copied は複製したファイルのパス。
	Copied []string
	// Removed は世代削減で消したファイルのパス。
	Removed []string
}

// BackupSQLite は SQLite を一貫したスナップショットとして複製する。
//
// Python 版は sqlite3 のオンラインバックアップ API を使っていた。Go の
// database/sql には相当する API が無いが、SQLite の VACUUM INTO が同じことを
// 1 文で行う（読み取りトランザクションの中で複製するので、書き込み中でも
// 中途半端な状態を写さない）。
func BackupSQLite(source, destination string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", fmt.Errorf("バックアップ先を作れません %s: %w", destination, err)
	}
	// VACUUM INTO は複製先が既にあると失敗する。同じ日に 2 回走ることは
	// あるので（cron の再試行、手動実行）、先に消して取り直す。
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("古いバックアップを消せません %s: %w", destination, err)
	}

	db, err := storage.OpenSQLite(source)
	if err != nil {
		return "", fmt.Errorf("バックアップ元を開けません %s: %w", source, err)
	}
	defer db.Close()

	// パスは SQL 文字列リテラルとして渡すので、シングルクォートだけ潰す
	quoted := strings.ReplaceAll(destination, "'", "''")
	if _, err := db.Exec("VACUUM INTO '" + quoted + "'"); err != nil {
		return "", fmt.Errorf("%s のバックアップに失敗しました: %w", source, err)
	}
	return destination, nil
}

// Options は BackupState の任意指定。
type Options struct {
	// Dest は複製先。空なら settings.BackupDir()。
	Dest string
	// Keep は元ファイルごとに残す世代数。0 以下なら削らない。
	Keep int
	// Today は世代名に使う日付。ゼロ値なら UTC の今日。
	Today time.Time
}

// DefaultKeep は既定の保持世代数（約 1 か月ぶん）。
const DefaultKeep = 30

// BackupState は state/ の全 SQLite を世代付きで複製する。
func BackupState(s *settings.AppSettings, opts Options) (Result, error) {
	directory := opts.Dest
	if directory == "" {
		directory = s.BackupDir()
	}
	day := opts.Today
	if day.IsZero() {
		day = clock.TodayUTC()
	}
	stamp := day.Format("20060102")

	result := Result{Copied: []string{}, Removed: []string{}}

	entries, err := os.ReadDir(s.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("state ディレクトリを読めません %s: %w", s.StateDir, err)
	}

	var sources []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".db") {
			sources = append(sources, e.Name())
		}
	}
	sort.Strings(sources)

	for _, name := range sources {
		stem := strings.TrimSuffix(name, ".db")
		target, err := BackupSQLite(filepath.Join(s.StateDir, name), filepath.Join(directory, stem+"-"+stamp+".db"))
		if err != nil {
			return result, err
		}
		result.Copied = append(result.Copied, target)

		if opts.Keep > 0 {
			old, err := generationsToRemove(directory, stem, opts.Keep)
			if err != nil {
				return result, err
			}
			for _, path := range old {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return result, fmt.Errorf("古い世代を消せません %s: %w", path, err)
				}
				result.Removed = append(result.Removed, path)
			}
		}
	}
	return result, nil
}

// generationsToRemove は保持数を超えた古い世代を返す。
// 名前が <stem>-YYYYMMDD.db なので、名前順＝古い順になる。
func generationsToRemove(directory, stem string, keep int) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(directory, stem+"-*.db"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	if len(matches) <= keep {
		return nil, nil
	}
	return matches[:len(matches)-keep], nil
}
