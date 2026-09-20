#!/usr/bin/env bash
# 引けの手仕舞いの安全網。**cron とは別系統**（systemd のタイマー）から回り、人が気づく前提にしない。
#
#   deploy/close-net.sh   # 平日 15:22・15:26（deploy/systemd/jstock-close-net.timer）
#
# cron の close（15:20・15:24・15:28）が走っていなければ、代わりに走らせる。cron が止まった・
# crontab が消えた・cron の行が壊れた、のいずれでも、マシンが生きていれば手仕舞える。
# マシンやネットが止まっているときは、ブローカーに置いた保険の引け注文
# （execution.protect_exit。daytrade protect）が引けで手仕舞う。
#
# 「今日の close が成功した」記録（ダイジェストの app=daytrade・command=close・outcome=ok・live=true・
# 15:20 JST 以降。dry-run は数えない）があれば何もしない。無ければ close を走らせる——close は何度回しても重ならない（手仕舞い済みは
# 数えない）ので、cron の回と重なっても害は無い。ロックは cron と同じ（with-lock.sh）。
# 休場日は close 自身が見送る。走らせたときは Discord に 1 通。
set -uo pipefail
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/lib-notify.sh"

CFG="${DAYTRADE_CONFIG_DIR:-config/daytrade_margin}"
day=$(TZ=Asia/Tokyo date +%F)
digest="$HOME_DIR/state/digest/${WBJP_ENV}-$day.jsonl"

# 15:20 JST = 06:20 UTC（同じ暦日）。ts_utc は "2026-09-18T11:30:13.348176+00:00" の形
done_ok=0
if [ -f "$digest" ]; then
  n=$(jq -c --arg since "${day}T06:20" \
        'select(.app == "daytrade" and .command == "close" and .outcome == "ok" and .live == true and (.verify | not) and (.ts_utc >= $since))' \
        "$digest" 2>/dev/null | wc -l)
  [ "${n:-0}" -gt 0 ] && done_ok=1
fi
if [ "$done_ok" -eq 1 ]; then
  log "close は cron が済ませている（$day）。何もしない"
  exit 0
fi

log "[run] 今日の close の成功記録が無い → 安全網として close を実行（$day）"
WITH_LOCK_TIMEOUT=170 "$HOME_DIR/deploy/with-lock.sh" "${CLOSE_NET_LOCK:-/tmp/daytrade.lock}" "${CLOSE_NET_LOCK_WAIT:-30}" "$HOME_DIR/state/logs/daytrade-close.log" \
  "$HOME_DIR/bin/daytrade" close --config-dir "$CFG" --live --yes
rc=$?
if [ "$rc" -eq 75 ]; then
  # with-lock.sh がロックを取れず見送った（cron の close が実行中）。cron の回が手仕舞うので何もしない
  log "cron の close が実行中（ロックを取れず見送り）。何もしない"
  exit 0
fi
if [ "$rc" -eq 0 ]; then
  # 休場日は close が見送る（digest は skipped）。手仕舞いを実行した日だけ知らせる
  if [ -f "$digest" ] && jq -e 'select(.app == "daytrade" and .command == "close" and .outcome == "ok" and .live == true and (.verify | not) and (.ts_utc >= "'"${day}"'T06:20"))' "$digest" >/dev/null 2>&1; then
    notify "デイトレ: 安全網が close を実行しました" "cron の close が走っていなかったため、systemd の安全網（deploy/close-net.sh）が実行しました。cron・crontab を確認してください（$day）"
  else
    log "close は何もせず終わった（休場日・時間帯の外など）"
  fi
else
  notify "デイトレ: 安全網の close が失敗しました（rc=$rc）" "cron の close が走っていないうえ、安全網の close も失敗しました。建玉が残っている恐れがあります（$day）。state/logs/daytrade-close.log を確認してください。ブローカーに保険の引け注文があれば引けで手仕舞われます"
fi
exit "$rc"
