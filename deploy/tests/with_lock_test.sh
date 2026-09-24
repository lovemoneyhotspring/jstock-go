#!/usr/bin/env bash
# deploy/with-lock.sh の終了コードとログの試験。打ち切り（TERM・KILL）と、上限前の SIGKILL（OOM の代わりに
# 自分で kill -9）を取り違えないこと。一時ディレクトリの中だけで動かし、通知はスタブの discord-post に書く。
#
#   deploy/tests/with_lock_test.sh
set -uo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
fail=0
ok()  { echo "ok   $*"; }
ng()  { echo "FAIL $*"; fail=1; }
check() { if eval "$2"; then ok "$1"; else ng "$1"; fi; }

W="$REPO/deploy/with-lock.sh"
L="$T/run.log"
P="$T/discord-post"
printf '#!/bin/sh\ncat >> "%s/posted"; echo "$@" >> "%s/posted"\n' "$T" "$T" > "$P"; chmod +x "$P"
run() { rm -f "$L" "$T/posted"; POST_BIN="$P" WITH_LOCK_NOTIFY=1 "$W" "$T/lock" 0 "$L" "$@" 2>/dev/null; rc=$?; }

WITH_LOCK_TIMEOUT=1 run sleep 5
check "上限で TERM → 124・[timeout]・通知" \
  '[ "$rc" = 124 ] && grep -q "\[error\] \[timeout\] 1 秒で" "$L" && grep -q "\[timeout\]" "$T/posted"'

WITH_LOCK_TIMEOUT=1 WITH_LOCK_KILL_AFTER=1 run sh -c 'trap "" TERM; sleep 5'
check "TERM で終わらず KILL（137）でも上限まで走っていれば 124・[timeout]" \
  '[ "$rc" = 124 ] && grep -q "\[error\] \[timeout\]" "$L" && ! grep -q "\[killed\]" "$L"'

WITH_LOCK_TIMEOUT=30 run sh -c 'kill -9 $$'
check "上限の前の SIGKILL（OOM の代わり）は 137・[killed]・通知で、[timeout] と書かない" \
  '[ "$rc" = 137 ] && grep -q "\[error\] \[killed\]" "$L" && ! grep -q "\[timeout\]" "$L" && grep -q "\[killed\]" "$T/posted"'

WITH_LOCK_TIMEOUT=0 run sh -c 'kill -9 $$'
check "上限なしでも SIGKILL は 137・[killed]" '[ "$rc" = 137 ] && grep -q "\[error\] \[killed\]" "$L"'

run sh -c 'exit 3'
check "cmd の失敗はそのまま返し、何も書き足さない" '[ "$rc" = 3 ] && ! grep -q "\[error\]" "$L" && [ ! -e "$T/posted" ]'

run sh -c 'exit 251'
check "cmd 自身の 251 は 250 に写して見送りと混ぜない" '[ "$rc" = 250 ] && ! grep -q lock_busy "$L"'

exec 9>"$T/lock"; flock 9
run true
exec 9>&-
check "ロックを取れなければ 75・[lock_busy]" '[ "$rc" = 75 ] && grep -q "\[warn\] \[lock_busy\]" "$L"'

run true
check "ふつうに終われば 0" '[ "$rc" = 0 ]'

exit "$fail"
