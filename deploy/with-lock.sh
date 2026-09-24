#!/bin/sh
# cron の 1 行を flock で排他して回し、**ロックを取れずに見送った事実をログに残す**。
#
#   deploy/with-lock.sh <lock> <wait_sec> <log> <cmd...>
#
# flock -n は取れないとき無言で exit 1 する。発注経路（daytrade open / close）では
# 「前の実行が長引いて 9:04 の回が消えた」と「9:04 の回が走って失敗した」を後から
# 区別できないと困るので、見送りは log に 1 行書く。wait_sec を 0 より大きくすると
# その秒数までロックを待つ（snap が握っていても open が少し待てば取れる）。
#
# cmd には時間の上限も掛ける（WITH_LOCK_TIMEOUT 秒。既定 170、0 で無効）。cmd が固まると
# ロックを握ったままになり、以降の回が——open が固まれば guard と close まで——全部見送られる。
# ふだんの実行は長くても 10 秒。既定の 170 秒は open の回が 3 分おきだった頃の値で、次の回より
# 前に手放すためのもの。9:00 台は次の回が 22〜38 秒後なので、その時間帯の行（8:59・9:00 の snap）は
# crontab.txt 側で WITH_LOCK_TIMEOUT を短くしている。
# 過ぎたら TERM、さらに 10 秒で KILL する。打ち切りは [error] [timeout] として log に残す。
# daytrade open は TERM を受けたら実行品質（滑り）の記録を書き出してから終える。
#
# 打ち切りと、外からの SIGKILL（メモリ不足の OOM killer など）は分けて記録する。timeout(1) は
# TERM で終われば 124、TERM で終わらず -k の KILL まで行けば 137 を返すが、上限の前に OOM で
# 殺されたときも 137（128+9）になる。137 は実行時間で見分ける——上限（limit 秒）より前に終わった
# 137 は打ち切りではないので [error] [killed] として残し、137 のまま返す。
#
# WITH_LOCK_NOTIFY=1 を立てた行は、打ち切り・SIGKILL・見送りを Discord にも知らせる（bin/discord-post）。
# ログに書くだけだと、読むのは朝の点検（open と snap だけ）で、close・guard・verify が打ち切られても
# 誰も気づかない。snap のように「打ち切り・見送りが設計のうち」の行には立てない。
#
# 終了コード: cmd のもの。ロックを取れなかったときは 75（EX_TEMPFAIL）、打ち切ったときは 124、
# 打ち切り以外の SIGKILL（OOM など）は 137。
#
# 見送りの判定は flock -E の番兵 251 で行う。以前は 75 を番兵にしていたが、それだと
# cmd 自身の exit 75 と区別できず、走って失敗した回が [lock_busy] として記録され、
# deploy/morning-check.sh の「ロック見送り」の数を狂わせた。cmd が万一 251 を返しても
# 250 に写し替えて番兵と混ざらないようにする。
set -eu

lock=${1:-}
wait=${2:-0}
log=${3:-}
[ $# -ge 3 ] && shift 3
if [ -z "$lock" ] || [ -z "$log" ] || [ $# -eq 0 ]; then
  echo "usage: with-lock.sh <lock> <wait_sec> <log> <cmd...>" >&2
  exit 64
fi
if ! command -v flock >/dev/null 2>&1; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') [error] flock が見つかりません（util-linux）。実行を見送り: $*" >> "$log"
  exit 69
fi

cmdline=$*

# notify <題> <本文>: WITH_LOCK_NOTIFY=1 のときだけ Discord に送る。送れなくても終了コードは変えない。
notify() {
  [ "${WITH_LOCK_NOTIFY:-0}" = "1" ] || return 0
  post=${POST_BIN:-$(dirname "$0")/../bin/discord-post}
  [ -x "$post" ] || return 0
  if command -v timeout >/dev/null 2>&1; then
    printf '%s\n' "$2" | timeout 60 "$post" --title "$1" >> "$log" 2>&1 || true
  else
    printf '%s\n' "$2" | "$post" --title "$1" >> "$log" 2>&1 || true
  fi
}

limit=${WITH_LOCK_TIMEOUT:-170}
if [ "$limit" -gt 0 ] && command -v timeout >/dev/null 2>&1; then
  # TERM で終わらない（D 状態など）ときの KILL までの猶予。ロックが空くのは最悪 limit + これ。
  # 次の回までの間が詰まっている行（8:59 台の snap）は crontab 側で WITH_LOCK_KILL_AFTER を短くする
  set -- timeout -k "${WITH_LOCK_KILL_AFTER:-10}" "$limit" "$@"
else
  limit=0
fi

busy=251
# 子は sh -c で包み、cmd 自身の 251 だけ 250 に写す（"$@" は sh -c の位置引数）。
# 137 は実行時間を測って、上限まで走っていれば（-k の KILL で終わった打ち切り）124 に写す。
# ロックを待った時間を含めないよう、測るのはロックを取った後（この sh の中）
if flock -w "$wait" -E "$busy" "$lock" \
    env WITH_LOCK_LIMIT="$limit" sh -c '
      t0=$(date +%s); "$@"; rc=$?
      [ "$rc" -eq 251 ] && exit 250
      if [ "$rc" -eq 137 ] && [ "$WITH_LOCK_LIMIT" -gt 0 ] && [ $(($(date +%s) - t0)) -ge "$WITH_LOCK_LIMIT" ]; then
        exit 124
      fi
      exit "$rc"' with-lock "$@" >> "$log" 2>&1; then
  rc=0
else
  rc=$?
fi
if [ "$limit" -gt 0 ] && [ "$rc" -eq 124 ]; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') [error] [timeout] ${limit} 秒で終わらず打ち切り（ロック $lock を手放した）: $*" >> "$log"
  notify "[timeout] ${limit} 秒で打ち切り" "$cmdline（ログ: $log）。建玉・注文が残っていないか口座を確認してください"
  exit 124
fi
if [ "$rc" -eq 137 ]; then
  # 上限の前に SIGKILL で終わった。打ち切りではなく、たいていメモリ不足（OOM killer）
  echo "$(date '+%Y-%m-%d %H:%M:%S') [error] [killed] SIGKILL で終わった（打ち切りではない。メモリ不足の疑い。journalctl -k | grep -i oom で確かめる）: $*" >> "$log"
  notify "[killed] SIGKILL で終了（メモリ不足の疑い）" "$cmdline（ログ: $log）。journalctl -k | grep -i oom で確かめ、建玉・注文が残っていないか口座を確認してください"
  exit 137
fi
if [ "$rc" -eq "$busy" ]; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') [warn] [lock_busy] ロック $lock を ${wait} 秒待っても取れず見送り: $*" >> "$log"
  notify "[lock_busy] ロックを取れず見送り" "$cmdline（ロック $lock を ${wait} 秒待った。ログ: $log）"
  exit 75
fi
exit "$rc"
