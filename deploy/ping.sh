#!/bin/sh
# 外の死活監視（healthchecks.io 型）へ「生きている」を打つ。
#
#   deploy/ping.sh <KEY> [終了コード]
#
# 通知はどれも**このマシンから** Discord へ出るので、マシン停止・ネット断・cron 停止では何も鳴らない。
# 場中に落ちれば建玉を持ち越す。外のサービスに定時の ping を打ち、**来なかったら向こうが知らせる**。
#
# 送り先は .env の HEALTHCHECK_URL_<KEY>（例: HEALTHCHECK_URL_MORNING=https://hc-ping.com/<uuid>）。
# **未設定なら ping は打たない**（exit 0。下の印だけ残す）。終了コードを渡すと、0 以外のとき URL の末尾に /<終了コード> を
# 付ける（healthchecks.io は失敗として記録する）。休場日も打つ——監視側の予定は「平日の毎日」でよい。
#
# ping が失敗しても cron の行の結果は変えない（常に exit 0）。
set -u

key=${1:-}
rc=${2:-0}
if [ -z "$key" ]; then
  echo "usage: ping.sh <KEY> [exit_code]" >&2
  exit 64
fi

# 鍵は変数名と sed のパターンに入る。英大文字・数字・_ だけを通す
case "$key" in
  *[!A-Z0-9_]*) echo "ping.sh: KEY は英大文字・数字・_ だけ: $key" >&2; exit 64 ;;
esac

home=${WBJP_HOME:-$(cd "$(dirname "$0")/.." && pwd)}
# 届いた印を手元にも残す（日付 時刻 終了コード）。deploy/mackerel-alive.sh がこれを読んで Mackerel へ
# 状態を投稿する。URL が未設定でも残す
mkdir -p "$home/state/ping" 2>/dev/null \
  && echo "$(date '+%Y-%m-%d %H:%M:%S') $rc" > "$home/state/ping/$key.tmp" \
  && mv -f "$home/state/ping/$key.tmp" "$home/state/ping/$key"
url=$(eval "printf '%s' \"\${HEALTHCHECK_URL_$key:-}\"")
if [ -z "$url" ] && [ -f "$home/.env" ]; then
  # .env を丸ごと source しない（cron の行の環境を変えない）。該当の 1 行だけ読む
  url=$(sed -n "s/^HEALTHCHECK_URL_$key=//p" "$home/.env" | tail -1 | tr -d '"'"'")
fi
[ -n "$url" ] || exit 0

case "$rc" in
  0|'') ;;
  *[!0-9]*) ;;
  *) url="$url/$rc" ;;
esac
curl -fsS -m 10 --retry 3 --retry-delay 2 -o /dev/null "$url" \
  || echo "$(date '+%Y-%m-%d %H:%M:%S') [warn] 死活監視の ping に失敗: $key" >&2
exit 0
