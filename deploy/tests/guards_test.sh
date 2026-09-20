#!/usr/bin/env bash
# deploy/close-net.sh と deploy/guard-preopen.sh の試験。スタブの bin と crontab を使う隔離環境で動かし、
# 本物の bin・crontab・Discord には触れない。
#
#   deploy/tests/guards_test.sh
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
  cp "$REPO"/deploy/{with-lock.sh,lib-notify.sh,close-net.sh,guard-preopen.sh} "$H/deploy/"
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

# --- close-net -----------------------------------------------------------------
new_home 1
cat > "$H/bin/daytrade" <<EOS
#!/bin/sh
echo run >> "$H/daytrade.calls"
echo '{"command":"close","outcome":"ok","ts_utc":"${day}T06:22:00+00:00"}' >> "$H/state/digest/prod-$day.jsonl"
EOS
chmod +x "$H/bin/daytrade"
# 1. cron の close が成功済み → 何もしない
echo '{"command":"close","outcome":"ok","ts_utc":"'"${day}"'T06:20:30+00:00"}' > "$H/state/digest/prod-$day.jsonl"
bash "$H/deploy/close-net.sh"
check "close 済みなら close を走らせない" '[ ! -f "$H/daytrade.calls" ]'
# 2. 成功記録なし（別の日・失敗・15:20 より前）→ 走らせて通知
echo '{"command":"close","outcome":"error","ts_utc":"'"${day}"'T06:20:30+00:00"}
{"command":"close","outcome":"ok","ts_utc":"'"${day}"'T05:00:00+00:00"}' > "$H/state/digest/prod-$day.jsonl"
bash "$H/deploy/close-net.sh"
check "成功記録が無ければ close を走らせる" '[ "$(wc -l < "$H/daytrade.calls")" = 1 ]'
check "走らせたら通知する" 'grep -q "安全網が close を実行" "$H/posted"'
# 3. 失敗 → 失敗を通知し、終了コードを返す
new_home 2
printf '#!/bin/sh\nexit 3\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
bash "$H/deploy/close-net.sh"; rc=$?
check "close が失敗したら rc を返す" '[ "$rc" = 3 ]'
check "失敗を通知する" 'grep -q "安全網の close が失敗" "$H/posted"'
# 4. 休場日など（close は 0 で終わるが ok の記録を残さない）→ 通知しない
new_home 3
printf '#!/bin/sh\nexit 0\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
bash "$H/deploy/close-net.sh"
check "何もせず終わった日は通知しない" '[ ! -f "$H/posted" ]'

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
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "設定を読めなければ作り直して復旧する" '[ -f "$H/build.calls" ] && grep -q "復旧しました" "$H/posted" && [ ! -f "$H/rb.calls" ] && [ "$rc" = 0 ]'
# 6. 作り直しても直らない → 1 世代前へ戻す → それで直る
new_home 6
printf '#!/bin/sh\necho "strict mode: fields missing"\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' 'printf "#!/bin/sh\\necho ok\\n" > "$(dirname "$0")/../bin/daytrade"'
bash "$H/deploy/guard-preopen.sh"; rc=$?
check "作り直しでも直らなければ 1 世代前へ戻す" '[ -f "$H/build.calls" ] && [ -f "$H/rb.calls" ] && grep -q "1 世代前の実行ファイルへ戻しました" "$H/posted" && [ "$rc" = 0 ]'
# 7. どうしても直らない → 失敗を通知して rc=1
new_home 7
printf '#!/bin/sh\necho "strict mode: fields missing"\necho "preflight-problems: config"\nexit 1\n' > "$H/bin/daytrade"; chmod +x "$H/bin/daytrade"
stub_common '' ''
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
exit "$fail"
