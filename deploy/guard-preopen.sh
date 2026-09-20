#!/usr/bin/env bash
# 寄る前の自動復旧。**cron とは別系統**（systemd のタイマー）から回り、人が気づく前提にしない。
#
#   deploy/guard-preopen.sh   # 平日 8:42・15:12（deploy/systemd/jstock-guard.timer）
#
# 1. crontab が消えていないか（8:42・15:12 とも）。jstock-go の行が控え（state/crontab.good）の
#    半分未満なら、控えから戻す。ふだんの編集（数行の増減）では戻さない。
# 2. 寄る前の点検（daytrade preflight。8:42 だけ）。落ちたら直し方を選んで直す:
#      config … 実行ファイルが設定を読めない → deploy/build.sh で作り直す。まだ読めなければ
#               deploy/rollback-bin.sh で 1 世代前へ（設定が新しいときは作り直しが先。戻すと悪化する）
#      plan   … 今日の plan が無い・壊れている → daytrade plan --if-missing で作る
#      ledger / disk … 自動では直せない。通知する
# 直したか・直せなかったかを Discord に 1 通。何も問題が無ければ通知しない（ログだけ）。
set -uo pipefail
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/lib-notify.sh"

CRONTAB="${CRONTAB:-crontab}"
CFG="${DAYTRADE_CONFIG_DIR:-config/daytrade_margin}"
DAYTRADE="$HOME_DIR/bin/daytrade"
summary=()
failed=0

# --- 1. crontab ------------------------------------------------------------
good="$HOME_DIR/state/crontab.good"
if [ -f "$good" ]; then
  want=$(grep -v '^[[:space:]]*#' "$good" | grep -c 'WBJP_BIN/daytrade' || true)
  have=$("$CRONTAB" -l 2>/dev/null | grep -v '^[[:space:]]*#' | grep -c 'WBJP_BIN/daytrade' || true)
  if [ "${want:-0}" -gt 0 ] && [ "${have:-0}" -lt $((want / 2)) ]; then
    mkdir -p "$HOME_DIR/state/backup/crontab"
    "$CRONTAB" -l > "$HOME_DIR/state/backup/crontab/crontab-$(date +%Y%m%d-%H%M%S)-before-restore.txt" 2>/dev/null || true
    if "$CRONTAB" -n "$good" >/dev/null 2>&1 && "$CRONTAB" "$good"; then
      summary+=("crontab の daytrade の行が $have 本（控えは $want 本）だったので、控えから戻しました")
      log "[restore] crontab を控えから戻した（$have → $want 本）"
    else
      summary+=("crontab の daytrade の行が $have 本（控えは $want 本）ですが、控えから戻せませんでした")
      failed=1
    fi
  else
    log "crontab は正常（daytrade の行 ${have:-0} 本 / 控え ${want:-0} 本）"
  fi
else
  log "[warn] state/crontab.good が無いので crontab の点検を飛ばす（deploy/install-crontab.sh で作られる）"
fi

# --- 2. 寄る前の点検（8:42 の回だけ）-----------------------------------------
hour="${GUARD_HOUR:-$(TZ=Asia/Tokyo date +%H)}"   # GUARD_HOUR は試験用
if [ "$((10#$hour))" -lt 9 ]; then
  run_preflight() {
    PREFLIGHT_NO_ALERT=1 "$DAYTRADE" preflight --config-dir "$CFG" 2>&1
  }
  out=$(run_preflight); rc=$?
  if [ "$rc" -eq 0 ]; then
    log "寄る前の点検: 問題なし"
  else
    codes=$(printf '%s\n' "$out" | sed -n 's/^preflight-problems: //p' | tail -1)
    # 実行ファイルが動かない（無い・実行できない・落ちた）ときはコードが出ない。設定の問題として扱う
    [ -n "$codes" ] || codes="config"
    log "[problem] 寄る前の点検が落ちた（rc=$rc, codes=$codes）"
    summary+=("寄る前の点検で問題: $codes")

    if [[ ",$codes," == *,config,* ]]; then
      log "実行ファイルを作り直す（deploy/build.sh）"
      if "$HOME_DIR/deploy/build.sh" >> "$GUARD_LOG" 2>&1; then
        summary+=("実行ファイルを作り直しました")
      else
        summary+=("実行ファイルの作り直しに失敗しました")
      fi
      out=$(run_preflight); rc=$?
      if [ "$rc" -ne 0 ] && printf '%s\n' "$out" | grep -q 'problems:.*config\|strict mode\|unknown field'; then
        log "作り直してもまだ設定を読めない → 1 世代前へ戻す"
        if "$HOME_DIR/deploy/rollback-bin.sh" >> "$GUARD_LOG" 2>&1; then
          summary+=("1 世代前の実行ファイルへ戻しました")
        else
          summary+=("1 世代前へ戻せませんでした")
        fi
        out=$(run_preflight); rc=$?
      fi
    fi
    if [ "$rc" -ne 0 ] && [[ ",$(printf '%s\n' "$out" | sed -n 's/^preflight-problems: //p' | tail -1)," == *,plan,* ]]; then
      log "今日の plan を作る（daytrade plan --if-missing）"
      WITH_LOCK_TIMEOUT=300 "$HOME_DIR/deploy/with-lock.sh" /tmp/daytrade.lock 30 "$HOME_DIR/state/logs/daytrade-plan.log" \
        "$DAYTRADE" plan --if-missing --config-dir "$CFG" >> "$GUARD_LOG" 2>&1
      out=$(run_preflight); rc=$?
      [ "$rc" -eq 0 ] && summary+=("plan を作りました")
    fi

    if [ "$rc" -eq 0 ]; then
      summary+=("→ 復旧しました（再点検は問題なし）")
    else
      failed=1
      summary+=("→ 自動では直せません。残りの問題:")
      summary+=("$(printf '%s\n' "$out" | grep '^- ' || printf '%s' "$out" | tail -3)")
    fi
  fi
fi

# --- 通知 -------------------------------------------------------------------
if [ "${#summary[@]}" -gt 0 ]; then
  title="デイトレ: 自動復旧（寄る前）"
  [ "$failed" -eq 1 ] && title="デイトレ: 自動復旧できません（寄る前）"
  notify "$title" "$(printf '%s\n' "${summary[@]}")"
fi
exit "$failed"
