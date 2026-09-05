#!/bin/sh
# cron の 1 行を flock で排他して回し、**ロックを取れずに見送った事実をログに残す**。
#
#   deploy/with-lock.sh <lock> <wait_sec> <log> <cmd...>
#
# flock -n は取れないとき無言で exit 1 する。発注経路（daytrade open / close）では
# 「前の実行が長引いて 9:04 の回が消えた」と「9:04 の回が走って失敗した」を後から
# 区別できないと困るので、見送りは log に 1 行書く。wait_sec を 0 より大きくすると
# その秒数までロックを待つ（snap が握っていても open が少し待てば取れる）。
#
# 終了コード: cmd のもの。ロックを取れなかったときは 75（EX_TEMPFAIL）。
lock=$1
wait=$2
log=$3
shift 3
if [ -z "$lock" ] || [ -z "$log" ] || [ $# -eq 0 ]; then
  echo "usage: with-lock.sh <lock> <wait_sec> <log> <cmd...>" >&2
  exit 64
fi
flock -w "${wait:-0}" -E 75 "$lock" "$@" >> "$log" 2>&1
rc=$?
if [ "$rc" -eq 75 ]; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') [warn] [lock_busy] ロック $lock を ${wait:-0} 秒待っても取れず見送り: $*" >> "$log"
fi
exit $rc
