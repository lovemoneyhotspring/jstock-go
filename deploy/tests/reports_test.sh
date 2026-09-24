#!/usr/bin/env bash
# deploy/morning-check.sh・deploy/night-repair.sh・deploy/report.sh の終了コードと環境の試験。
# スタブの bin（claude・discord-post・jquants）を使う隔離環境で動かし、本物の Discord・claude には触れない。
#
#   deploy/tests/reports_test.sh
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

today=$(TZ=Asia/Tokyo date +%F)

new_home() {
  H="$T/home$1"; rm -rf "$H"
  mkdir -p "$H/deploy" "$H/bin" "$H/state/logs" "$H/state/digest" "$H/fakebin"
  cp "$REPO"/deploy/{morning-check.sh,night-repair.sh,report.sh} "$H/deploy/"
  # discord-post: 標準入力と、見えた環境変数（秘密が混ざっていないか）を残す。POST_FAIL があれば失敗する
  cat > "$H/bin/discord-post" <<'EOS'
#!/bin/sh
D="$(dirname "$0")/.."
echo "ARGS=$* BODY=$(cat | head -c 200)" >> "$D/posted"
[ -f "$D/POST_FAIL" ] && exit 1
exit 0
EOS
  chmod +x "$H/bin/discord-post"
  # jquants: 取引カレンダーの HolDiv を返す（既定は営業日の 1）
  printf '#!/bin/sh\necho HolDiv\necho "${FAKE_HOLDIV:-1}"\n' > "$H/bin/jquants"; chmod +x "$H/bin/jquants"
  # claude: 環境を残してレポート本文を出す
  cat > "$H/fakebin/claude" <<'EOS'
#!/bin/sh
D="$(dirname "$0")/.."
env > "$D/claude.env"
cat > /dev/null
echo "# レポート本文"
EOS
  chmod +x "$H/fakebin/claude"
  printf 'WBJP_ENV=prod\nWBJP_PROD_APP_SECRET=secret-must-not-leak\nWBJP_DISCORD_BOT_TOKEN=tok\n' > "$H/.env"
  ( cd "$H" && git init -q -b main && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m init )
  export WBJP_HOME="$H" POST_BIN="$H/bin/discord-post" QUERY_BIN="$H/bin/jquants" CLAUDE_BIN="$H/fakebin/claude" WBJP_ENV=prod
  unset WBJP_PROD_APP_SECRET WBJP_DISCORD_BOT_TOKEN
}

# --- morning-check --------------------------------------------------------------------
# 板も open のログも無い仮ホームなので、問題は必ず 1 件以上出る
new_home 1
bash "$H/deploy/morning-check.sh" > /dev/null 2>&1; rc=$?
check "morning-check: 問題があれば送ったうえで 1" '[ "$rc" = 1 ] && grep -q "寄り付きの記録" "$H/posted"'
touch "$H/POST_FAIL"
bash "$H/deploy/morning-check.sh" > /dev/null 2>&1; rc=$?
check "morning-check: Discord に送れなければ 3" '[ "$rc" = 3 ]'
rm -f "$H/POST_FAIL"
POST_BIN="$H/bin/nope" bash "$H/deploy/morning-check.sh" > /dev/null 2>&1; rc=$?
check "morning-check: discord-post が無ければ 3（以前は黙って 0）" '[ "$rc" = 3 ]'
rm -f "$H/posted"
NO_POST=1 bash "$H/deploy/morning-check.sh" > /dev/null 2>&1; rc=$?
check "morning-check: NO_POST=1 は送らず、問題の有無だけ返す" '[ "$rc" = 1 ] && [ ! -f "$H/posted" ]'
FAKE_HOLDIV=3 bash "$H/deploy/morning-check.sh" > /dev/null 2>&1; rc=$?
check "morning-check: 休場日は 0" '[ "$rc" = 0 ] && [ ! -f "$H/posted" ]'

# --- night-repair ----------------------------------------------------------------------
dg() { echo "$H/state/digest/prod-$today.jsonl"; }
# 1. 異常なし → claude を起こさない
new_home 2
echo '{"app":"daytrade","command":"open","outcome":"ok"}' > "$(dg)"
bash "$H/deploy/night-repair.sh" > /dev/null 2>&1; rc=$?
check "night-repair: 異常なしなら claude を起こさない" '[ "$rc" = 0 ] && [ ! -f "$H/claude.env" ]'
# 2. 壊れた行の後ろの異常も数える（以前は jq が壊れた行で止まり 0 件だった）
new_home 3
printf '{"command":"open","outcome":"ok"}\n{broken\n{"command":"close","outcome":"error"}\n' > "$(dg)"
out=$(bash "$H/deploy/night-repair.sh" 2>&1); rc=$?
check "night-repair: 壊れた行の後ろの異常も数え、壊れた行も異常に含める" '[ -f "$H/claude.env" ] && grep -q "異常 2 件" <<<"$out" && grep -q "読めない行が 1 行" <<<"$out"'
check "night-repair: claude に .env の秘密を渡さない" '! grep -q "secret-must-not-leak\|WBJP_DISCORD_BOT_TOKEN" "$H/claude.env"'
check "night-repair: 配達できれば 0" '[ "$rc" = 0 ] && grep -q "夜間自己修復" "$H/posted"'
# 3. 検証（verify）と backtest は数えない。オブジェクトでない行は壊れた行
new_home 4
printf '{"command":"open","outcome":"error","verify":true}\n{"command":"backtest","outcome":"error"}\n' > "$(dg)"
bash "$H/deploy/night-repair.sh" > /dev/null 2>&1
check "night-repair: verify・backtest は異常に数えない" '[ ! -f "$H/claude.env" ]'
echo '5' >> "$(dg)"
bash "$H/deploy/night-repair.sh" > /dev/null 2>&1
check "night-repair: オブジェクトでない行は壊れた行として異常" '[ -f "$H/claude.env" ]'
# 4. jq が落ちる → 異常の側に倒す（claude を起こす）
new_home 5
echo '{"command":"open","outcome":"ok"}' > "$(dg)"
printf '#!/bin/sh\necho "jq: 落ちた" >&2\nexit 2\n' > "$H/fakebin/jq"; chmod +x "$H/fakebin/jq"
out=$(PATH="$H/fakebin:$PATH" bash "$H/deploy/night-repair.sh" 2>&1)
check "night-repair: jq が落ちたら異常として claude を起こす" '[ -f "$H/claude.env" ] && grep -q "終了コード 2 で止まり" <<<"$out"'
# 5. 配達に失敗 → 0 以外
new_home 6
echo '{"command":"close","outcome":"error"}' > "$(dg)"
touch "$H/POST_FAIL"
bash "$H/deploy/night-repair.sh" > /dev/null 2>&1; rc=$?
check "night-repair: Discord に配達できなければ 3" '[ "$rc" = 3 ] && [ -s "$H/state/reports/night-repair-$today.md" ]'

# --- report.sh -------------------------------------------------------------------------
new_home 7
unset WBJP_ENV
bash "$H/deploy/report.sh" daily "$today" > /dev/null 2>&1; rc=$?
check "report: .env の WBJP_ENV で動き、配達できれば 0" '[ "$rc" = 0 ] && grep -q "レポート本文" "$H/posted"'
check "report: claude に .env の秘密を渡さない" '[ -f "$H/claude.env" ] && ! grep -q "secret-must-not-leak\|WBJP_DISCORD_BOT_TOKEN" "$H/claude.env" && grep -q "^WBJP_ENV=prod$" "$H/claude.env"'
touch "$H/POST_FAIL"
bash "$H/deploy/report.sh" daily "$today" > /dev/null 2>&1; rc=$?
check "report: Discord に配達できなければ 3" '[ "$rc" = 3 ]'
rm -f "$H/POST_FAIL"
printf '#!/bin/sh\ncat > /dev/null\nexit 1\n' > "$H/fakebin/claude"
bash "$H/deploy/report.sh" daily "$today" > /dev/null 2>&1; rc=$?
check "report: 生成に失敗したら失敗を送って 1" '[ "$rc" = 1 ] && grep -q "生成に失敗" "$H/posted"'
export WBJP_ENV=prod

[ "$fail" -eq 0 ] && echo "全部通った"
exit "$fail"
