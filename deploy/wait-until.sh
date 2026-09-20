#!/bin/sh
# 指定した時刻（JST、今日）ちょうどまで待つ。cron の 1 行の先頭に挟んで使う。
#
#   deploy/wait-until.sh HH:MM:SS[.s]
#
# cron は毎分 :01〜:02 秒ごろに起こすので、`sleep 3` と書いても実際の開始は :04 台になる
# （vault 30-projects/daytrade-open-latency.md）。1 分前の行から起こしてここで待てば、
# cron の揺れが経路から消えて 0.1 秒の刻みで時刻を決められる。
#
# - もう過ぎていればすぐ抜ける（exit 0。遅れて起きた回を止めない）
# - 120 秒より先なら何もせず exit 1（時刻の書き間違いで発注の回が何分も寝ないように）
set -eu
target="${1:?usage: wait-until.sh HH:MM:SS[.s]}"
now=$(TZ=Asia/Tokyo date +%s.%N)
at=$(TZ=Asia/Tokyo date -d "today $target" +%s.%N)
wait=$(awk -v a="$at" -v n="$now" 'BEGIN { printf "%.3f", a - n }')
case "$wait" in
-*) exit 0 ;;
esac
if awk -v w="$wait" 'BEGIN { exit !(w > 120) }'; then
	echo "wait-until: $target は ${wait} 秒先。120 秒より先は待たない" >&2
	exit 1
fi
sleep "$wait"
