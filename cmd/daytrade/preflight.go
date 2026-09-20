package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/spf13/cobra"
)

// preflightMinFreeBytes は state のあるファイルシステムに要る空き。台帳（SQLite の WAL）・ログ・
// 履歴が書けないと、発注はできても記録が残らず、次の回が二重に建てる。
const preflightMinFreeBytes = 1 << 30

// newPreflightCmd は寄る前の事前点検。**読むだけ**でブローカーには繋がない（ロックも要らない）。
//
// 8:59:45 の open が落ちる理由のうち、前もって分かるものを 8:40 に確かめる: この bin が設定を
// 読めるか（項目を足して build.sh を忘れると strict mode で全コマンドが止まる）、今日の plan が
// 読めるか、台帳を開けるか、ディスクに空きがあるか。問題があれば通知して異常終了する。
// 20 分あれば人が直せる（plan は `daytrade plan`、bin は `deploy/build.sh`）。
func newPreflightCmd() *cobra.Command {
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "寄る前の事前点検（設定・plan・台帳・ディスク）。読むだけで発注もブローカーへの接続もしない",
		RunE: func(cmd *cobra.Command, args []string) error {
			day, err := dayOrToday(dateFlag, clock.NowUTC())
			if err != nil {
				return err
			}
			if skipHolidayFor(day, "preflight", false) {
				return nil
			}
			problems := preflightProblems(day.Format(DateLayout), func() error {
				_, err := loadConfig()
				return err
			}, func() error {
				p, ok, err := dtplan.Load(appSettings.DaytradeDir(), day)
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("plan がありません（`daytrade plan` で作る）")
				}
				if p.Meta.Eligible == 0 {
					return errors.New("plan の候補が 0 銘柄です")
				}
				return nil
			}, func() error {
				led, err := dtledger.Open(appSettings.DaytradeDBPath())
				if err != nil {
					return err
				}
				return led.Close()
			}, func() error {
				return checkFreeSpace(appSettings.StateDir, preflightMinFreeBytes)
			})
			if len(problems) == 0 {
				fmt.Printf("%s 寄る前の点検: 問題なし\n", day.Format(DateLayout))
				logInfo("daytrade.preflight", "寄る前の点検: 問題なし", map[string]any{"day": day.Format(DateLayout)})
				return nil
			}
			body := strings.Join(problems, "\n")
			fmt.Printf("%s 寄る前の点検: %d 件の問題\n%s\n", day.Format(DateLayout), len(problems), body)
			logError("daytrade.preflight", "寄る前の点検で問題", map[string]any{"day": day.Format(DateLayout), "problems": problems})
			digest.Anomaly("daytrade.preflight", fmt.Sprintf("寄る前の点検で %d 件の問題", len(problems)))
			alert(fmt.Sprintf("デイトレ: 寄る前の点検で %d 件の問題（8:59:45 の open までに直す）", len(problems)), body)
			return fmt.Errorf("寄る前の点検で %d 件の問題", len(problems))
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

// preflightProblems は点検を順に回し、問題を人向けの行にして返す。1 つ落ちても残りは続ける
// （設定が読めなくても plan・ディスクは別に確かめられる）。
func preflightProblems(day string, config, plan, ledger, disk func() error) []string {
	var problems []string
	for _, check := range []struct {
		name string
		run  func() error
	}{
		{"設定を読めません（項目を足したなら deploy/build.sh）", config},
		{day + " の plan", plan},
		{"台帳を開けません", ledger},
		{"ディスク", disk},
	} {
		if err := check.run(); err != nil {
			problems = append(problems, fmt.Sprintf("- %s: %v", check.name, err))
		}
	}
	return problems
}

// checkFreeSpace は dir のあるファイルシステムの空きが need バイト以上あるか。
func checkFreeSpace(dir string, need uint64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("%s の空きを調べられません: %w", filepath.Clean(dir), err)
	}
	free := st.Bavail * uint64(st.Bsize)
	if free < need {
		return fmt.Errorf("%s の空きが %d MB しかありません（%d MB 以上要る）", filepath.Clean(dir), free>>20, need>>20)
	}
	// 書けることも確かめる（読み取り専用で再マウントされた・権限が変わった）
	probe, err := os.CreateTemp(dir, ".preflight-*")
	if err != nil {
		return fmt.Errorf("%s に書けません: %w", filepath.Clean(dir), err)
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}
