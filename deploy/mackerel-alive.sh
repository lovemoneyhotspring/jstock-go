#!/bin/sh
# Mackerel のサービスメトリックへ「cron が回っている」「定時の点検が済んだ」を 5 分おきに投稿する。
#
#   deploy/mackerel-alive.sh [--dry-run]
#
# 通知はどれも**このマシンから** Discord へ出るので、マシン停止・ネット断・cron 停止では何も鳴らない。
# 外から見る役は Mackerel に寄せる:
#   - マシン停止・ネット断 … mackerel-agent の connectivity 監視（もとから動いている）
#   - cron 停止           … alive.cron の途切れ監視（この投稿が来なくなる）
#   - 点検の失敗・未実行   … alive.morning / alive.verify が 0 でなくなる
#   - systemd の安全網の失敗・未実行 … alive.guard / alive.closenet が 0 でなくなる
#   - レーティングの取り込みの失敗・未実行 … alive.rate が 0 でなくなる
#
# 値の意味（alive.morning / alive.verify / alive.guard / alive.closenet / alive.rate）:
#   0 = 正常（今日の ping が終了コード 0 で届いた・まだ期限前・土日）
#   1 = 今日の ping は届いたが終了コードが 0 でない
#   2 = 平日で期限を過ぎたのに今日の ping が無い
# 「届いた」は deploy/ping.sh が state/ping/<KEY> に残す 1 行（日付 時刻 終了コード）で見る。
# 休場日も cron は ping を打つので、期待は「平日の毎日」でよい（ping.sh と同じ）。
#
# API キーは .env の MACKEREL_APIKEY、無ければ mackerel-agent の設定から読む。どちらも無ければ何もしない。
# 投稿に失敗しても exit 0（cron のメールを増やさない。来なければ途切れ監視が鳴る）。
set -u

dry=0
[ "${1:-}" = "--dry-run" ] && dry=1

home=${WBJP_HOME:-$(cd "$(dirname "$0")/.." && pwd)}
service=${MACKEREL_SERVICE:-jstock}
agent_conf=${MACKEREL_AGENT_CONF:-/etc/mackerel-agent/mackerel-agent.conf}
# テスト用: 現在時刻を差し替える（"YYYY-MM-DD HHMM u"。u は date +%u）
now=${MACKEREL_ALIVE_NOW:-$(date '+%Y-%m-%d %H%M %u')}
today=${now%% *}
rest=${now#* }
hhmm=${rest%% *}
dow=${rest#* }

# KEY:期限（HHMM）。9:22 の朝の点検と 15:40 の verify に、終わるまでの猶予を足した時刻
# GUARD・CLOSENET は cron ではなく systemd のタイマー（jstock-guard 8:42・15:12、jstock-close-net
# 15:22・15:26）の ExecStopPost（deploy/unit-done.sh）が打つ。期限は 8:42 の回の上限（600 秒）と
# 15:26 の回の上限（240 秒）に猶予を足した時刻。1 日に 2 回打つので、値は後の回の終わり方になる。
# Mackerel 側の監視ルール（alive.guard > 0・alive.closenet > 0・alive.rate > 0）は 2026-09-25 に足した
# （deploy/install-systemd.sh でユニットを入れ直す前は印が無く、平日の期限後は 2 になる。ルールは入れ直した後に）
# RATE は 7:30 の rate sync（cron。平日の日単位の失敗で終了 1）。ふだん数十秒で終わるので期限は 8:30
checks="MORNING:0940 VERIFY:1600 GUARD:0900 CLOSENET:1535 RATE:0830"

state_of() {
  key=$1
  due=$2
  f="$home/state/ping/$key"
  if [ -f "$f" ]; then
    # 1 行: YYYY-MM-DD HH:MM:SS rc
    read -r d _ rc < "$f" || true
    if [ "${d:-}" = "$today" ]; then
      case "${rc:-}" in
        0) echo 0 ;;
        *) echo 1 ;;
      esac
      return
    fi
  fi
  if [ "$dow" -le 5 ] && [ "$hhmm" -ge "$due" ]; then
    echo 2
  else
    echo 0
  fi
}

epoch=$(date +%s)
body="[{\"name\":\"alive.cron\",\"time\":$epoch,\"value\":1}"
for c in $checks; do
  key=${c%%:*}
  due=${c##*:}
  name=$(printf '%s' "$key" | tr 'A-Z' 'a-z')
  body="$body,{\"name\":\"alive.$name\",\"time\":$epoch,\"value\":$(state_of "$key" "$due")}"
done
body="$body]"

if [ "$dry" -eq 1 ]; then
  echo "$body"
  exit 0
fi

apikey=${MACKEREL_APIKEY:-}
if [ -z "$apikey" ] && [ -f "$home/.env" ]; then
  # .env を丸ごと source しない（ping.sh と同じ）。該当の 1 行だけ読む
  apikey=$(sed -n "s/^MACKEREL_APIKEY=//p" "$home/.env" | tail -1 | tr -d '"'"'")
fi
if [ -z "$apikey" ] && [ -r "$agent_conf" ]; then
  apikey=$(sed -n -E 's/^apikey[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "$agent_conf" | head -1)
fi
[ -n "$apikey" ] || exit 0

# キーは引数に出さない（ps から見える）。ヘッダは標準入力の設定で渡す
printf 'header = "X-Api-Key: %s"\n' "$apikey" \
  | curl -fsS -m 10 --retry 2 --retry-delay 2 -o /dev/null -K - \
      -X POST -H 'Content-Type: application/json' -d "$body" \
      "https://api.mackerelio.com/api/v0/services/$service/tsdb" \
  || echo "$(date '+%Y-%m-%d %H:%M:%S') [warn] Mackerel への投稿に失敗" >&2
exit 0
