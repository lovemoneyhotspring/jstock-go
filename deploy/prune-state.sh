#!/usr/bin/env bash
# 掃除の仕組みが無かった置き場を、保持の決まりを過ぎた分だけ片付ける。週 1 回 cron から（deploy/crontab.txt）。
#
#   deploy/prune-state.sh            # 片付ける
#   deploy/prune-state.sh --dry-run  # 片付けるものを数えて見せるだけ（何も書き換えない）
#
# 決まり（どれも控えめ・長め。足りなくなったら縮める）:
#   state/logs/*.log      cron の >> が書く素のログ。PRUNE_LOG_MAX_MB（既定 10）MB を超えたら
#                         <name>.log.1 へ退避する（前の .1 は上書き。1 本あたり最大でおよそ 2 倍）。
#                         rename なので、書いている最中のプロセスは退避先に書き続け、次の回から
#                         新しいファイルに書く（行を失わない）。朝の点検（morning-check.sh）は当日の行しか
#                         見ないので、退避した側を読まなくてよい。いちばん大きい jquants-sync.log でも
#                         年に 5MB 程度なので、ふだんは何もしない
#                         （JSONL の *.jsonl は Go 側が日次で退避し 90 日で消す。pkg/wbcore/logging/rotate.go。触らない）
#   state/digest/<env>-YYYY-MM-DD.jsonl
#                         日付が PRUNE_DIGEST_DAYS（既定 400）日より前なら消す。月次の振り返りと
#                         前年同月の見比べに足りる長さ。1 日 20KB 程度
#   state/backup/crontab/crontab-YYYYMMDD-HHMMSS*.txt と state/logs/crontab.backup.YYYYMMDD-HHMMSS（以前の置き場）
#                         日付が PRUNE_CRONTAB_DAYS（既定 90）日より前で、かつ新しい方から
#                         PRUNE_CRONTAB_KEEP（既定 20）世代に入らないものを消す（まとめて 1 つの並びで数える）
#   state/tachibana/session-<env>-YYYYMMDD.json と .json.lock
#                         立花のセッション（仮想URL と採番）とその flock 用のロック。1 日 1 組で、使うのは
#                         当日の分だけ（pkg/wbcore/broker/tachibana.go の sessionFilePath）。日付が
#                         PRUNE_SESSION_DAYS（既定 7）日より前なら消す。ロックを誰かが握っていれば
#                         （flock -n で取れなければ）その組は残す
#   data/jquants/_raw     **消さない。** 一括ダウンロードの csv.gz の原本で、J-Quants は 10 年より前を
#                         返さなくなる（2016 年の月が順に取れなくなっていく）。Parquet の変換にバグが
#                         あったときに API を叩き直さず作り直す最後の保険（docs/JQUANTS_ARCHIVE.md「冪等性・安全」）。
#                         再取得できないので日数では消さない。容量が問題になったら別の場所へ移す
#
# 名前から日付を読めないファイルは触らない（人が置いたものかもしれない。notify の pruneArchive と同じ）。
set -uo pipefail

HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
DRY_RUN=0
case "${1:-}" in
  --dry-run) DRY_RUN=1 ;;
  "") ;;
  *) echo "usage: prune-state.sh [--dry-run]" >&2; exit 64 ;;
esac

LOG_MAX_MB="${PRUNE_LOG_MAX_MB:-10}"
DIGEST_DAYS="${PRUNE_DIGEST_DAYS:-400}"
CRONTAB_DAYS="${PRUNE_CRONTAB_DAYS:-90}"
CRONTAB_KEEP="${PRUNE_CRONTAB_KEEP:-20}"
SESSION_DAYS="${PRUNE_SESSION_DAYS:-7}"
TODAY="${PRUNE_TODAY:-$(TZ=Asia/Tokyo date +%F)}"   # PRUNE_TODAY は試験用

stamp() { date '+%Y-%m-%d %H:%M:%S'; }
say() { echo "$(stamp) $*"; }
size_of() { stat -c %s "$1" 2>/dev/null || echo 0; }
human() { numfmt --to=iec --suffix=B "$1" 2>/dev/null || echo "${1}B"; }

failed=0
# remove <置き場の名前> <パス>: 消す（dry-run なら数えるだけ）。数と大きさは n[名前]・b[名前] に足す
declare -A n b
remove() {
  local place=$1 path=$2 size
  size=$(size_of "$path")
  n[$place]=$(( ${n[$place]:-0} + 1 ))
  b[$place]=$(( ${b[$place]:-0} + size ))
  if [ "$DRY_RUN" -eq 1 ]; then
    say "[dry-run] 消す: $path ($(human "$size"))"
  elif rm -f -- "$path"; then
    say "消した: $path ($(human "$size"))"
  else
    say "[error] 消せません: $path"; failed=1
  fi
}

# --- state/logs/*.log -------------------------------------------------------------
limit=$(( LOG_MAX_MB * 1024 * 1024 ))
for f in "$HOME_DIR"/state/logs/*.log; do
  [ -f "$f" ] || continue
  size=$(size_of "$f")
  [ "$size" -gt "$limit" ] || continue
  n[logs]=$(( ${n[logs]:-0} + 1 ))
  if [ -f "$f.1" ]; then b[logs]=$(( ${b[logs]:-0} + $(size_of "$f.1") )); fi
  if [ "$DRY_RUN" -eq 1 ]; then
    say "[dry-run] 退避する: $f ($(human "$size")) → $f.1（前の .1 は消える）"
  elif mv -f -- "$f" "$f.1"; then
    say "退避した: $f ($(human "$size")) → $f.1"
  else
    say "[error] 退避できません: $f"; failed=1
  fi
done

# --- state/digest -----------------------------------------------------------------
cutoff=$(date -d "$TODAY -$DIGEST_DAYS days" +%F)
for f in "$HOME_DIR"/state/digest/*.jsonl; do
  [ -f "$f" ] || continue
  name=$(basename "$f" .jsonl)
  day=${name: -10}
  [[ "$day" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || continue
  [[ "$day" < "$cutoff" ]] && remove digest "$f"
done

# --- crontab の控え ------------------------------------------------------------------
# 「YYYYMMDD-HHMMSS パス」を新しい順に並べ、先頭 KEEP 世代は日付によらず残す
cutoff=$(date -d "$TODAY -$CRONTAB_DAYS days" +%Y%m%d)
i=0
while read -r ts path; do
  [ -n "$path" ] || continue
  i=$((i + 1))
  [ "$i" -le "$CRONTAB_KEEP" ] && continue
  [[ "${ts%%-*}" < "$cutoff" ]] && remove crontab "$path"
done < <(
  for f in "$HOME_DIR"/state/backup/crontab/crontab-*.txt "$HOME_DIR"/state/logs/crontab.backup.*; do
    [ -f "$f" ] || continue
    if [[ "$(basename "$f")" =~ ([0-9]{8}-[0-9]{6}) ]]; then
      printf '%s %s\n' "${BASH_REMATCH[1]}" "$f"
    fi
  done | sort -r
)

# --- 立花のセッション -------------------------------------------------------------------
# session-<env>-YYYYMMDD.json とそのロック（.json.lock）。ロックを握っているプロセスがいれば組ごと残す
# （日付の古い組を握ることは無いはずだが、握っている最中に消すと次に開いた側と別の inode を錠にしてしまう）
cutoff=$(date -d "$TODAY -$SESSION_DAYS days" +%Y%m%d)
for f in "$HOME_DIR"/state/tachibana/session-*.json "$HOME_DIR"/state/tachibana/session-*.json.lock; do
  [ -f "$f" ] || continue
  [[ "$(basename "$f")" =~ ^session-.+-([0-9]{8})\.json(\.lock)?$ ]] || continue
  [[ "${BASH_REMATCH[1]}" < "$cutoff" ]] || continue
  lock="${f%.lock}.lock"
  if [ -f "$lock" ] && ! flock -n "$lock" true; then
    say "使用中のため残す: $f（$lock を握っているプロセスがいる）"
    continue
  fi
  remove session "$f"
done

# --- まとめ -------------------------------------------------------------------------
verb="片付けた"; [ "$DRY_RUN" -eq 1 ] && verb="[dry-run] 片付ける"
say "$verb: logs（${LOG_MAX_MB}MB 超を退避）${n[logs]:-0} 本（消える .1 $(human "${b[logs]:-0}")）/" \
  "digest（${DIGEST_DAYS} 日より前）${n[digest]:-0} 件 $(human "${b[digest]:-0}") /" \
  "crontab の控え（${CRONTAB_DAYS} 日より前・新しい ${CRONTAB_KEEP} 世代は残す）${n[crontab]:-0} 件 $(human "${b[crontab]:-0}") /" \
  "立花のセッション（${SESSION_DAYS} 日より前）${n[session]:-0} 件 /" \
  "data/jquants/_raw は消さない"
exit "$failed"
