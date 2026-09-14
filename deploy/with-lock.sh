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
#
# 見送りの判定は flock -E の番兵 251 で行う。以前は 75 を番兵にしていたが、それだと
# cmd 自身の exit 75 と区別できず、走って失敗した回が [lock_busy] として記録され、
# deploy/morning-check.sh の「ロック見送り」の数を狂わせた。cmd が万一 251 を返しても
# 250 に写し替えて番兵と混ざらないようにする。
set -eu

lock=${1:-}
wait=${2:-0}
log=${3:-}
[ $# -ge 3 ] && shift 3
if [ -z "$lock" ] || [ -z "$log" ] || [ $# -eq 0 ]; then
  echo "usage: with-lock.sh <lock> <wait_sec> <log> <cmd...>" >&2
  exit 64
fi
if ! command -v flock >/dev/null 2>&1; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') [error] flock が見つかりません（util-linux）。実行を見送り: $*" >> "$log"
  exit 69
fi

busy=251
# 子は sh -c で包み、cmd 自身の 251 だけ 250 に写す（"$@" は sh -c の位置引数）
if flock -w "$wait" -E "$busy" "$lock" \
    sh -c '"$@"; rc=$?; [ "$rc" -eq 251 ] && exit 250; exit "$rc"' with-lock "$@" >> "$log" 2>&1; then
  rc=0
else
  rc=$?
fi
if [ "$rc" -eq "$busy" ]; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') [warn] [lock_busy] ロック $lock を ${wait} 秒待っても取れず見送り: $*" >> "$log"
  exit 75
fi
exit "$rc"
