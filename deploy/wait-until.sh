#!/bin/sh
# 指定した時刻（JST、今日）ちょうどまで待つ。cron の 1 行の先頭に挟んで使う。
#
#   [WAIT_UNTIL_LOG=<log>] deploy/wait-until.sh HH:MM:SS[.s]
#
# cron は毎分 :01〜:02 秒ごろに起こすので、`sleep 3` と書いても実際の開始は :04 台になる
# （vault 30-projects/daytrade-open-latency.md）。1 分前の行から起こしてここで待てば、
# cron の揺れが経路から消えて 0.1 秒の刻みで時刻を決められる。
#
# - もう過ぎていればすぐ抜ける（exit 0。遅れて起きた回を止めない）
# - 120 秒より先なら何もせず exit 1（時刻の書き間違いで発注の回が何分も寝ないように）
# - 時刻が空・読めないときも exit 1（後ろの `&&` の回は走らない。いつ走るか分からない回は走らせない）
#
# **見送った事実は WAIT_UNTIL_LOG に 1 行残す**（書式は deploy/with-lock.sh の見送りと同じ
# 「日時 [error] [札] …」）。ここで止まると後ろの with-lock.sh に届かないので、残さなければ
# 通知もログも無い——stderr は crontab で捨てられ、構造化ログにも回が現れない（2026-09-21 のレビュー）。
# 行き先は後ろの回と同じログにする（open なら state/logs/daytrade-open.log）。[error] なので
# deploy/morning-check.sh が拾い、[wait_skip] の札で「時刻待ちの見送り」として ❌ に出す。
# 行き先を引数でなく環境変数で受けるのは、crontab の変数（$DT_OPEN_AT）が空のとき引数が
# 1 つずれて、ログのパスを時刻として読むのを避けるため。
set -eu

# skip <理由>: stderr とログに残して exit 1。ログに書けなくても見送りは変えない（fail-closed のまま）
skip() {
	echo "wait-until: $1" >&2
	if [ -n "${WAIT_UNTIL_LOG:-}" ]; then
		echo "$(TZ=Asia/Tokyo date '+%Y-%m-%d %H:%M:%S') [error] [wait_skip] $1。後ろの回は起動していない" >> "$WAIT_UNTIL_LOG" 2>/dev/null || true
	fi
	exit 1
}

target="${1:-}"
[ -n "$target" ] || skip "時刻が空（usage: wait-until.sh HH:MM:SS[.s]。crontab の変数が消えていないか）"
now=$(TZ=Asia/Tokyo date +%s.%N)
at=$(TZ=Asia/Tokyo date -d "today $target" +%s.%N 2>/dev/null) || skip "時刻 '$target' を読めない"
wait=$(awk -v a="$at" -v n="$now" 'BEGIN { printf "%.3f", a - n }')
case "$wait" in
-*) exit 0 ;;
esac
if awk -v w="$wait" 'BEGIN { exit !(w > 120) }'; then
	skip "$target は ${wait} 秒先。120 秒より先は待たない"
fi
sleep "$wait"
