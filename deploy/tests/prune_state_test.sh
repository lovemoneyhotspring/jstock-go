#!/usr/bin/env bash
# deploy/prune-state.sh の試験。一時ディレクトリに置き場の形を作り、WBJP_HOME をそこへ向けて回す。
# 本物の state・data には触れない。
#
#   deploy/tests/prune_state_test.sh
set -uo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
fail=0
ok()  { echo "ok   $*"; }
ng()  { echo "FAIL $*"; fail=1; }
check() { if eval "$2"; then ok "$1"; else ng "$1"; fi; }

S="$T/state"
mkdir -p "$S/logs" "$S/digest" "$S/backup/crontab" "$T/data/jquants/_raw/equities_bars_daily"
# ログ: 2MB（上限 1MB を超える）と小さいもの。退避先の .1 は前からある
head -c $((2 * 1024 * 1024)) /dev/zero > "$S/logs/big.log"
echo old > "$S/logs/big.log.1"
echo small > "$S/logs/small.log"
echo '{}' > "$S/logs/daytrade-prod.jsonl"
# digest: 今日を 2026-09-24 として 400 日前は 2025-08-20
for d in 2025-08-19 2025-08-20 2026-09-23; do echo '{}' > "$S/digest/prod-$d.jsonl"; done
echo '{}' > "$S/digest/uat-2025-01-01.jsonl"
echo x > "$S/digest/notes.jsonl"          # 日付の無い名前は触らない
# crontab の控え: 古いもの 25 本（2026-01-01〜）と新しいもの 3 本。以前の置き場にも 2 本
for k in $(seq -w 1 25); do echo c > "$S/backup/crontab/crontab-202601${k}-120000.txt"; done
echo c > "$S/backup/crontab/crontab-20260110-130000-before-restore.txt"
for d in 20260920 20260921 20260922; do echo c > "$S/backup/crontab/crontab-$d-120000.txt"; done
echo c > "$S/logs/crontab.backup.20250101-000000"
echo c > "$S/logs/crontab.backup.20260920-083208"
echo c > "$S/backup/crontab/handmade.txt"
echo raw > "$T/data/jquants/_raw/equities_bars_daily/x_201608.csv.gz"
# 立花のセッション: 今日を 2026-09-24 として 7 日前は 2026-09-17。0910 の組はロックを握られている
mkdir -p "$S/tachibana"
for d in 20260910 20260916 20260917 20260924; do
  echo '{}' > "$S/tachibana/session-prod-$d.json"; : > "$S/tachibana/session-prod-$d.json.lock"
done
: > "$S/tachibana/session-uat-20260911.json.lock"   # ロックだけ残った組
echo x > "$S/tachibana/session-notes.json"          # 日付の無い名前は触らない
# test/out: 更新時刻で見る（名前に日付が無い）。31 日前は消し、29 日前・.gitkeep は残す
mkdir -p "$T/test/out/sub"
echo o > "$T/test/out/old.parquet"; touch -d "31 days ago" "$T/test/out/old.parquet"
echo o > "$T/test/out/sub/old.csv"; touch -d "31 days ago" "$T/test/out/sub/old.csv"
echo n > "$T/test/out/recent.parquet"; touch -d "29 days ago" "$T/test/out/recent.parquet"
: > "$T/test/out/.gitkeep"; touch -d "100 days ago" "$T/test/out/.gitkeep"
flock "$S/tachibana/session-prod-20260910.json.lock" sleep 30 &
holder=$!
trap 'kill "$holder" 2>/dev/null; rm -rf "$T"' EXIT
sleep 0.3

before=$(find "$T" -type f | sort | xargs -I{} sh -c 'echo "{} $(stat -c %s {})"')
out=$(WBJP_HOME="$T" PRUNE_TODAY=2026-09-24 PRUNE_LOG_MAX_MB=1 "$REPO/deploy/prune-state.sh" --dry-run); rc=$?
after=$(find "$T" -type f | sort | xargs -I{} sh -c 'echo "{} $(stat -c %s {})"')
check "--dry-run は何も変えない（exit 0）" '[ "$rc" = 0 ] && [ "$before" = "$after" ]'
check "--dry-run は数を出す" 'printf "%s\n" "$out" | tail -1 | grep -q "logs（1MB 超を退避）1 本.*digest（400 日より前）2 件.*crontab の控え（90 日より前・新しい 20 世代は残す）11 件.*立花のセッション（7 日より前）3 件.*test/out（30 日より前）2 件"'

out=$(WBJP_HOME="$T" PRUNE_TODAY=2026-09-24 PRUNE_LOG_MAX_MB=1 "$REPO/deploy/prune-state.sh"); rc=$?
check "本番の回も exit 0" '[ "$rc" = 0 ]'
check "上限を超えたログは .1 へ退避し、元の名前は空く" \
  '[ ! -e "$S/logs/big.log" ] && [ "$(stat -c %s "$S/logs/big.log.1")" = $((2 * 1024 * 1024)) ]'
check "小さいログと JSONL は触らない" '[ -f "$S/logs/small.log" ] && [ -f "$S/logs/daytrade-prod.jsonl" ]'
check "digest は 400 日より前だけ消す（境目の日は残す・env によらない）" \
  '[ ! -e "$S/digest/prod-2025-08-19.jsonl" ] && [ ! -e "$S/digest/uat-2025-01-01.jsonl" ] && [ -f "$S/digest/prod-2025-08-20.jsonl" ] && [ -f "$S/digest/prod-2026-09-23.jsonl" ]'
check "日付の無い名前は触らない" '[ -f "$S/digest/notes.jsonl" ] && [ -f "$S/backup/crontab/handmade.txt" ]'
# 新しい順: 0920〜0922（4 本。以前の置き場の 0920 を含む）+ 1 月の 26 本の新しい方 16 本 = 20 本残る
check "crontab の控えは新しい 20 世代を残し、残りのうち 90 日より前を消す" \
  '[ "$(ls "$S/backup/crontab" | grep -c "^crontab-")" = 19 ] && [ -f "$S/backup/crontab/crontab-20260125-120000.txt" ] && [ -f "$S/backup/crontab/crontab-20260110-130000-before-restore.txt" ] && [ ! -e "$S/backup/crontab/crontab-20260101-120000.txt" ]'
check "以前の置き場（state/logs/crontab.backup.*）も同じ並びで数える" \
  '[ ! -e "$S/logs/crontab.backup.20250101-000000" ] && [ -f "$S/logs/crontab.backup.20260920-083208" ]'
check "立花のセッションは 7 日より前の組を消す（境目の日と当日は残す・ロックだけの組も消す）" \
  '[ ! -e "$S/tachibana/session-prod-20260916.json" ] && [ ! -e "$S/tachibana/session-prod-20260916.json.lock" ] && [ ! -e "$S/tachibana/session-uat-20260911.json.lock" ] && [ -f "$S/tachibana/session-prod-20260917.json" ] && [ -f "$S/tachibana/session-prod-20260924.json.lock" ]'
check "ロックを握られている組は古くても残す" \
  '[ -f "$S/tachibana/session-prod-20260910.json" ] && [ -f "$S/tachibana/session-prod-20260910.json.lock" ] && printf "%s\n" "$out" | grep -q "使用中のため残す"'
check "日付の無いセッションの名前は触らない" '[ -f "$S/tachibana/session-notes.json" ]'
check "test/out は 30 日より前だけ消す（下のディレクトリも・.gitkeep とディレクトリは残す）" \
  '[ ! -e "$T/test/out/old.parquet" ] && [ ! -e "$T/test/out/sub/old.csv" ] && [ -f "$T/test/out/recent.parquet" ] && [ -f "$T/test/out/.gitkeep" ] && [ -d "$T/test/out/sub" ]'
check "data/jquants/_raw は消さない" '[ -f "$T/data/jquants/_raw/equities_bars_daily/x_201608.csv.gz" ]'

"$REPO/deploy/prune-state.sh" --bogus >/dev/null 2>&1; rc=$?
check "知らない引数は 64" '[ "$rc" = 64 ]'

exit "$fail"
