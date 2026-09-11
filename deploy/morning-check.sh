#!/usr/bin/env bash
# 寄り付きの記録（daytrade snap）と dry-run の open が、その朝ちゃんと回ったかを点検して
# Discord に 1 通投げる。板は過去に遡れないので、**欠けたその日のうちに気づく**ためのもの。
#
#   deploy/morning-check.sh            # cron 用（9:22）。結果を必ず 1 通投げる
#   QUIET=1 deploy/morning-check.sh    # 問題が**無ければ**投げない
#   NO_POST=1 deploy/morning-check.sh  # 何があっても投げない（試験用）
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
OPEN_LOG="$HOME_DIR/state/logs/daytrade-open-dryrun.log"

if [ -f "$HOME_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$HOME_DIR/.env"
  set +a
fi
export WBJP_ENV="${WBJP_ENV:-prod}"
export JQUANTS_READ_BUDGET_MB="${JQUANTS_READ_BUDGET_MB:-2048}"
export WBJP_DUCKDB_MEMORY_LIMIT="${WBJP_DUCKDB_MEMORY_LIMIT:-3GB}"

POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"
QUERY_BIN="${QUERY_BIN:-$HOME_DIR/bin/jquants}"

# 朝に回るはずの snap の時刻帯（cron と同じ。15:00 / 15:19 は引け後なのでここでは見ない）
EXPECTED_SLOTS=(0830 0845 0855 0859 0900 0902 0905 0911)

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
      if ! grep -q "^$slot\|[[:space:]]$slot[[:space:]]" <<<"$summary"; then
        echo "❌ slot $slot の記録がありません"
        problems=$((problems + 1))
      fi
    done
  fi

  # --- dry-run の open --------------------------------------------------------
  echo
  if [ -f "$OPEN_LOG" ]; then
    # grep -c は 0 件のとき「0」を出して終了コード 1 を返す。`|| echo 0` を足すと
    # 0 が 2 行になるので、成否は無視して出力だけ取る
    count() { grep -c "$@" 2>/dev/null | head -1; }
    runs=$(count "$TODAY.*daytrade.run" "$OPEN_LOG")
    errs=$(count "$TODAY.*\[error\]" "$OPEN_LOG")
    busy=$(count "$TODAY.*lock_busy" "$OPEN_LOG")
    echo "dry-run の open: 完了 $runs 回 / エラー $errs 件 / ロック見送り $busy 件"
    [ "$errs" != "0" ] && problems=$((problems + 1))
    echo '```'
    grep "$TODAY" "$OPEN_LOG" | grep -E "\[error\]|\[warn\]" | tail -5
    echo '```'
  else
    echo "❌ $OPEN_LOG がありません（cron が動いていない）"
    problems=$((problems + 1))
  fi

  # --- 気配の鮮度（本番でしか出ない数字）-----------------------------------------
  #
  # tDPP:T は「現在値時刻」＝最後に約定した時刻で、板の時刻ではない。約定の薄い銘柄は
  # 板が生きていても数分前の約定時刻を返すので stale に落ちる（2026-09-11 の朝で 2 割）。
  # 加えて秒が返らず分単位なので、年齢は最大 59 秒ぶん多く出る。数字はこの 2 つを
  # 承知のうえで読むこと（docs/OPENING_DATA.md「実機で確かめること」）。
  #
  # 未来の件数は別の穴。寄り前の銘柄が前日の 15:30 を返すと「今日の 15:30」＝未来になり、
  # 鮮度の検査を素通りする（FutureStamped は数えるだけで除外しない）。
  JSONL="$HOME_DIR/state/logs/daytrade-prod.jsonl"
  if [ -f "$JSONL" ]; then
    # 「気配を取得」（受信数）と「使えない気配を除外」（内訳）は別の行なので、
    # 直前の取得の受信数を覚えておいて内訳の行に添える。足し算はしない——
    # future は Fresh と無関係に受信ぜんぶを走査するので stale/delayed と重なる。
    freshness=$(jq -sr --arg d "$TODAY" '
      # 9:01〜9:13 JST は同じ UTC 日付の 00:01〜00:13。朝の回だけを拾う
      [ .[] | select(.code == "daytrade.quotes" and (.ts_utc | startswith($d))) ]
      | reduce .[] as $e ({recv: null, req: null, out: []};
          if $e.extra.received != null then
            .recv = $e.extra.received | .req = $e.extra.requested
          elif $e.extra.future != null then
            .out += [ "\(($e.ts_utc[11:13] | tonumber + 9)):\($e.ts_utc[14:16]) JST  受信 \(.recv // "?")/\(.req // "?")  使えない stale \($e.extra.stale) / delayed \($e.extra.delayed) / 未来 \($e.extra.future)" ]
          else . end)
      | .out[]
    ' "$JSONL" 2>/dev/null | tail -6)
    if [ -n "$freshness" ]; then
      echo
      echo "気配の鮮度（dry-run の open）:"
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

if [ "${NO_POST:-}" = "1" ]; then
  exit 0
fi
if [ "${QUIET:-}" = "1" ] && [ "$problems" = "0" ]; then
  exit 0
fi
[ -x "$POST_BIN" ] && "$POST_BIN" --title "寄り付きの記録 $TODAY" < "$body"
exit 0
