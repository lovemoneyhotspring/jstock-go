#!/usr/bin/env bash
# 日次レポート: その日の運用を daily-report サブエージェントに振り返らせ、Discord に流す。
#
#   deploy/daily-report.sh              # 今日（Asia/Tokyo）ぶん
#   deploy/daily-report.sh 2026-09-02   # 日付を指定
#   DRY_RUN=1 deploy/daily-report.sh    # 生成するだけで Discord に送らない
#
# 設計の要点:
#   - レポートの生成（claude）と配達（discord-post）を分ける。モデルが
#     「送るのを忘れる」経路を無くし、配達は決め打ちのスクリプトが担う
#   - 生成に失敗しても黙って消えないよう、失敗した事実を Discord に流す
#   - 本文は state/reports/ に必ず残す。Discord に届かなくても後から読める
#   - claude には Edit / Write を渡さない（サブエージェント側でも禁止しているが二重に）

set -uo pipefail

HOME_DIR="${WBJP_HOME:-/home/abobo/jstock-go}"
cd "$HOME_DIR" || exit 1

DAY="${1:-$(TZ=Asia/Tokyo date +%F)}"
WEEKDAY="$(TZ=Asia/Tokyo date -d "$DAY" +%a)"
REPORT_DIR="$HOME_DIR/state/reports"
REPORT="$REPORT_DIR/daily-$DAY.md"
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

# cron の PATH には ~/.local/bin が入っていないので絶対パスで持つ
CLAUDE_BIN="${CLAUDE_BIN:-$HOME/.local/bin/claude}"
[ -x "$CLAUDE_BIN" ] || CLAUDE_BIN="$(command -v claude || echo "$CLAUDE_BIN")"

# 配達係（deploy/build.sh が bin/ に作る）
POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"

# ダイジェストを起動前に写し取る。エージェントが叩く `review` / `evaluate` は
# それ自体がダイジェストに 1 行足すので、生のファイルを読ませると自分の足跡を
# 「今日の運用」として数えてしまう。読ませるのは起動時点の写しに固定する。
SNAPSHOT="$REPORT_DIR/digest-$DAY.snapshot.jsonl"
cp "$HOME_DIR/state/digest/$WBJP_ENV-$DAY.jsonl" "$SNAPSHOT" 2>/dev/null || : > "$SNAPSHOT"

# 現在時刻を渡す。これが無いと「まだ実行時刻が来ていないジョブ」と「動くはず
# なのに動かなかったジョブ」を区別できず、平常運転を異常として書いてしまう。
NOW="$(TZ=Asia/Tokyo date +%H:%M)"

PROMPT="$DAY（$WEEKDAY）の運用を振り返り、日次レポートを書いてください。
基準日は $DAY、いまは $NOW JST です。この時刻より後に予定されている cron の
ジョブは、まだ動いていなくて当たり前です（異常として書かないこと）。
その日のダイジェストは $SNAPSHOT を読んでください（起動時点の写し。生の
state/digest/$WBJP_ENV-$DAY.jsonl は、あなた自身が叩いたコマンドの行が混ざります）。
標準出力にはレポート本文だけを書いてください。"

# --agent で daily-report を使う。--effort は既定のまま（十分な深さで、かつ
# 毎日回すので上げすぎない）。900 秒で打ち切る（cron が詰まるのを防ぐ）。
#
# プロンプトは**標準入力から渡す**。--disallowedTools は可変長引数なので、
# 後ろに置いたプロンプトまでツール名として飲み込んでしまう。
printf '%s' "$PROMPT" | timeout 900 "$CLAUDE_BIN" -p \
  --agent daily-report \
  --permission-mode bypassPermissions \
  --disallowedTools "Edit,Write,NotebookEdit" \
  > "$REPORT" 2> "$REPORT_DIR/daily-$DAY.err"
STATUS=${PIPESTATUS[1]}

if [ $STATUS -ne 0 ] || [ ! -s "$REPORT" ]; then
  {
    echo "**日次レポート $DAY — 生成に失敗**"
    echo "claude の終了コード: $STATUS（124 なら 15 分で時間切れ）"
    echo '```'
    tail -c 800 "$REPORT_DIR/daily-$DAY.err" 2>/dev/null
    echo '```'
    echo "サーバーで確認: \`$HOME_DIR/deploy/daily-report.sh $DAY\`"
  } | "$POST_BIN"
  exit 1
fi

if [ "${DRY_RUN:-}" = "1" ]; then
  cat "$REPORT"
  exit 0
fi

"$POST_BIN" < "$REPORT"
