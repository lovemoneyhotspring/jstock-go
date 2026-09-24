#!/usr/bin/env bash
# deploy/close-net.sh・deploy/guard-preopen.sh・deploy/unit-done.sh（ユニットの ExecStopPost）の試験。スタブの bin と crontab を使う隔離環境で動かし、
# 本物の bin・crontab・Discord には触れない。
#
#   deploy/tests/guards_test.sh
# check は式を eval するので、式の中の変数は単一引用符のまま・代入は未使用に見える
# shellcheck disable=SC2016,SC2034
set -uo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
fail=0
ok()  { echo "ok   $*"; }
ng()  { echo "FAIL $*"; fail=1; }
check() { if eval "$2"; then ok "$1"; else ng "$1"; fi; }

new_home() {
  H="$T/home$1"; rm -rf "$H"
  mkdir -p "$H/deploy" "$H/bin" "$H/state/logs" "$H/state/digest" "$H/fakebin"
  cp "$REPO"/deploy/{with-lock.sh,lib-notify.sh,close-net.sh,guard-preopen.sh,unit-done.sh,ping.sh,mackerel-alive.sh} "$H/deploy/"
  # discord-post: 標準入力を posted に残す
  cat > "$H/bin/discord-post" <<'EOS'
#!/bin/sh
echo "TITLE=$2 BODY=$(cat)" >> "$(dirname "$0")/../posted"
EOS
  chmod +x "$H/bin/discord-post"
  # 偽の crontab
  cat > "$H/fakebin/crontab" <<'EOS'
#!/bin/sh
F="$(dirname "$0")/../crontab.store"
case "$1" in
  -l) [ -f "$F" ] && cat "$F" || exit 1 ;;
  -n) exit 0 ;;
  *) cp "$1" "$F" ;;
esac
EOS
  chmod +x "$H/fakebin/crontab"
  export WBJP_HOME="$H" CRONTAB="$H/fakebin/crontab" POST_BIN="$H/bin/discord-post" WBJP_ENV=prod
}
day=$(TZ=Asia/Tokyo date +%F)
# 作り直しは「コミット済みの main」でだけ行うので、仮ホームを git リポジトリにする
git_home() {
  echo tracked > "$H/tracked.txt"
  ( cd "$H" && git init -q -b main && git -c user.name=t -c user.email=t@t add -A \
      && git -c user.name=t -c user.email=t@t commit -q -m init )
}

# --- close-net -----------------------------------------------------------------
row() { printf '{"app":"daytrade","command":"close","outcome":"%s","live":%s,"ts_utc":"%sT%s+00:00"}\n' "$1" "$2" "$day" "$3"; }
new_home 1
cat > "$H/bin/daytrade" <<EOS
#!/bin/sh
echo run >> "$H/daytrade.calls"
echo '{"app":"daytrade","command":"close","outcome":"ok","live":true,"ts_utc":"${day}T06:22:00+00:00"}' >> "$H/state/digest/prod-$day.jsonl"
EOS
chmod +x "$H/bin/daytrade"
# 1. cron の close が成功済み（本番・15:20 以降）→ 何もしない
row ok true 06:20:30 > "$H/state/digest/prod-$day.jsonl"
bash "$H/deploy/close-net.sh"
check "close 済みなら close を走らせない" '[ ! -f "$H/daytrade.calls" ]'
# 2. 成功記録なし（失敗・15:20 より前）→ 走らせて通知
{ row error true 06:20:30; row ok true 05:00:00; } > "$H/state/digest/prod-$day.jsonl"
bash "$H/deploy/close-net.sh"
check "成功記録が無ければ close を走らせる" '[ "$(wc -l < "$H/daytrade.calls")" = 1 ]'
check "走らせたら通知する" 'grep -q "安全網が close を実行" "$H/posted"'
# 2b. dry-run の close の成功は数えない（本番の close が走っていない日に沈黙しない）
new_home 12
cp "$T/home1/bin/daytrade" "$H/bin/daytrade"; sed -i "s#$T/home1#$H#g" "$H/bin/daytrade"
row ok false 06:21:00 > "$H/state/digest/prod-$day.jsonl"
bash "$H/deploy/close-net.sh"
check "dry-run の close は「済み」と数えない" '[ -f "$H/daytrade.calls" ]'
# 3. 失敗 → 失敗を通知し、終了コードを返す
new_home 2
printf '#!/bin/sh\nexit 3\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
bash "$H/deploy/close-net.sh"; rc=$?
check "close が失敗したら rc を返す" '[ "$rc" = 3 ]'
check "失敗を通知する" 'grep -q "安全網の close も正常に終わりませんでした" "$H/posted"'
# 4. 休場日など（close は 0 で終わるが ok の記録を残さない）→ 通知しない
new_home 3
printf '#!/bin/sh\nexit 0\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
bash "$H/deploy/close-net.sh"
check "何もせず終わった日は通知しない" '[ ! -f "$H/posted" ]'
# 4b. cron の close が実行中（ロックを取れない）→ 失敗と誤報せず何もしない
new_home 13
printf '#!/bin/sh\necho ran >> "$(dirname "$0")/../daytrade.calls"\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
( flock "$H/lock" sleep 8 ) &
sleep 0.5
CLOSE_NET_LOCK="$H/lock" CLOSE_NET_LOCK_WAIT=0 bash "$H/deploy/close-net.sh"; rc=$?
wait
check "ロックで見送ったら誤報せず rc=0" '[ "$rc" = 0 ] && [ ! -f "$H/posted" ] && [ ! -f "$H/daytrade.calls" ]'

# --- guard-preopen: crontab ---------------------------------------------------------
new_home 4
printf '#!/bin/sh\necho "2026 寄る前の点検: 問題なし"\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
GOOD='30 20 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade plan
59 8 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade open
20,24,28 15 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade close
40 15 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade verify'
echo "$GOOD" > "$H/state/crontab.good"
# 数行の編集では戻さない
echo "$GOOD" | head -3 > "$H/crontab.store"
bash "$H/deploy/guard-preopen.sh"
check "数行の増減では crontab を戻さない" '[ "$(wc -l < "$H/crontab.store")" = 3 ] && [ ! -f "$H/posted" ]'
# 消えたら戻す
echo "# 空" > "$H/crontab.store"
bash "$H/deploy/guard-preopen.sh"
check "crontab が消えたら控えから戻す" 'diff -q "$H/crontab.store" "$H/state/crontab.good" >/dev/null'
check "戻したら通知する" 'grep -q "控えから戻しました" "$H/posted"'
rm -f "$H/crontab.store"
bash "$H/deploy/guard-preopen.sh"
check "crontab が無い（-l が失敗）ときも戻す" 'diff -q "$H/crontab.store" "$H/state/crontab.good" >/dev/null'

# --- guard-preopen: 寄る前の点検（GUARD_HOUR=8 で 8 時台の分岐を動かす）--------------------------
export GUARD_HOUR=8
stub_common() {
  printf '#!/bin/sh\necho built >> "$(dirname "$0")/../build.calls"\n%s\n' "$1" > "$H/deploy/build.sh"; chmod +x "$H/deploy/build.sh"
  printf '#!/bin/sh\necho rolled >> "$(dirname "$0")/../rb.calls"\n%s\n' "$2" > "$H/deploy/rollback-bin.sh"; chmod +x "$H/deploy/rollback-bin.sh"
}
# 5. 設定を読めない → build で直る
new_home 5
printf '#!/bin/sh\necho "- 設定を読めません"\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common 'printf "#!/bin/sh\\necho ok\\n" > "$(dirname "$0")/../bin/daytrade"' ''
git_home
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "設定を読めなければ作り直して復旧する" '[ -f "$H/build.calls" ] && grep -q "復旧しました" "$H/posted" && [ ! -f "$H/rb.calls" ] && [ "$rc" = 0 ]'
# 6. 作り直しても直らない → 1 世代前へ戻す → それで直る
new_home 6
printf '#!/bin/sh\necho "strict mode: fields missing"\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' 'printf "#!/bin/sh\\necho ok\\n" > "$(dirname "$0")/../bin/daytrade"'
git_home
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "作り直しでも直らなければ 1 世代前へ戻す" '[ -f "$H/build.calls" ] && [ -f "$H/rb.calls" ] && grep -q "1 世代前の実行ファイルへ戻しました" "$H/posted" && [ "$rc" = 0 ]'
# 7. どうしても直らない → 失敗を通知して rc=1
new_home 7
printf '#!/bin/sh\necho "strict mode: fields missing"\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' ''
git_home
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "直せなければ失敗を通知して rc=1" '[ "$rc" = 1 ] && grep -q "自動復旧できません" "$H/posted"'
# 8. plan が無い → plan --if-missing で直る（1 回目は失敗、plan を作ると通る）
new_home 8
cat > "$H/bin/daytrade" <<'EOS'
#!/bin/sh
D="$(dirname "$0")/.."
case "$1" in
  plan) echo planned >> "$D/plan.calls"; touch "$D/plan.ok" ;;
  preflight) if [ -f "$D/plan.ok" ]; then echo "問題なし"; else echo "- plan がありません"; echo "preflight-problems: plan"; exit 1; fi ;;
esac
EOS
chmod +x "$H/bin/daytrade"; stub_common '' ''
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "plan が無ければ作って復旧する" '[ -f "$H/plan.calls" ] && [ ! -f "$H/build.calls" ] && grep -q "plan を作りました" "$H/posted" && [ "$rc" = 0 ]'
# 9. 台帳・ディスクは自動では直せない → 通知だけ
new_home 9
printf '#!/bin/sh\necho "- ディスク: 空きが少ない"\necho "preflight-problems: disk"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' ''
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "ディスクは作り直さず通知だけ" '[ "$rc" = 1 ] && [ ! -f "$H/build.calls" ] && grep -q "空きが少ない" "$H/posted"'
# 10. 問題なし → 通知しない
new_home 10
printf '#!/bin/sh\necho 問題なし\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"; stub_common '' ''
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "問題なしなら通知しない" '[ "$rc" = 0 ] && [ ! -f "$H/posted" ]'
# 11. 9 時以降は寄る前の点検を飛ばす
new_home 11
GUARD_HOUR=10 bash "$H/deploy/guard-preopen.sh"
printf '#!/bin/sh\necho x >> "$(dirname "$0")/../calls"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"; stub_common '' ''
GUARD_HOUR=10 bash "$H/deploy/guard-preopen.sh"
check "9 時以降は点検を飛ばす" '[ ! -f "$H/calls" ]'
# 12. 作業ツリーが未コミットなら作り直さず、1 世代前へ戻す側へ進む
new_home 14
printf '#!/bin/sh\necho "strict mode: fields missing"\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' 'printf "#!/bin/sh\\necho ok\\n" > "$(dirname "$0")/../bin/daytrade"'
echo 'package x' > "$H/a.go"
git_home
echo '// dirty' >> "$H/a.go"
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "未コミットなら build.sh も rollback も走らせない（設定が新しいとき、戻すと悪化する）" '[ ! -f "$H/build.calls" ] && [ ! -f "$H/rb.calls" ] && grep -q "自動では何もしません" "$H/posted" && [ "$rc" = 1 ]'
# 12b. 追跡されていない新規 .go ファイルも「汚れ」として数える（git に無いコードから作らない）。戻しもしない
new_home 18
printf '#!/bin/sh\necho "strict mode: fields missing"\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' ''
git_home
echo 'package x' > "$H/new.go"
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "未追跡の .go があれば作り直しも戻しもしない" '[ ! -f "$H/build.calls" ] && [ ! -f "$H/rb.calls" ] && [ "$rc" = 1 ]'
# 13. 作り直しても実行ファイルが起動しない（コードが出ない）→ 戻す
new_home 15
printf '#!/bin/sh\nexit 127\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' 'printf "#!/bin/sh\\necho ok\\n" > "$(dirname "$0")/../bin/daytrade"'
git_home
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "起動しない実行ファイルも 1 世代前へ戻す" '[ -f "$H/build.calls" ] && [ -f "$H/rb.calls" ] && [ "$rc" = 0 ]'
# 14. crontab: ブロックの目印が残っていて行だけ無効 → 止めたとみなして戻さない（通知はする）
unset GUARD_HOUR; export GUARD_HOUR=10
new_home 16
printf '#!/bin/sh\necho ok\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
printf '# wbjp（/x）の cron\n30 20 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade plan\n59 8 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade open\n20 15 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade close\n40 15 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade verify\n' > "$H/state/crontab.good"
printf '# wbjp（/x）の cron\n#30 20 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade plan\n#59 8 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade open\n' > "$H/crontab.store"
before=$(cat "$H/crontab.store")
bash "$H/deploy/guard-preopen.sh"
check "コメントアウトで止めた crontab は戻さない" '[ "$(cat "$H/crontab.store")" = "$before" ] && grep -q "止めた状態とみなして" "$H/posted"'
# 15. state/crontab.paused があれば何もしない
new_home 17
printf '#!/bin/sh\necho ok\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
printf '30 20 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade plan\n59 8 * * 1-5 cd $WBJP_HOME && $WBJP_BIN/daytrade open\n' > "$H/state/crontab.good"
echo "# 空" > "$H/crontab.store"; touch "$H/state/crontab.paused"
bash "$H/deploy/guard-preopen.sh"
check "crontab.paused があれば戻さない" '[ "$(cat "$H/crontab.store")" = "# 空" ] && [ ! -f "$H/posted" ]'
# --- guard-preopen: 時間の上限（systemd の TimeoutStartSec=600 に殺される前に終える）------------------
export GUARD_HOUR=8
# 16. preflight が固まる → 上限で打ち切り、作り直しも戻しもせず通知して rc=1
new_home 19
printf '#!/bin/sh\nexec sleep 30\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' ''
git_home
t0=$(date +%s)
GUARD_PREFLIGHT_TIMEOUT=6 bash "$H/deploy/guard-preopen.sh"; rc=$?
el=$(( $(date +%s) - t0 ))
check "preflight が固まっても上限で打ち切る（${el} 秒）" '[ "$el" -lt 25 ] && [ "$rc" = 1 ]'
check "時間切れは作り直し・戻しをしない" '[ ! -f "$H/build.calls" ] && [ ! -f "$H/rb.calls" ] && grep -q "終わりませんでした" "$H/posted"'
# 17. 締め切りまでの残りが足りなければ段を飛ばす（作り直しが固まっても全体は GUARD_BUDGET に収まる）
new_home 20
printf '#!/bin/sh\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common 'exec sleep 60' ''
git_home
t0=$(date +%s)
GUARD_BUDGET=25 bash "$H/deploy/guard-preopen.sh"; rc=$?
el=$(( $(date +%s) - t0 ))
check "作り直しが固まっても締め切り内に終える（${el} 秒）" '[ "$el" -lt 40 ] && [ "$rc" = 1 ] && grep -q "作り直しが時間内に終わりませんでした" "$H/posted"'
# 18. 1 世代前へ戻しても設定を読めない → 戻す前の版へ進め直す（rollback-bin.sh をもう一度）
new_home 21
printf '#!/bin/sh\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' ''
git_home
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "戻しても直らなければ戻す前の版へ進め直す" '[ "$(wc -l < "$H/rb.calls")" = 2 ] && grep -q "戻す前の実行ファイルへ進め直しました" "$H/posted" && [ "$rc" = 1 ]'
# 19. 戻して設定は読めたが plan だけ無い → 戻した版のまま plan を作る
new_home 22
printf '#!/bin/sh\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' "$(cat <<'EOS'
cat > "$(dirname "$0")/../bin/daytrade" <<'EOD'
#!/bin/sh
D="$(dirname "$0")/.."
case "$1" in
  plan) touch "$D/plan.ok" ;;
  preflight) [ -f "$D/plan.ok" ] && exit 0; echo "preflight-problems: plan"; exit 1 ;;
esac
EOD
chmod +x "$(dirname "$0")/../bin/daytrade"
EOS
)"
git_home
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "戻した版が設定を読めれば進め直さず plan へ進む" '[ "$(wc -l < "$H/rb.calls")" = 1 ] && grep -q "plan を作りました" "$H/posted" && [ "$rc" = 0 ]'
unset GUARD_HOUR

# --- unit-done.sh（ExecStopPost）と mackerel-alive.sh --------------------------------------
new_home 23
SERVICE_RESULT=success EXIT_CODE=exited EXIT_STATUS=0 bash "$H/deploy/unit-done.sh" GUARD "寄る前の自動復旧"
check "成功なら ping 0・通知なし" 'grep -q " 0$" "$H/state/ping/GUARD" && [ ! -f "$H/posted" ]'
SERVICE_RESULT=exit-code EXIT_CODE=exited EXIT_STATUS=1 bash "$H/deploy/unit-done.sh" GUARD "寄る前の自動復旧"
check "ふつうの失敗は ping 1・通知はスクリプト任せ" 'grep -q " 1$" "$H/state/ping/GUARD" && [ ! -f "$H/posted" ]'
SERVICE_RESULT=timeout EXIT_CODE=killed EXIT_STATUS=TERM bash "$H/deploy/unit-done.sh" CLOSENET "引けの安全網" "建玉を確かめる"
check "時間切れは ping に残して「途中で止められた」を通知" 'grep -q " TERM$" "$H/state/ping/CLOSENET" && grep -q "途中で止められました（timeout）" "$H/posted" && grep -q "建玉を確かめる" "$H/posted"'
now="$(TZ=Asia/Tokyo date +%F) 1600 3"
body=$(MACKEREL_ALIVE_NOW="$now" sh "$H/deploy/mackerel-alive.sh" --dry-run)
check "mackerel-alive: 止められた CLOSENET は 1、成功の後に失敗した GUARD は 1" 'grep -q "\"alive.closenet\",\"time\":[0-9]*,\"value\":1" <<<"$body" && grep -q "\"alive.guard\",\"time\":[0-9]*,\"value\":1" <<<"$body"'
rm -f "$H/state/ping/GUARD"
body=$(MACKEREL_ALIVE_NOW="$now" sh "$H/deploy/mackerel-alive.sh" --dry-run)
check "mackerel-alive: 平日の期限後に GUARD の印が無ければ 2" 'grep -q "\"alive.guard\",\"time\":[0-9]*,\"value\":2" <<<"$body"'

exit "$fail"
