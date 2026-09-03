#!/usr/bin/env bash
# 夜間自己修復: 前夜からの実行ログに異常があれば、night-repair サブエージェントに
# 原因調査とコード修正案の作成（新しいブランチ → push → PR 作成）までやらせる。
#
#   deploy/night-repair.sh                      # 通常実行（cron 用、6:00）
#   DRY_RUN=1 deploy/night-repair.sh            # 異常判定・調査はするが push・PR 作成・Discord 送信をしない
#   NIGHT_REPAIR_MODEL=sonnet deploy/night-repair.sh   # モデルを変えて試す（既定は opus）
#
# 設計の要点:
#   - 異常が無ければ claude を起動しない（前段の jq 判定。API コストを抑える）
#   - main への直接コミット・push、deploy/build.sh の実行、bin/ の書き換え、
#     crontab の変更、--live 系コマンドは night-repair サブエージェントの
#     プロンプト（.claude/agents/night-repair.md）で禁じてある。
#     deploy/build.sh・crontab の実行はツールレベルでも禁止（--disallowedTools）。
#     このスクリプト自身はそれらを一切行わない
#   - 生成（claude）と配達（discord-post）を分ける。daily-report.sh と同じ理由
#   - 本文は state/reports/ に必ず残す。Discord に届かなくても後から読める

set -uo pipefail

HOME_DIR="${WBJP_HOME:-/home/abobo/jstock-go}"
cd "$HOME_DIR" || exit 1

TODAY="$(TZ=Asia/Tokyo date +%F)"
YESTERDAY="$(TZ=Asia/Tokyo date -d yesterday +%F)"
REPORT_DIR="$HOME_DIR/state/reports"
REPORT="$REPORT_DIR/night-repair-$TODAY.md"
mkdir -p "$REPORT_DIR"

# .env の中身（WBJP_ALERT_WEBHOOK_URL など）を環境変数に載せる。cron は
# ログインシェルを通らないので、ここで読まないと通知先が分からない。
if [ -f "$HOME_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$HOME_DIR/.env"
  set +a
fi
export WBJP_ENV="${WBJP_ENV:-prod}"

# --- 異常の有無を先に判定する（claude を起動する前）--------------------------
DIGESTS=()
for d in "$YESTERDAY" "$TODAY"; do
  f="$HOME_DIR/state/digest/$WBJP_ENV-$d.jsonl"
  [ -f "$f" ] && DIGESTS+=("$f")
done

if [ ${#DIGESTS[@]} -eq 0 ]; then
  echo "ダイジェストが見つかりません（$YESTERDAY / $TODAY）。何もしません"
  exit 0
fi

ANOMALY_COUNT="$(jq -c 'select(.anomalies or .outcome == "error")' "${DIGESTS[@]}" 2>/dev/null | wc -l | tr -d ' ')"
if [ "$ANOMALY_COUNT" = "0" ]; then
  echo "異常なし（$YESTERDAY 〜 $TODAY）。claude は起動しません"
  exit 0
fi

echo "異常 $ANOMALY_COUNT 件を検知。night-repair エージェントを起動します"

# cron の PATH には ~/.local/bin が入っていないので絶対パスで持つ
CLAUDE_BIN="${CLAUDE_BIN:-$HOME/.local/bin/claude}"
[ -x "$CLAUDE_BIN" ] || CLAUDE_BIN="$(command -v claude || echo "$CLAUDE_BIN")"

# 配達係（deploy/build.sh が bin/ に作る）
POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"

NOW="$(TZ=Asia/Tokyo date +%H:%M)"

# 本番反映（deploy/build.sh・crontab 編集）は常時ブロック。プロンプト
# （.claude/agents/night-repair.md）でも禁じているが、二重に強制する。
DISALLOWED="Bash(deploy/build.sh:*),Bash(crontab:*)"

PROMPT="$TODAY（JST 今 $NOW）の運用ログに異常が $ANOMALY_COUNT 件見つかりました。
原因を調べ、直せるならコードの修正案を作ってください（.claude/agents/night-repair.md の
手順・制約に従うこと）。標準出力にはレポート本文だけを書いてください。"

if [ "${DRY_RUN:-}" = "1" ]; then
  # DRY_RUN はテスト実行なので、実際の push・PR 作成はツールレベルで
  # 強制的に止める（プロンプトの「お願い」だけに頼らない）。
  DISALLOWED="$DISALLOWED,Bash(git push:*),Bash(gh pr create:*),Bash(gh pr edit:*)"
  PROMPT="$PROMPT

これは DRY_RUN（テスト実行）です。git push と PR 作成はツール側でブロックされていて
実行できません（失敗しても気にせず続けてよい）。ブランチを切ってコミットするところまでは
試してよいですが、最後は必ず git checkout main で終えてください。レポートには
「本番実行ならここで push して PR を作成していた」という想定内容を書いてください。"
fi

# --agent で night-repair を使う。Edit/Write は許可するが、危険な操作
# （main への直接コミット・本番反映・--live 実行）はエージェント定義側の
# 禁止事項として書いてある。1800 秒（30 分）で打ち切る。
printf '%s' "$PROMPT" | timeout 1800 "$CLAUDE_BIN" -p \
  --agent night-repair \
  --model "${NIGHT_REPAIR_MODEL:-opus}" \
  --permission-mode bypassPermissions \
  --disallowedTools "$DISALLOWED" \
  > "$REPORT" 2> "$REPORT_DIR/night-repair-$TODAY.err"
STATUS=${PIPESTATUS[1]}

if [ $STATUS -ne 0 ] || [ ! -s "$REPORT" ]; then
  {
    echo "**夜間トリアージ $TODAY — 生成に失敗**"
    echo "検知した異常: $ANOMALY_COUNT 件"
    echo "claude の終了コード: $STATUS（124 なら 30 分で時間切れ）"
    echo '```'
    tail -c 800 "$REPORT_DIR/night-repair-$TODAY.err" 2>/dev/null
    echo '```'
    echo "サーバーで確認: \`$HOME_DIR/deploy/night-repair.sh\`"
  } | "$POST_BIN" --title "夜間自己修復 $TODAY"
  exit 1
fi

if [ "${DRY_RUN:-}" = "1" ]; then
  cat "$REPORT"
  exit 0
fi

"$POST_BIN" --title "夜間自己修復 $TODAY" < "$REPORT"
