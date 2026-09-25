#!/usr/bin/env bash
# 寄り付きの記録（daytrade snap）と open が、その朝ちゃんと回ったかを点検して
# Discord に 1 通投げる。板は過去に遡れないので、**欠けたその日のうちに気づく**ためのもの。
#
#   deploy/morning-check.sh            # cron 用（9:22）。結果を必ず 1 通投げる
#   QUIET=1 deploy/morning-check.sh    # 問題が**無ければ**投げない
#   NO_POST=1 deploy/morning-check.sh  # 何があっても投げない（試験用）
#
# 終了コード: 0 = 問題なし・1 = 問題あり・2 = WBJP_ENV 未設定・3 = Discord に送れなかった（末尾の説明）
#
# 試験は必ず NO_POST=1 で回すこと。QUIET=1 は「問題が 0 件のときだけ黙る」ので、
# 開発中に問題を出しながら試すと毎回 Discord に飛ぶ（2026-09-11 に 6 通飛ばした）。
#
# 判断はしない・直さない。読んで人が気づくためだけの通知（docs/FEEDBACK.md の線引き）。
# night-repair（6:00）はダイジェストの異常を見るが、こちらは「回るはずの回数が回ったか」を見る
# ——成功したまま回数が足りない（cron が動かない・ロックで見送り）を拾えるのはこちら。
set -uo pipefail

HOME_DIR="${WBJP_HOME:-/home/abobo/jstock-go}"
cd "$HOME_DIR" || exit 1

TODAY="$(TZ=Asia/Tokyo date +%F)"
BOOK_DIR="$HOME_DIR/state/daytrade/history/book"
SNAP_LOG="$HOME_DIR/state/logs/daytrade-snap.log"
# open が本番（--live）だったか dry-run だったかは、**crontab ではなくその朝の事実**で決める。
# 構造化ログの daytrade.config に extra.live が残っているので、そこから読む。
#
# 以前は `crontab -l | grep -v … | grep -q …--live` で決めていた。`grep -q` は一致した時点で
# 終了するので、負荷が高くて上流がまだ書いている途中だと上流が SIGPIPE で死に、pipefail の
# せいで「一致しているのに失敗」になる。本番の朝に dry-run（9/14 で止まったログ）を数え、
# 完了 0・見送り 0 で誤報を出した（2026-09-16。~/obsidian-vault/30-projects/daytrade-morning-check-open-log.md）。
#
# ログの ts_utc は UTC。**8:59:45（寄る前）の open は UTC では前日の 23:59** なので、
# UTC の日付で絞ると寄る前の回が丸ごと漏れる。jq の jst_day で JST に直してから比べる
# （下の「判断の経過」の deploy/open-pipeline.jq と同じ前提）。
JSONL="$HOME_DIR/state/logs/daytrade-prod.jsonl"
OPEN_LOG_LIVE="$HOME_DIR/state/logs/daytrade-open.log"
OPEN_LOG_DRYRUN="$HOME_DIR/state/logs/daytrade-open-dryrun.log"
open_live=""
if [ -f "$JSONL" ]; then
  open_live=$(jq -sr --arg d "$TODAY" '
    def jst_day: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 + 32400 | strftime("%Y-%m-%d");
    [ .[] | select(.command == "open" and .code == "daytrade.config"
                   and ((.ts_utc | jst_day) == $d)) | .extra.live ]
    | if length == 0 then "" elif any then "true" else "false" end' "$JSONL" 2>/dev/null)
fi
case "$open_live" in
  true)
    OPEN_LOG="$OPEN_LOG_LIVE";   OPEN_LABEL="open（本番）";     OPEN_WHY="構造化ログの live=true" ;;
  false)
    OPEN_LOG="$OPEN_LOG_DRYRUN"; OPEN_LABEL="dry-run の open"; OPEN_WHY="構造化ログの live=false" ;;
  *)
    # 今日の daytrade.config が無い（cron が動かなかった・config を残す前の版）。
    # 次善の事実として、今日の行があるログの方を選ぶ。どちらも無ければ本番
    live_lines=$(grep -c "$TODAY" "$OPEN_LOG_LIVE" 2>/dev/null | head -1)
    dry_lines=$(grep -c "$TODAY" "$OPEN_LOG_DRYRUN" 2>/dev/null | head -1)
    if [ "${dry_lines:-0}" -gt "${live_lines:-0}" ]; then
      OPEN_LOG="$OPEN_LOG_DRYRUN"; OPEN_LABEL="dry-run の open"
    else
      OPEN_LOG="$OPEN_LOG_LIVE";   OPEN_LABEL="open（本番）"
    fi
    OPEN_WHY="構造化ログに $TODAY の open なし。今日の行が多い方（本番 ${live_lines:-0} 行 / dry-run ${dry_lines:-0} 行）"
    ;;
esac

if [ -f "$HOME_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$HOME_DIR/.env"
  set +a
fi
# 口座は明示させる（Go の既定は uat。docs/DEPLOY.md「cron の環境」）。cron の行は WBJP_ENV=prod を渡す
if [ -z "${WBJP_ENV:-}" ]; then
  echo "WBJP_ENV が未設定です。prod か uat を明示してください（例: WBJP_ENV=prod NO_POST=1 $0）" >&2
  exit 2
fi
export WBJP_ENV
export JQUANTS_READ_BUDGET_MB="${JQUANTS_READ_BUDGET_MB:-2048}"
export WBJP_DUCKDB_MEMORY_LIMIT="${WBJP_DUCKDB_MEMORY_LIMIT:-3GB}"

POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"
QUERY_BIN="${QUERY_BIN:-$HOME_DIR/bin/jquants}"

# --- 休場日なら何もしない -------------------------------------------------------
#
# cron は平日（1-5）にしか回らないので土日は来ないが、**祝日は来る**。snap も open も
# 休場日は skipHoliday で何もしないので、点検すれば「板が 1 件も無い」で必ず ❌ になる。
# 2026-09-21〜23 のような平日の 3 連休で 3 日続けて誤報が飛ぶ。
#
# 判定は Go と同じ取引カレンダー（J-Quants の HolDiv、1=営業日 2=半日）。カレンダーを
# 引けないときは**点検を続ける**——黙って飛ばすと、本当に板が欠けた日まで見逃す。
holdiv=$("$QUERY_BIN" query --limit 0 "
  SELECT HolDiv FROM read_parquet('$HOME_DIR/data/jquants/markets_calendar/*.parquet')
  WHERE Date = DATE '$TODAY'" 2>/dev/null | tail -1 | tr -d ' ')
case "$holdiv" in
  0|3)
    echo "$TODAY は休場日（HolDiv=$holdiv）。点検しません"
    exit 0
    ;;
esac

# 朝に**必ず**回るはずの snap の時刻帯（cron と同じ。15:00 / 15:19 は引け後なのでここでは見ない）
#
# 寄り直前の全銘柄の板（2026-09-25 から寄る前の open が撮る候補の外。slot は撮り始めた時刻 0859xx）はここに入れない。
# 記録だけで、失敗しても open は発注を続ける設計なので、必須にすると発注が優先されただけの日に誤報が出る。
# 下の集計表には出るので、取れた日は読める。
# 逆に古いスロットを残しても誤報になる——085945 は 2026-09-19 に 085935 / 085955 へ分け、
# 2026-09-24 に 085943 の 1 回へまとめ、2026-09-25 に 085940 へ前倒し・同日 085942 へ、同日夜に open の中へ畳んだ（残すと毎営業日「記録がありません」を出す）。cron を触ったらここも直す
# 2026-09-25 に 0830・0845・0855・0857 をやめた。085930（8:59:30）はロックを待たない回なので入れない（上と同じ理由）
EXPECTED_SLOTS=(0859 0900 0902 0905 0911)

problems=0
body="$(mktemp)"
trap 'rm -f "$body"' EXIT

{
  echo "**寄り付きの記録 $TODAY**"
  echo

  # --- 板（snap）--------------------------------------------------------------
  files=$(find "$BOOK_DIR" -maxdepth 1 -name "${TODAY}T*.parquet" 2>/dev/null | wc -l | tr -d ' ')
  if [ "$files" = "0" ]; then
    echo "❌ 板が 1 件も記録されていません（$BOOK_DIR に $TODAY のファイルなし）"
    problems=$((problems + 1))
  else
    # slot ごとの行数と、板（pGAP1）が入っている銘柄数
    summary=$("$QUERY_BIN" query --limit 0 "
      SELECT slot,
             count(*) AS 銘柄,
             sum(CASE WHEN \"pGAP1\" IS NOT NULL AND \"pGAP1\" <> '' THEN 1 ELSE 0 END) AS 板あり,
             sum(CASE WHEN \"pIEP\" IS NOT NULL AND \"pIEP\" <> '' THEN 1 ELSE 0 END) AS 予想約定
      FROM read_parquet('$BOOK_DIR/${TODAY}T*.parquet', union_by_name=true)
      GROUP BY slot ORDER BY slot" 2>&1)
    echo '```'
    echo "$summary"
    echo '```'
    for slot in "${EXPECTED_SLOTS[@]}"; do
      if ! grep -q "^$slot[[:space:]]\|[[:space:]]$slot[[:space:]]" <<<"$summary"; then
        echo "❌ slot $slot の記録がありません"
        problems=$((problems + 1))
      fi
    done
  fi

  # --- open -------------------------------------------------------------------
  #
  # 起動・完了・見送りは構造化ログから数える。下の「判断の経過」と材料を 1 本に揃えるため。
  # テキストのログは [error] と lock_busy（with-lock.sh が書く。JSONL には残らない）にだけ使う
  echo
  echo "読んだログ: $OPEN_LOG（$OPEN_WHY）"
  attempts=0; runs=0; skips=0; counted=1
  if [ -f "$JSONL" ]; then
    # 起動 = その日の open の run_id の数、完了 = daytrade.run のあった回、
    # 見送り = daytrade.skip のあった回（危険信号・候補なし等。2026-09-14 は 4 回とも見送り）
    if counts=$(jq -sr --arg d "$TODAY" '
      def jst_day: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 + 32400 | strftime("%Y-%m-%d");
      [ .[] | select(.command == "open" and ((.ts_utc | jst_day) == $d)) ]
      | group_by(.run_id)
      | [ length,
          (map(select(any(.[]; .code == "daytrade.run")))  | length),
          (map(select(any(.[]; .code == "daytrade.skip"))) | length) ]
      | @tsv' "$JSONL" 2>/dev/null); then
      [ -n "$counts" ] && read -r attempts runs skips <<<"$counts"
    else
      # jq -s はファイル全体を一括で読むので、1 行でも壊れていれば何も数えられない。
      # それを「0 回」と読むと「open が 1 回も動いていません」と誤報する（9b250ff と同じ型）
      counted=0
    fi
  else
    # JSONL が退避・ローテートされた朝や、構造化ログを書かない env。これも 0 回と読むと同じ誤報
    counted=0
  fi
  # grep -c は 0 件のとき「0」を出して終了コード 1 を返す。`|| echo 0` を足すと
  # 0 が 2 行になるので、成否は無視して出力だけ取る
  count() { grep -c "$@" 2>/dev/null | head -1; }
  # 時刻待ちの見送り = deploy/wait-until.sh が後ろの open を起こさずに終わった回（時刻が空・読めない・
  # 120 秒より先）。with-lock.sh にも daytrade にも届かないので、構造化ログには回そのものが現れず、
  # 「起動」の数にも入らない——他の回が動いていれば attempts = 0 の ❌ にも掛からない。残るのは
  # wait-until.sh がこのログに書く [error] [wait_skip] の 1 行だけなので、エラーとは分けて数えて名指しする
  errs=0; busy=0; waitskips=0
  if [ -f "$OPEN_LOG" ]; then
    waitskips=$(count "$TODAY.*\[wait_skip\]" "$OPEN_LOG")
    errs=$(grep "$TODAY.*\[error\]" "$OPEN_LOG" 2>/dev/null | grep -vc "\[wait_skip\]" | head -1)
    busy=$(count "$TODAY.*lock_busy" "$OPEN_LOG")
  else
    echo "※ $OPEN_LOG がありません（エラーとロック見送りは数えられません）"
  fi
  # 構造化ログから今日の回を数えられなかった朝は、テキストのログで数え直す。材料は粗い
  # （起動は完了・見送り・ロック見送りの和で代用）が、0 回と読んで「open が 1 回も動いて
  # いません」と誤報するよりは事実に近い（2026-09-16 のレビュー）。
  #
  # jq の失敗・JSONL 不在（counted = 0）だけでなく、**jq は成功したが今日の open の行が
  # 1 つも無い**（attempts = 0）も同じ扱いにする。open が別 env の JSONL に出た朝・JSONL を
  # 空で置き直した朝は jq が素直に 0 回を返すので、counted だけ見ていると誤報が残る。しかも
  # OPEN_LOG は「今日の行が多い方」で選んでいるので、今日の行があるログを選んでおきながら
  # 「1 回も動いていません」と言うことになる（2026-09-17 のレビュー。同じ型の誤報の 4 度目）
  if { [ "$counted" = "0" ] || [ "$attempts" = "0" ]; } && [ -f "$OPEN_LOG" ]; then
    runs=$(count "$TODAY.*daytrade.run" "$OPEN_LOG")
    skips=$(count "$TODAY.*\[daytrade.skip\]" "$OPEN_LOG")
    attempts=$((runs + skips + busy))
    # テキストのログにも今日の回が無ければ、数え直しは何も足さない。counted はそのままにして
    # 「数えられなかった（0）」と「本当に 1 回も動いていない（1）」の区別を保つ
    if [ "$attempts" != "0" ]; then
      counted=2
      echo "※ 構造化ログに $TODAY の open が無いので、回数はテキストのログ（$OPEN_LOG）で数えました"
    fi
  fi
  echo "$OPEN_LABEL: 起動 $attempts 回 / 完了 $runs 回 / 見送り $skips 回 / エラー $errs 件 / ロック見送り $busy 件 / 時刻待ちの見送り $waitskips 件"
  if [ "$skips" != "0" ] && [ -f "$JSONL" ]; then
    jq -sr --arg d "$TODAY" '
      def jst_day: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 + 32400 | strftime("%Y-%m-%d");
      [ .[] | select(.command == "open" and .code == "daytrade.skip"
                     and ((.ts_utc | jst_day) == $d)) | .msg ]
      | group_by(.) | map("  \(length) \(.[0])") | .[]' "$JSONL" 2>/dev/null
  fi
  [ "$errs" != "0" ] && problems=$((problems + 1))
  if [ "${waitskips:-0}" != "0" ]; then
    echo "❌ wait-until.sh が open の起動を $waitskips 回見送りました（crontab の DT_OPEN_AT / DT_PREOPEN_AT が空・不正・120 秒より先。その回は起動していません）"
    problems=$((problems + 1))
  fi
  if [ "$counted" = "0" ]; then
    echo "❌ open の回数を数えられませんでした（構造化ログを読めず、$OPEN_LOG もありません）"
    problems=$((problems + 1))
  elif [ "$attempts" = "0" ]; then
    echo "❌ open が 1 回も動いていません（cron が動いていない・ロックで見送り）"
    problems=$((problems + 1))
  elif [ "$runs" = "0" ] && [ "$skips" = "0" ]; then
    echo "❌ open は $attempts 回動きましたが、1 回も判断まで進んでいません（途中で止まった）"
    problems=$((problems + 1))
  fi
  if [ -f "$OPEN_LOG" ]; then
    tail_warn=$(grep "$TODAY" "$OPEN_LOG" 2>/dev/null | grep -E "\[error\]|\[warn\]" | tail -5)
    if [ -n "$tail_warn" ]; then
      echo '```'
      echo "$tail_warn"
      echo '```'
    fi
  fi

  # --- open の判断の経過と気配の鮮度（本番でしか出ない数字）-----------------------------------------
  #
  # tDPP:T は「現在値時刻」＝最後に約定した時刻で、板の時刻ではない。約定の薄い銘柄は
  # 板が生きていても数分前の約定時刻を返すので stale に落ちる（2026-09-11 の朝で 2 割）。
  # 加えて秒が返らず分単位なので、年齢は最大 59 秒ぶん多く出る。数字はこの 2 つを
  # 承知のうえで読むこと（docs/OPENING_DATA.md「実機で確かめること」）。
  #
  # 未来の件数は別の穴。寄り前の銘柄が前日の 15:30 を返すと「今日の 15:30」＝未来になり、
  # 鮮度の検査を素通りする（FutureStamped は数えるだけで除外しない）。
  if [ -f "$JSONL" ]; then
    # 回（run_id）ごとに、気配の内訳（受信・除外・板で残す・未来・使えた）、前夜の米国の値、
    # 結末と理由、発注した回は選んだ銘柄とその上で外れた銘柄の理由。読み方は deploy/open-pipeline.jq
    freshness=$(jq -sr --arg d "$TODAY" -f "$HOME_DIR/deploy/open-pipeline.jq" "$JSONL" 2>&1)
    if [ -n "$freshness" ]; then
      echo
      echo "$OPEN_LABEL の判断の経過（除外 = 鮮度で落とした / 板で残す = 除外していない）:"
      echo '```'
      echo "$freshness"
      echo '```'
      if echo "$freshness" | grep -qE "未来 [1-9]"; then
        echo "※ 未来の時刻を持つ気配がある。寄り前に前日の 15:30 が返っている可能性。"
        echo "  この数が寄り後（9:04 以降）も減らないなら、--live を開ける前に塞ぐこと"
      fi
    fi
  fi

  # --- snap のログの警告 -------------------------------------------------------
  if [ -f "$SNAP_LOG" ]; then
    warns=$(grep "$TODAY" "$SNAP_LOG" 2>/dev/null | grep -cE "\[error\]|lock_busy" | head -1)
    if [ "${warns:-0}" != "0" ]; then
      echo
      echo "snap の警告 $warns 件:"
      echo '```'
      grep "$TODAY" "$SNAP_LOG" | grep -E "\[error\]|lock_busy" | tail -5
      echo '```'
      problems=$((problems + 1))
    fi
  fi

  echo
  if [ "$problems" = "0" ]; then
    echo "問題は見つかりませんでした。"
  else
    echo "**要確認 $problems 件。** 板は遡れないので、原因の切り分けは今日のうちに。"
  fi
} > "$body"

cat "$body"

# --- 終了コード ----------------------------------------------------------------
# crontab の行は `deploy/ping.sh MORNING $?` でこの終了コードを state/ping/MORNING に残し、
# deploy/mackerel-alive.sh が alive.morning（0 以外で 1）として Mackerel に投稿する。監視ルールは
# 「alive.morning > 0 で警報」なので、**0 以外はそのまま Mackerel の警報（メール）になる**。
#
#   0 = 問題なし（または休場日）で、送るべきものは送れた
#   1 = 問題が見つかった（Discord には送れた）
#   2 = WBJP_ENV が未設定（上で止まる）
#   3 = Discord に送れなかった（discord-post が無い・失敗・時間切れ）。問題の有無より優先する
#
# 以前は最後が必ず exit 0 で、問題があっても・Discord に届かなくても Mackerel は正常のままだった
# （2026-09-25 のレビュー M1）。problems>0 も警報にする判断: 過去の運用ログでは要確認は 10 営業日に
# 1 回ほどで、どれも「板は遡れないので今日のうちに」見るべきもの。Discord と Mackerel の二重になるが、
# Discord の 1 通は流れて見落としうるので、残る側（Mackerel は当日中 1 のまま）にも出す。
# 警報は翌営業日の期限（9:40）前に 0 へ戻る——alive.morning は「今日の印」しか見ない。
# 誤報が多すぎると感じたら、ここで rc_problems=0 にすれば送信失敗だけを警報にできる。
rc_problems=1
rc=0
[ "$problems" = "0" ] || rc=$rc_problems

# NO_POST=1 は試験用。送らないので送信失敗は無く、問題の有無だけを返す（cron の行では使わない）
if [ "${NO_POST:-}" = "1" ]; then
  exit "$rc"
fi
# QUIET=1 で問題が無ければ送らない。送っていないので rc は 0 のまま
if [ "${QUIET:-}" = "1" ] && [ "$problems" = "0" ]; then
  exit 0
fi
if [ ! -x "$POST_BIN" ]; then
  echo "[error] $POST_BIN が無い（実行できない）ので Discord に送れませんでした" >&2
  exit 3
fi
# 送信が固まっても点検は終える（-k は TERM を無視されたとき）
if ! timeout -k 10 120 "$POST_BIN" --title "寄り付きの記録 $TODAY" < "$body"; then
  echo "[error] Discord への送信に失敗しました（$POST_BIN）" >&2
  exit 3
fi
exit "$rc"
