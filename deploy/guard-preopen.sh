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
#
# 時間の上限: systemd の TimeoutStartSec=600 を超えると SIGTERM で止められ、通知も出せない。
# 作り直し・preflight（最大 4 回）・plan のどれにも timeout を掛け、全体を GUARD_BUDGET 秒
# （既定 500）の締め切りの内に収める。残りの時間が足りない段は飛ばし、飛ばしたことを通知する。
# 最後の通知（60 秒＋-k 10）を足しても 600 秒に届かない。それでも止められたとき（固まった
# ・OOM）は、ユニットの ExecStopPost（deploy/unit-done.sh）が「途中で止められた」を送る。
set -uo pipefail
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/lib-notify.sh"

CRONTAB="${CRONTAB:-crontab}"
CFG="${DAYTRADE_CONFIG_DIR:-config/daytrade_margin}"
DAYTRADE="$HOME_DIR/bin/daytrade"
summary=()
failed=0

# --- 締め切り ---------------------------------------------------------------
GUARD_BUDGET="${GUARD_BUDGET:-500}"
deadline=$(( $(date +%s) + GUARD_BUDGET ))
# 各段の上限（秒）。preflight はふだん 1 秒未満、作り直しは 1〜2 分
PREFLIGHT_TIMEOUT="${GUARD_PREFLIGHT_TIMEOUT:-60}"
BUILD_TIMEOUT="${GUARD_BUILD_TIMEOUT:-240}"
ROLLBACK_TIMEOUT="${GUARD_ROLLBACK_TIMEOUT:-60}"
PLAN_TIMEOUT="${GUARD_PLAN_TIMEOUT:-300}"
KILL_AFTER=10

# step_limit <上限>: 締め切りまでの残り（-k の猶予を引く）と上限の小さい方。足りなければ 0
step_limit() {
  local left=$(( deadline - $(date +%s) - KILL_AFTER ))
  local t=$1
  [ "$left" -lt "$t" ] && t=$left
  [ "$t" -lt 5 ] && t=0
  echo "$t"
}
# bounded <上限> <cmd...>: 時間を区切って実行する。時間が残っていなければ走らせず 124
bounded() {
  local t
  t=$(step_limit "$1"); shift
  if [ "$t" -eq 0 ]; then
    log "[warn] 締め切り（${GUARD_BUDGET} 秒）までの残りが足りないので飛ばす: $*"
    return 124
  fi
  timeout -k "$KILL_AFTER" "$t" "$@"
}

# --- 1. crontab ------------------------------------------------------------
# 戻すのは「jstock-go のブロックごと消えた」とき（別の内容で上書きされた・空になった）だけ。
#   - ブロックの目印（`# wbjp（`）が残っていて行だけ無効なら、人がコメントアウトして止めたとみなして
#     戻さず、通知だけ（止めるなら execution.kill_switch。crontab で止めるなら state/crontab.paused を作る）
#   - state/crontab.paused があれば何もしない
good="$HOME_DIR/state/crontab.good"
if [ -f "$HOME_DIR/state/crontab.paused" ]; then
  log "state/crontab.paused があるので crontab の点検を飛ばす"
elif [ -f "$good" ]; then
  want=$(grep -v '^[[:space:]]*#' "$good" | grep -c 'WBJP_BIN/daytrade' || true)
  current=$("$CRONTAB" -l 2>/dev/null || true)
  have=$(printf '%s\n' "$current" | grep -v '^[[:space:]]*#' | grep -c 'WBJP_BIN/daytrade' || true)
  if [ "${want:-0}" -gt 0 ] && [ "${have:-0}" -lt $((want / 2)) ]; then
    if printf '%s\n' "$current" | grep -q '^# wbjp（'; then
      log "[warn] crontab の daytrade の行が有効なのは $have 本（控えは $want 本）だが、ブロックの目印は残っている。止めたとみなして戻さない"
      summary+=("crontab の daytrade の行が有効なのは $have 本（控えは $want 本）です。ブロックの目印は残っているので、止めた状態とみなして戻していません。意図した停止でなければ deploy/install-crontab.sh で入れ直してください（止めておくなら state/crontab.paused を作る）")
    else
      mkdir -p "$HOME_DIR/state/backup/crontab"
      printf '%s\n' "$current" > "$HOME_DIR/state/backup/crontab/crontab-$(date +%Y%m%d-%H%M%S)-before-restore.txt"
      if "$CRONTAB" -n "$good" >/dev/null 2>&1 && "$CRONTAB" "$good"; then
        summary+=("crontab から jstock-go のブロックが消えていた（daytrade の行 $have 本 / 控え $want 本）ので、控えから戻しました")
        log "[restore] crontab を控えから戻した（$have → $want 本）"
      else
        summary+=("crontab から jstock-go のブロックが消えていましたが、控えから戻せませんでした")
        failed=1
      fi
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
    bounded "$PREFLIGHT_TIMEOUT" env PREFLIGHT_NO_ALERT=1 "$DAYTRADE" preflight --config-dir "$CFG" 2>&1
  }
  # 時間切れ（124）・-k の KILL（137）は「実行ファイルが起動しない」とは分けて扱う。固まるのは
  # たいてい外（ネット・ロック・ディスク）で、作り直しや 1 世代前へ戻しても直らない
  timed_out() { [ "$1" -eq 124 ] || [ "$1" -eq 137 ]; }
  out=$(run_preflight); rc=$?
  if [ "$rc" -eq 0 ]; then
    log "寄る前の点検: 問題なし"
  else
    codes=$(printf '%s\n' "$out" | sed -n 's/^preflight-problems: //p' | tail -1)
    if [ -z "$codes" ] && timed_out "$rc"; then
      codes="timeout"
    fi
    # 実行ファイルが動かない（無い・実行できない・落ちた）ときはコードが出ない。設定の問題として扱う
    [ -n "$codes" ] || codes="config"
    log "[problem] 寄る前の点検が落ちた（rc=$rc, codes=$codes）"
    summary+=("寄る前の点検で問題: $codes")
    if [ "$codes" = "timeout" ]; then
      summary+=("preflight が ${PREFLIGHT_TIMEOUT} 秒で終わりませんでした。作り直し・1 世代前へ戻すことはしません（固まる原因は実行ファイルの外にあることが多い）")
    fi

    if [[ ",$codes," == *,config,* ]]; then
      # 作り直すのは、作業ツリーが**コミット済みの main** のときだけ。未コミット・別ブランチのコードから
      # 作った実行ファイルで 8:59:45 の open を動かさない（誤発注の入口になる）。そうでなければ
      # 作り直さず、1 世代前へ戻す側に進む
      # 対象はビルドに効くもの（.go・go.mod・go.sum・設定）。追跡されていない新規ファイルも数える
      # （go build は未追跡の .go も取り込むので、git に無いコードから作ってしまう）
      dirty=$(git -C "$HOME_DIR" status --porcelain -- '*.go' go.mod go.sum config 2>/dev/null | wc -l)
      branch=$(git -C "$HOME_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "?")
      if [ "$dirty" -eq 0 ] && [ "$branch" = "main" ]; then
        log "実行ファイルを作り直す（deploy/build.sh）"
        bounded "$BUILD_TIMEOUT" "$HOME_DIR/deploy/build.sh" >> "$GUARD_LOG" 2>&1; brc=$?
        if [ "$brc" -eq 0 ]; then
          summary+=("実行ファイルを作り直しました")
        elif timed_out "$brc"; then
          summary+=("実行ファイルの作り直しが時間内に終わりませんでした（rc=$brc。上限 ${BUILD_TIMEOUT} 秒・締め切り ${GUARD_BUDGET} 秒）")
        else
          summary+=("実行ファイルの作り直しに失敗しました")
        fi
        out=$(run_preflight); rc=$?
      else
        # 作り直しも戻しもしない。設定が新しいとき、古い実行ファイルへ戻すと悪化する
        # （古い実行ファイルは新しい項目をもっと読めない）。人が deploy/build.sh を実行するまで待つ
        summary+=("作業ツリーが未コミットか main でない（変更 ${dirty} 件 / ブランチ $branch）ため、自動では何もしません。コミットして deploy/build.sh を実行してください")
        log "[warn] 作業ツリーが main でクリーンでないので build.sh も rollback も走らせない"
        skip_repair=1
      fi
      # まだ動かない: 起動しない（コードが出ない）か、設定を読めない → 1 世代前へ戻す
      if [ "$rc" -ne 0 ] && [ "${skip_repair:-0}" -eq 0 ]; then
        after=$(printf '%s\n' "$out" | sed -n 's/^preflight-problems: //p' | tail -1)
        if [ -z "$after" ] || [[ ",$after," == *,config,* ]]; then
          log "まだ動かない → 1 世代前へ戻す"
          if bounded "$ROLLBACK_TIMEOUT" "$HOME_DIR/deploy/rollback-bin.sh" >> "$GUARD_LOG" 2>&1; then
            summary+=("1 世代前の実行ファイルへ戻しました")
            out=$(run_preflight); rc=$?
            back=$(printf '%s\n' "$out" | sed -n 's/^preflight-problems: //p' | tail -1)
            # 戻しても設定を読めない・起動しないなら、戻す前（作業ツリーの main から作った版）へ進め直す。
            # どちらも動かないなら、人が deploy/build.sh で入れた版のままにしておく方が後で追いやすい
            # （古い版を黙って置いておくと、次の build まで「いつの版か」が分からなくなる）。
            # rollback-bin.sh は戻す前の版を .prev に入れ替えて残すので、もう一度呼べば進む。
            # plan など設定以外だけが残るなら、戻した版は設定を読めているので残す
            if [ "$rc" -ne 0 ] && { [ -z "$back" ] || [[ ",$back," == *,config,* ]]; }; then
              log "戻しても動かない → 戻す前の実行ファイルへ進め直す"
              if bounded "$ROLLBACK_TIMEOUT" "$HOME_DIR/deploy/rollback-bin.sh" >> "$GUARD_LOG" 2>&1; then
                summary+=("1 世代前でも動かなかったので、戻す前の実行ファイルへ進め直しました（どちらの版も点検に通りません）")
              else
                summary+=("1 世代前でも動かず、戻す前の実行ファイルへ進め直すのにも失敗しました。bin/ は 1 世代前の版のままです")
              fi
            fi
          else
            summary+=("1 世代前へ戻せませんでした")
          fi
        fi
      fi
    fi
    if [ "$rc" -ne 0 ] && [[ ",$(printf '%s\n' "$out" | sed -n 's/^preflight-problems: //p' | tail -1)," == *,plan,* ]]; then
      # with-lock.sh は自分で timeout を掛ける（ロック待ち 30 秒＋上限＋-k の 10 秒）。締め切りから逆算する
      plan_limit=$(step_limit $((PLAN_TIMEOUT + 30)))
      plan_limit=$((plan_limit - 30))
      if [ "$plan_limit" -ge 30 ]; then
        log "今日の plan を作る（daytrade plan --if-missing、上限 ${plan_limit} 秒）"
        WITH_LOCK_TIMEOUT="$plan_limit" "$HOME_DIR/deploy/with-lock.sh" /tmp/daytrade.lock 30 "$HOME_DIR/state/logs/daytrade-plan.log" \
          "$DAYTRADE" plan --if-missing --config-dir "$CFG" >> "$GUARD_LOG" 2>&1
        out=$(run_preflight); rc=$?
        [ "$rc" -eq 0 ] && summary+=("plan を作りました")
      else
        log "[warn] 締め切りまでの残りが足りないので plan を作らない"
        summary+=("締め切り（${GUARD_BUDGET} 秒）までの残りが足りず、plan を作れませんでした")
      fi
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
