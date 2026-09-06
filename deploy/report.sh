#!/usr/bin/env bash
# 運用レポート: その期間を claude のサブエージェントに振り返らせ、Discord に流す。
#
#   deploy/report.sh daily                # 今日（Asia/Tokyo）
#   deploy/report.sh daily 2026-09-02     # 日付を指定
#   deploy/report.sh weekly               # 今週（月〜金）
#   deploy/report.sh weekly 2026-09-02    # その日を含む週
#   deploy/report.sh monthly              # 前月（1 日に cron が回す前提）
#   deploy/report.sh monthly 2026-08      # その月
#   DRY_RUN=1 deploy/report.sh weekly     # 生成するだけで Discord に送らない・vault にも入れない
#
# 設計の要点（3 期間で共通）:
#   - レポートの生成（claude）と配達（discord-post）を分ける。モデルが
#     「送るのを忘れる」経路を無くし、配達は決め打ちのスクリプトが担う
#   - 生成に失敗しても黙って消えないよう、失敗した事実を Discord に流す
#   - 本文は state/reports/ に必ず残す。Discord に届かなくても後から読める
#   - 週次・月次は Obsidian の vault（50-reports/）にも写して commit する。記録の本体は vault
#   - claude には Edit / Write を渡さない（サブエージェント側でも禁止しているが二重に）
#
# 日次だけ前提が違う（1 日ぶんのダイジェストの写しを渡す）ので、そこだけ分岐する。
# 週次・月次は追記専用の履歴（state/*/history、Parquet）とダイジェストを期間で読む
# ——日次レポートの .md には依存しない（保持期間より長い期間を振り返れるように）。

set -uo pipefail

PERIOD="${1:-daily}"
ARG="${2:-}"

HOME_DIR="${WBJP_HOME:-/home/abobo/jstock-go}"
cd "$HOME_DIR" || exit 1

REPORT_DIR="$HOME_DIR/state/reports"
mkdir -p "$REPORT_DIR"

# .env の中身（WBJP_DISCORD_BOT_TOKEN・チャンネル ID など）を環境変数に載せる。cron は
# ログインシェルを通らないので、ここで読まないと通知先が分からない。
if [ -f "$HOME_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$HOME_DIR/.env"
  set +a
fi
export WBJP_ENV="${WBJP_ENV:-prod}"

# メモリの上限（deploy/crontab.txt と同じ値）。claude が子プロセスで daytrade / jquants を
# 叩くので、ここで export しないと上限の無いまま DuckDB がシステムメモリの 80% を取りに行く
export JQUANTS_READ_BUDGET_MB="${JQUANTS_READ_BUDGET_MB:-2048}"
export GOMEMLIMIT="${GOMEMLIMIT:-4GiB}"
export GOGC="${GOGC:-400}"
export WBJP_DUCKDB_MEMORY_LIMIT="${WBJP_DUCKDB_MEMORY_LIMIT:-3GB}"

# cron の PATH には ~/.local/bin が入っていないので絶対パスで持つ
CLAUDE_BIN="${CLAUDE_BIN:-$HOME/.local/bin/claude}"
[ -x "$CLAUDE_BIN" ] || CLAUDE_BIN="$(command -v claude || echo "$CLAUDE_BIN")"

# 配達係（deploy/build.sh が bin/ に作る）
POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"

jst() { TZ=Asia/Tokyo date "$@"; }

# 期間ごとに: 対象範囲（FROM〜TO）、見出し、出力ファイル、使うエージェントを決める
case "$PERIOD" in
  daily)
    FROM="${ARG:-$(jst +%F)}"
    TO="$FROM"
    TITLE="日次レポート $FROM（$(jst -d "$FROM" +%a)）"
    REPORT="$REPORT_DIR/daily-$FROM.md"
    AGENT="daily-report"
    ;;
  weekly)
    BASE="${ARG:-$(jst +%F)}"
    # その日を含む週の月曜〜金曜（日曜に回しても前の月曜から数える）
    FROM="$(jst -d "$BASE -$(( ($(jst -d "$BASE" +%u) + 6) % 7 )) days" +%F)"
    TO="$(jst -d "$FROM +4 days" +%F)"
    TITLE="週次レポート $FROM〜$TO"
    REPORT="$REPORT_DIR/weekly-$FROM.md"
    AGENT="periodic-report"
    ;;
  monthly)
    if [ -z "$ARG" ]; then
      MONTH="$(jst -d "$(jst +%Y-%m-01) -1 month" +%Y-%m)"   # 既定は前月
    else
      MONTH="$(jst -d "${ARG}-01" +%Y-%m 2>/dev/null || jst -d "$ARG" +%Y-%m)"
    fi
    FROM="$MONTH-01"
    TO="$(jst -d "$FROM +1 month -1 day" +%F)"
    TITLE="月次レポート $MONTH"
    REPORT="$REPORT_DIR/monthly-$MONTH.md"
    AGENT="periodic-report"
    ;;
  *)
    echo "使い方: $0 <daily|weekly|monthly> [基準日]" >&2
    exit 2
    ;;
esac

NOW="$(jst +%F' '%H:%M)"

if [ "$PERIOD" = "daily" ]; then
  # ダイジェストを起動前に写し取る。エージェントが叩く `review` / `evaluate` は
  # それ自体がダイジェストに 1 行足すので、生のファイルを読ませると自分の足跡を
  # 「今日の運用」として数えてしまう。読ませるのは起動時点の写しに固定する。
  SNAPSHOT="$REPORT_DIR/digest-$FROM.snapshot.jsonl"
  cp "$HOME_DIR/state/digest/$WBJP_ENV-$FROM.jsonl" "$SNAPSHOT" 2>/dev/null || : > "$SNAPSHOT"

  # 現在時刻を渡す。これが無いと「まだ実行時刻が来ていないジョブ」と「動くはず
  # なのに動かなかったジョブ」を区別できず、平常運転を異常として書いてしまう。
  PROMPT="$FROM（$(jst -d "$FROM" +%a)）の運用を振り返り、日次レポートを書いてください。
基準日は $FROM、いまは ${NOW#* } JST です。この時刻より後に予定されている cron の
ジョブは、まだ動いていなくて当たり前です（異常として書かないこと）。
その日のダイジェストは $SNAPSHOT を読んでください（起動時点の写し。生の
state/digest/$WBJP_ENV-$FROM.jsonl は、あなた自身が叩いたコマンドの行が混ざります）。
標準出力にはレポート本文だけを書いてください。"
else
  PROMPT="$TITLE を書いてください。対象期間は $FROM〜$TO（両端を含む、Asia/Tokyo）、
いまは $NOW JST です。期間の種類は $PERIOD です。
期間で読める記録（state/*/history の Parquet、state/digest/$WBJP_ENV-<日付>.jsonl、
state/notify/<日付>.jsonl）と、bin/*  の review / evaluate に --from $FROM --to $TO を
付けて集計してください。日次レポートの .md には依存しないこと（保持期間が短い）。
標準出力にはレポート本文だけを書いてください。"
fi

# --agent でエージェントを選ぶ。--effort は既定のまま（十分な深さで、かつ
# 定期的に回すので上げすぎない）。時間切れで cron が詰まるのを防ぐ。
# 期間が長いほど読む量が増えるので、上限も伸ばす。
case "$PERIOD" in
  daily)   TIMEOUT=900 ;;
  weekly)  TIMEOUT=1500 ;;
  monthly) TIMEOUT=2400 ;;
esac

# プロンプトは**標準入力から渡す**。--disallowedTools は可変長引数なので、
# 後ろに置いたプロンプトまでツール名として飲み込んでしまう。
printf '%s' "$PROMPT" | timeout "$TIMEOUT" "$CLAUDE_BIN" -p \
  --agent "$AGENT" \
  --permission-mode bypassPermissions \
  --disallowedTools "Edit,Write,NotebookEdit" \
  > "$REPORT" 2> "${REPORT%.md}.err"
STATUS=${PIPESTATUS[1]}

if [ $STATUS -ne 0 ] || [ ! -s "$REPORT" ]; then
  {
    echo "**$TITLE — 生成に失敗**"
    echo "claude の終了コード: $STATUS（124 なら $((TIMEOUT / 60)) 分で時間切れ）"
    echo '```'
    tail -c 800 "${REPORT%.md}.err" 2>/dev/null
    echo '```'
    echo "サーバーで確認: \`$HOME_DIR/deploy/report.sh $PERIOD $ARG\`"
  } | "$POST_BIN"
  exit 1
fi

if [ "${DRY_RUN:-}" = "1" ]; then
  cat "$REPORT"
  exit 0
fi

# 週次・月次は Obsidian の vault にも残す。記録の本体は vault 側（~/obsidian-vault）で、
# state/reports/ は生成の作業場という位置づけ。日次は量が多くノイズなので入れない。
# vault は独立した private リポジトリなので、書いたら commit・push まで済ませる。
# ここで失敗してもレポートの配達は止めない（記録が 1 本欠けるだけ）。
VAULT_DIR="${VAULT_DIR:-$HOME/obsidian-vault}"
if [ "$PERIOD" != "daily" ] && [ -d "$VAULT_DIR/.git" ]; then
  VAULT_NOTE="$VAULT_DIR/50-reports/$(basename "$REPORT")"
  {
    echo "---"
    echo "type: report"
    echo "domain: 投資"
    echo "project: jstock-go"
    echo "period: $PERIOD"
    echo "from: $FROM"
    echo "to: $TO"
    echo "generated: $NOW"
    echo "tags:"
    echo "  - レポート"
    echo "---"
    echo
    echo "# $TITLE"
    echo
    cat "$REPORT"
  } > "$VAULT_NOTE"

  # 先に commit してから取り込む。pull --rebase は未コミットの変更があると必ず失敗するので、
  # pull を先に置くと（VM 側で何か触りかけているだけで）毎回止まる。他人の変更が残っていても
  # 進めるよう --autostash を付ける。競合したら push を諦めて次回に回す。
  git -C "$VAULT_DIR" add "$VAULT_NOTE" 2>>"${REPORT%.md}.err" || :
  # 同じ期間を撮り直したときは中身が変わらないことがある。その場合は commit しない
  if git -C "$VAULT_DIR" diff --cached --quiet -- "$VAULT_NOTE"; then
    echo "vault: $TITLE に変更なし" >&2
  elif git -C "$VAULT_DIR" commit --quiet -m "docs(reports): $TITLE" 2>>"${REPORT%.md}.err"; then
    git -C "$VAULT_DIR" pull --rebase --autostash --quiet 2>>"${REPORT%.md}.err" \
      && git -C "$VAULT_DIR" push --quiet origin main 2>>"${REPORT%.md}.err" \
      || echo "vault の push に失敗（commit は済んでいる。次回の実行で一緒に上がる）" >&2
  else
    echo "vault への commit に失敗（ノートは $VAULT_NOTE に残っている）" >&2
  fi
fi

# 日次は本文の 1 行目が見出し（エージェントが書く）。週次・月次は期間が見出しなので
# ここで付ける——スレッド名にもなる
if [ "$PERIOD" = "daily" ]; then
  "$POST_BIN" < "$REPORT"
else
  "$POST_BIN" --title "$TITLE" < "$REPORT"
fi

# 古い控えを消す。日次は 45 日（月次レポートが前月ぶんを読み返せる余裕を持たせる）、
# 週次は 180 日、月次は消さない——月次が長期の記録そのものなので。
# 判断は更新時刻。後から手で開いたファイルは残る。
find "$REPORT_DIR" -maxdepth 1 -type f -mtime +45 \
  \( -name 'daily-*.md' -o -name 'daily-*.err' -o -name 'digest-*.snapshot.jsonl' \) \
  -delete 2>/dev/null || :
find "$REPORT_DIR" -maxdepth 1 -type f -mtime +180 \
  \( -name 'weekly-*.md' -o -name 'weekly-*.err' \) \
  -delete 2>/dev/null || :
