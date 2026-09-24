#!/usr/bin/env bash
# systemd のユニット（jstock-guard・jstock-close-net）の ExecStopPost から呼ぶ後始末。
#
#   deploy/unit-done.sh <KEY> <題> [止められたときに人がやること]
#   例: deploy/unit-done.sh GUARD "寄る前の自動復旧" "8:59 の open までに preflight を手で確かめる"
#
# 1. 終わり方を deploy/ping.sh <KEY> <終了コード> で state/ping/<KEY> に残す。
#    deploy/mackerel-alive.sh がそれを alive.<key> として Mackerel に投稿する（cron の MORNING・VERIFY と同じ仕組み）
# 2. スクリプト自身が通知できない終わり方（時間切れで止められた・シグナル・コアダンプ）のときだけ、
#    Discord に「途中で止められた」を 1 通送る。ふつうの失敗（exit-code）はスクリプトが自分で通知済み。
#
# ExecStopPost はメインのプロセスが時間切れ（TimeoutStartSec）で SIGTERM・SIGKILL されたあとも走る。
# スクリプトの中で TERM を trap する形は、SIGKILL（-k・OOM）では何も出せないのでこちらにした。
# systemd が渡す環境変数（systemd.exec(5)）:
#   SERVICE_RESULT … success / exit-code / signal / timeout / core-dump / watchdog / oom-kill / resources など
#   EXIT_CODE      … exited / killed / dumped
#   EXIT_STATUS    … exited なら終了コード（数字）、killed・dumped ならシグナル名（TERM・KILL など）
# 手で呼ぶときはこの 3 つを自分で渡す（試験は deploy/tests/guards_test.sh）。
set -uo pipefail

key=${1:-}
label=${2:-$key}
hint=${3:-}
if [ -z "$key" ]; then
  echo "usage: unit-done.sh <KEY> [題] [人がやること]" >&2
  exit 64
fi
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/lib-notify.sh"

result=${SERVICE_RESULT:-}
status=${EXIT_STATUS:-}
# ping に渡す終了コード。success は 0。exited なら終了コードそのまま。シグナルで終わったときは
# シグナル名を渡す（ping.sh は数字でないものを URL に付けないが、state/ping には残り、
# mackerel-alive.sh は 0 以外として 1 を投稿する）
if [ "$result" = "success" ]; then
  rc=0
elif [ "${EXIT_CODE:-}" = "exited" ] && [ -n "$status" ] && [ "$status" != "0" ]; then
  rc=$status
else
  rc=${status:-1}
  [ "$rc" = "0" ] && rc=1
  # 空白を含むと ping.sh の 1 行（日付 時刻 終了コード）が崩れるので詰める
  rc=${rc// /_}
fi

"$HOME_DIR/deploy/ping.sh" "$key" "$rc" >> "$HOME_DIR/state/logs/ping.log" 2>&1

case "$result" in
  success|exit-code|'')
    # 成功、またはスクリプトが自分で終えた失敗（通知はスクリプトが済ませている）
    log "[unit-done] $key: result=${result:-?} rc=$rc"
    ;;
  *)
    log "[unit-done] $key: 途中で止められた（result=$result, exit=${EXIT_CODE:-?}/${status:-?}）"
    notify "$label が途中で止められました（$result）" "systemd がユニットを止めました（result=$result、${EXIT_CODE:-?} ${status:-?}）。timeout なら TimeoutStartSec を超えています。スクリプトは最後の通知を出せていません。$GUARD_LOG と journalctl --user -u 'jstock-*' を確認してください。${hint}"
    ;;
esac
exit 0
