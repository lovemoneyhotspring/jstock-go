#!/usr/bin/env bash
# deploy/wait-until.sh（見送りをログに残す）・deploy/rollback-bin.sh（半端に戻さない）と、
# deploy/morning-check.sh が wait-until の見送りを拾うことの試験。一時ディレクトリの中だけで動かし、
# 本物の bin・ログ・Discord には触れない（morning-check は NO_POST=1 とスタブの discord-post）。
#
#   deploy/tests/wait_rollback_test.sh
set -uo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
T="$(mktemp -d)"
trap 'chmod -R u+w "$T" 2>/dev/null; rm -rf "$T"' EXIT
fail=0
ok()  { echo "ok   $*"; }
ng()  { echo "FAIL $*"; fail=1; }
check() { if eval "$2"; then ok "$1"; else ng "$1"; fi; }

# --- wait-until ------------------------------------------------------------------
W="$REPO/deploy/wait-until.sh"
L="$T/open.log"
day=$(TZ=Asia/Tokyo date +%F)

rm -f "$L"; WAIT_UNTIL_LOG="$L" "$W" 00:00:00 2>/dev/null; rc=$?
check "過ぎた時刻はすぐ抜ける（exit 0、ログには何も書かない）" '[ "$rc" = 0 ] && [ ! -e "$L" ]'

rm -f "$L"; soon=$(TZ=Asia/Tokyo date -d '+1 second' +%H:%M:%S.%3N)
WAIT_UNTIL_LOG="$L" "$W" "$soon" 2>/dev/null; rc=$?
check "120 秒以内の時刻は待って exit 0（ログには何も書かない）" '[ "$rc" = 0 ] && [ ! -e "$L" ]'

# crontab と同じ形（変数が空 → 引数が消える）。後ろの && の回が走らないことも見る
rm -f "$L" "$T/ran"
( cd "$T" && DT_OPEN_AT="" && sh -c "WAIT_UNTIL_LOG='$L' '$W' \$DT_OPEN_AT && touch '$T/ran'" 2>/dev/null ); rc=$?
check "時刻が空なら exit 1 で後ろの回を起こさず、[error] [wait_skip] を 1 行残す" \
  '[ "$rc" = 1 ] && [ ! -e "$T/ran" ] && [ "$(wc -l < "$L")" = 1 ] && grep -q "^$day [0-9:]* \[error\] \[wait_skip\] 時刻が空" "$L"'

rm -f "$L"; WAIT_UNTIL_LOG="$L" "$W" 25:99:00 2>/dev/null; rc=$?
check "読めない時刻は exit 1 で、理由をログに残す" '[ "$rc" = 1 ] && grep -q "\[error\] \[wait_skip\] 時刻 .25:99:00. を読めない" "$L"'

# 23:57 より後は「今日の 23:59:59」が 120 秒以内になるので飛ばす
if [ "$(TZ=Asia/Tokyo date +%H%M)" -lt 2357 ]; then
  rm -f "$L"; WAIT_UNTIL_LOG="$L" "$W" 23:59:59 2>"$T/err"; rc=$?
  check "120 秒より先は待たずに exit 1 で、理由をログと stderr に残す" \
    '[ "$rc" = 1 ] && grep -q "\[error\] \[wait_skip\] 23:59:59 は .*120 秒より先は待たない" "$L" && grep -q "120 秒より先" "$T/err"'
else
  echo "skip 120 秒より先の試験（23:57 以降は今日の中に 120 秒より先の時刻を作れない）"
fi

rm -f "$L"; ( cd "$T" && env -u WAIT_UNTIL_LOG "$W" "" 2>/dev/null ); rc=$?
check "WAIT_UNTIL_LOG が無ければ従来どおり（exit 1、ファイルは作らない）" '[ "$rc" = 1 ] && [ ! -e "$L" ]'

WAIT_UNTIL_LOG="$T/no-such-dir/open.log" "$W" "" 2>/dev/null; rc=$?
check "ログに書けなくても見送りは変わらない（exit 1）" '[ "$rc" = 1 ] && [ ! -e "$T/no-such-dir" ]'

# --- rollback-bin ----------------------------------------------------------------
R="$REPO/deploy/rollback-bin.sh"
cmds=(wbjp accum daytrade jquants discord-post rate news)
new_bin() {
  B="$T/bin$1"; rm -rf "$B"; mkdir -p "$B/.prev"
  for c in "${cmds[@]}"; do echo "new-$c" > "$B/$c"; echo "old-$c" > "$B/.prev/$c"; chmod +x "$B/$c" "$B/.prev/$c"; done
  export BIN_DIR="$B"
}
# all_are <dir> <new|old> [除くコマンド]: dir の下の全コマンドがその世代か
all_are() {
  local c
  for c in "${cmds[@]}"; do
    [ "$c" = "${3:-}" ] && continue
    [ "$(cat "$1/$c" 2>/dev/null)" = "$2-$c" ] || return 1
  done
}
no_litter() { [ -z "$(find "$B" -name '.*.rb' -o -name '.*.keep')" ]; }

new_bin 1; "$R" >/dev/null 2>&1; rc=$?
check "一式そろっていれば bin と .prev を入れ替える" '[ "$rc" = 0 ] && all_are "$B" old && all_are "$B/.prev" new && no_litter && [ -x "$B/daytrade" ]'
"$R" >/dev/null 2>&1; rc=$?
check "もう一度実行すれば進む（元に戻る）" '[ "$rc" = 0 ] && all_are "$B" new && all_are "$B/.prev" old && no_litter'

new_bin 2; rm "$B/.prev/rate"; "$R" >/dev/null 2>&1; rc=$?
check ".prev が 1 つ欠けていれば何も動かさない" '[ "$rc" = 1 ] && all_are "$B" new && all_are "$B/.prev" old rate && no_litter'

# 以前はここで打ち切られ、wbjp・accum・daytrade だけ戻った bin と .rb の残骸が残った
new_bin 3; rm "$B/jquants"; out=$("$R" 2>&1); rc=$?
check "bin が 1 つ欠けていても途中で止まらず、全部を 1 世代前にそろえる" '[ "$rc" = 0 ] && all_are "$B" old && no_litter'
check "欠けていたコマンドは戻すだけ（.prev はそのまま）で、そう報告する" \
  '[ "$(cat "$B/.prev/jquants")" = old-jquants ] && all_are "$B/.prev" new jquants && grep -q "jquants は bin/ に無かったので戻しただけ" <<<"$out"'

new_bin 4; rm "$B/rate"; mkdir "$B/rate"; "$R" >/dev/null 2>&1; rc=$?
check "bin/<cmd> が通常のファイルでなければ何も動かさない" '[ "$rc" = 1 ] && all_are "$B" new rate && all_are "$B/.prev" old && no_litter'

# 下ごしらえ（ln）が失敗する形: .prev に書けない。root は書けてしまうので飛ばす
if [ "$(id -u)" != 0 ]; then
  new_bin 5; chmod 555 "$B/.prev"; "$R" >/dev/null 2>&1; rc=$?; chmod 755 "$B/.prev"
  check "下ごしらえの途中で失敗しても bin は 1 つも動かず、残骸も残さない" '[ "$rc" != 0 ] && all_are "$B" new && all_are "$B/.prev" old && no_litter'
else
  echo "skip 下ごしらえの失敗の試験（root は読み取り専用のディレクトリにも書ける）"
fi
unset BIN_DIR

# --- morning-check: wait-until の見送りを拾う --------------------------------------------
# 他の回は動いている朝（起動 2 回）。見送りの 1 行だけが手がかりになる形
H="$T/mc"; mkdir -p "$H/deploy" "$H/bin" "$H/state/logs"
cp "$REPO"/deploy/{morning-check.sh,open-pipeline.jq} "$H/deploy/"
printf '#!/bin/sh\nexit 0\n' > "$H/bin/jquants"
printf '#!/bin/sh\ncat >> "$(dirname "$0")/../posted"\n' > "$H/bin/discord-post"
chmod +x "$H/bin/jquants" "$H/bin/discord-post"
ts=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
for run in r1 r2; do
  printf '{"schema":1,"ts_utc":"%s","run_id":"%s","app":"daytrade","env":"prod","command":"open","level":"info","code":"daytrade.config","msg":"設定","extra":{"live":true}}\n' "$ts" "$run"
  printf '{"schema":1,"ts_utc":"%s","run_id":"%s","app":"daytrade","env":"prod","command":"open","level":"info","code":"daytrade.run","msg":"完了","extra":{}}\n' "$ts" "$run"
done > "$H/state/logs/daytrade-prod.jsonl"
mc() { WBJP_HOME="$H" WBJP_ENV=prod NO_POST=1 POST_BIN="$H/bin/discord-post" QUERY_BIN="$H/bin/jquants" "$H/deploy/morning-check.sh" 2>&1; }

echo "$day 09:00:02 [info] 何もなし" > "$H/state/logs/daytrade-open.log"
out=$(mc)
check "見送りが無い朝は 0 件で、❌ を出さない" 'grep -q "起動 2 回 / 完了 2 回 .*エラー 0 件 .*時刻待ちの見送り 0 件" <<<"$out" && ! grep -q "wait-until.sh が open の起動を" <<<"$out"'

( cd "$H" && WAIT_UNTIL_LOG=state/logs/daytrade-open.log "$W" "" 2>/dev/null )
out=$(mc)
check "見送りの行が今日あれば ❌ で名指しする（エラーの件数とは分ける）" \
  'grep -q "エラー 0 件 .*時刻待ちの見送り 1 件" <<<"$out" && grep -q "❌ wait-until.sh が open の起動を 1 回見送りました" <<<"$out" && grep -q "\[wait_skip\] 時刻が空" <<<"$out"'

echo "2000-01-01 08:59:01 [error] [wait_skip] 昔の行" > "$H/state/logs/daytrade-open.log"
out=$(mc)
check "昨日までの見送りは数えない" 'grep -q "時刻待ちの見送り 0 件" <<<"$out"'
check "NO_POST=1 では Discord に投げない" '[ ! -e "$H/posted" ]'

echo
[ "$fail" = 0 ] && echo "全部通った" || echo "失敗あり"
exit "$fail"
