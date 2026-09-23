#!/usr/bin/env bash
# 夜間自己修復: 前夜からの実行ログに異常があれば、night-repair サブエージェントに
# 原因調査とコード修正案の作成（新しいブランチ → push → PR 作成）までやらせる。
#
#   WBJP_ENV=prod deploy/night-repair.sh               # 通常実行（cron 用、6:00）
#   WBJP_ENV=prod DRY_RUN=1 deploy/night-repair.sh     # 異常判定・調査はするが push・PR 作成・Discord 送信をしない
#   NIGHT_REPAIR_MODEL=sonnet deploy/night-repair.sh   # モデルを変えて試す
#
# 設計の要点:
#   - 異常が無ければ claude を起動しない（前段の jq 判定。API コストを抑える）
#   - コードを直すのは /tmp の git worktree の中だけ。この作業ツリー（$HOME_DIR）は
#     8:30〜 の cron が config/daytrade_margin/*.toml と bin/ を読むので、ブランチを
#     切り替えない・pull しない・書き込まない
#   - 禁止事項（main への commit・push、deploy/build.sh、crontab、--live と発注系の
#     コマンド、config/ の編集、本番の作業ツリーでの checkout / pull）はプロンプト
#     （.claude/agents/night-repair.md）に書いたうえで、--disallowedTools でも止める。
#     ただしツールの規則は「Claude が書くいつもの形」を止めるだけで境界ではない
#     （別の書き方で抜けうる）ので、終わった後に本番の作業ツリーが動いていないかを
#     このスクリプトが確かめ、動いていればレポートの頭に書いて Discord に流す
#   - .env は丸ごと export しない。claude の子に立花・Webull・J-Quants の鍵を渡さない。
#     Discord の送信（post）だけがサブシェルで .env を読む。bin/* の Go は自分で .env を読む
#   - 生成（claude）と配達（discord-post）を分ける。report.sh と同じ理由
#   - 本文は state/reports/ に必ず残す。Discord に届かなくても後から読める

set -uo pipefail

HOME_DIR="${WBJP_HOME:-/home/abobo/jstock-go}"
cd "$HOME_DIR" || exit 1

TODAY="$(TZ=Asia/Tokyo date +%F)"
YESTERDAY="$(TZ=Asia/Tokyo date -d yesterday +%F)"
REPORT_DIR="$HOME_DIR/state/reports"
REPORT="$REPORT_DIR/night-repair-$TODAY.md"
mkdir -p "$REPORT_DIR"

# 配達係（deploy/build.sh が bin/ に作る）
POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"

# with_dotenv は .env を読んだサブシェルでコマンドを 1 つ走らせる。cron はログインシェルを
# 通らないので、Discord の送り先（WBJP_DISCORD_BOT_TOKEN・チャンネル ID）はここでしか分からない。
# 以前は `set -a; . .env` をこのシェルでやっていて、.env の全部（秘密鍵を含む）が claude に
# 渡っていた。claude が要るのは WBJP_ENV とメモリの上限だけ
with_dotenv() {
  (
    if [ -f "$HOME_DIR/.env" ]; then
      set -a
      # shellcheck disable=SC1091
      . "$HOME_DIR/.env"
      set +a
    fi
    "$@"
  )
}
post() { with_dotenv "$POST_BIN" "$@"; }

# 口座は明示させる（Go の既定は uat。docs/DEPLOY.md「cron の環境」）。cron の行は WBJP_ENV=prod を
# 渡す。環境に無ければ .env の値を使う
if [ -z "${WBJP_ENV:-}" ]; then
  WBJP_ENV="$(with_dotenv printenv WBJP_ENV || :)"
fi
if [ -z "${WBJP_ENV:-}" ]; then
  echo "WBJP_ENV が未設定です。prod か uat を明示してください（例: WBJP_ENV=prod DRY_RUN=1 $0）" >&2
  exit 2
fi
export WBJP_ENV

# メモリの上限（deploy/crontab.txt と同じ値）。claude が子プロセスで daytrade / jquants を
# 叩くので、ここで export しないと上限の無いまま DuckDB がシステムメモリの 80% を取りに行く
export JQUANTS_READ_BUDGET_MB="${JQUANTS_READ_BUDGET_MB:-2048}"
export GOMEMLIMIT="${GOMEMLIMIT:-4GiB}"
export GOGC="${GOGC:-400}"
export WBJP_DUCKDB_MEMORY_LIMIT="${WBJP_DUCKDB_MEMORY_LIMIT:-3GB}"

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

# verify: true は人が --broker-verify を付けて走らせた発注経路の検証。日次レポート
# （.claude/agents/daily-report.md）は既に除いているのに、ここだけ拾っていた
# ——検証で出した「時間外の発注」「持ち越し」で claude が起動し、直す物が無いまま
# 30 分ぶんのトリアージと PR が生まれる。同じ規則で除く。
# command == "backtest" も除く。研究で手で叩く検証で本番の売買経路ではなく、設定の置き忘れや
# 試作の OOM で落ちても直す物が無い（2026-09-13 に 5 件でトリアージが起きた）。
ANOMALY_COUNT="$(jq -c 'select((.anomalies or .outcome == "error") and (.verify | not) and .command != "backtest")' "${DIGESTS[@]}" 2>/dev/null | wc -l | tr -d ' ')"
if [ "$ANOMALY_COUNT" = "0" ]; then
  echo "異常なし（$YESTERDAY 〜 $TODAY）。claude は起動しません"
  exit 0
fi

echo "異常 $ANOMALY_COUNT 件を検知。night-repair エージェントを起動します"

# cron の PATH には ~/.local/bin が入っていないので絶対パスで持つ
CLAUDE_BIN="${CLAUDE_BIN:-$HOME/.local/bin/claude}"
[ -x "$CLAUDE_BIN" ] || CLAUDE_BIN="$(command -v claude || echo "$CLAUDE_BIN")"

NOW="$(TZ=Asia/Tokyo date +%H:%M)"

# ツールの禁止（Claude Code の deny 規則。bypassPermissions でも効き、&& で繋いだ各部分にも効く）。
# `*` はどの位置にも置ける。規則はコマンドの文字列全体に当たるので、コミットメッセージに
# `--live` と書いても止まる——md で「文字列として書かない」と伝えてある
DISALLOWED=(
  # 本番反映（どの呼び方でも）
  "Bash(*deploy/build.sh*)"
  "Bash(crontab:*)"
  # 発注系。dry-run に見えても叩かない
  "Bash(*--live*)"
  "Bash(*bin/daytrade open*)" "Bash(*bin/daytrade close*)" "Bash(*bin/wbjp run*)" "Bash(*bin/accum run*)"
  "Bash(*cmd/daytrade open*)" "Bash(*cmd/daytrade close*)" "Bash(*cmd/wbjp run*)" "Bash(*cmd/accum run*)"
  # 本番の作業ツリーを動かす git。worktree の中ではどれも要らない
  # （-C は cd の代わりに他の木を触れてしまうので、cd してから叩かせる）
  "Bash(git checkout:*)" "Bash(git switch:*)" "Bash(git pull:*)" "Bash(git merge:*)"
  "Bash(git rebase:*)" "Bash(git reset:*)" "Bash(git stash:*)" "Bash(git -C:*)"
  # main への push・行き先を省いた push・強制 push・ブランチの削除・マージ
  "Bash(git push)" "Bash(git push origin)" "Bash(git push -u origin)"
  "Bash(git push* main)" "Bash(git push* main *)" "Bash(git push*:main*)"
  "Bash(git push*--force*)" "Bash(git push* -f*)" "Bash(git push*+*)"
  "Bash(git push*--all*)" "Bash(git push*--mirror*)" "Bash(git push*--delete*)"
  "Bash(gh pr merge:*)"
  # 書き込みは worktree（/tmp/night-repair-*）の中だけ。戦略パラメータ（config/）は worktree でも変えない。
  # Edit の規則は Write を含む書き込み全部に効く（Write(...) の規則は照合されず、起動時に警告が出るだけ）
  "Edit(/$HOME_DIR/**)"
  "Edit(//tmp/night-repair-*/config/**)"
  "Read(/$HOME_DIR/.env)"
)

PROMPT="$TODAY（JST 今 $NOW）の運用ログに異常が $ANOMALY_COUNT 件見つかりました。
原因を調べ、直せるならコードの修正案を作ってください（.claude/agents/night-repair.md の
手順・制約に従うこと）。コードを直すときは本番の作業ツリー $HOME_DIR では作業せず、
git worktree（/tmp/night-repair-<YYYYMMDD>-<slug>）を作ってその中で編集・テスト・コミットしてください。
標準出力にはレポート本文だけを書いてください。"

if [ "${DRY_RUN:-}" = "1" ]; then
  # DRY_RUN はテスト実行なので、実際の push・PR 作成はツールレベルで
  # 強制的に止める（プロンプトの「お願い」だけに頼らない）。
  DISALLOWED+=("Bash(git push:*)" "Bash(gh pr create:*)" "Bash(gh pr edit:*)")
  PROMPT="$PROMPT

これは DRY_RUN（テスト実行）です。git push と PR 作成はツール側でブロックされていて
実行できません（失敗しても気にせず続けてよい）。worktree でブランチを切ってコミットする
ところまでは試してよいですが、最後は必ず worktree を消して（git worktree remove）終えてください。
レポートには「本番実行ならここで push して PR を作成していた」という想定内容を書いてください。"
fi

# 本番の作業ツリーの状態を控える（終わった後に動いていないかを見る）
prod_branch() { git symbolic-ref --short -q HEAD || echo "(detached)"; }
BRANCH_BEFORE="$(prod_branch)"
HEAD_BEFORE="$(git rev-parse -q HEAD)"
STATUS_BEFORE="$(git status --porcelain)"

# --agent で night-repair を使う。1800 秒（30 分）で打ち切る。
# プロンプトは標準入力から渡す（--disallowedTools は可変長引数で、後ろの引数を飲み込む）
printf '%s' "$PROMPT" | timeout 1800 "$CLAUDE_BIN" -p \
  --agent night-repair \
  --model "${NIGHT_REPAIR_MODEL:-claude-fable-5-1}" \
  --effort "${NIGHT_REPAIR_EFFORT:-medium}" \
  --permission-mode bypassPermissions \
  --disallowedTools "${DISALLOWED[@]}" \
  > "$REPORT" 2> "$REPORT_DIR/night-repair-$TODAY.err"
STATUS=${PIPESTATUS[1]}

# --- 本番の作業ツリーが動いていないか ---------------------------------------
GUARD=()
BRANCH_AFTER="$(prod_branch)"
if [ "$BRANCH_AFTER" != "$BRANCH_BEFORE" ]; then
  # 汚れていなければ元のブランチに戻す（8:30 の cron が別ブランチの config を読まないように）。
  # 汚れていれば触らない——人の判断が要る
  if [ -z "$(git status --porcelain)" ] && git checkout --quiet "$BRANCH_BEFORE" 2>/dev/null; then
    GUARD+=("本番の作業ツリーのブランチが $BRANCH_BEFORE → $BRANCH_AFTER に変わっていたので $BRANCH_BEFORE に戻した")
  else
    GUARD+=("本番の作業ツリーのブランチが $BRANCH_BEFORE → $BRANCH_AFTER のまま（未コミットの変更があり戻していない。8:30 の cron までに確認）")
  fi
fi
if [ "$(git rev-parse -q HEAD)" != "$HEAD_BEFORE" ] && [ "$(prod_branch)" = "$BRANCH_BEFORE" ]; then
  GUARD+=("本番の作業ツリーの $BRANCH_BEFORE に commit が積まれた（$(git log --oneline -1 | cut -c1-60)）")
fi
if [ "$(git status --porcelain)" != "$STATUS_BEFORE" ]; then
  GUARD+=("本番の作業ツリーに未コミットの変更が増えた: $(git status --porcelain | head -3 | tr '\n' ' ')")
fi
# 残った worktree を片付ける（ブランチとコミットは消えない。push 済みなら PR も残る）
git worktree list --porcelain | sed -n 's|^worktree \(/tmp/night-repair-.*\)|\1|p' | while read -r wt; do
  git worktree remove --force "$wt" 2>/dev/null && echo "残っていた worktree を消した: $wt"
done
git worktree prune 2>/dev/null || :

if [ ${#GUARD[@]} -gt 0 ]; then
  {
    echo "**⚠ 本番の作業ツリーが動いた（night-repair.md の禁止事項に反する）**"
    for g in "${GUARD[@]}"; do echo "- $g"; done
    echo
    cat "$REPORT" 2>/dev/null
  } > "$REPORT.tmp" && mv "$REPORT.tmp" "$REPORT"
  printf '%s\n' "${GUARD[@]}"
fi

if [ $STATUS -ne 0 ] || [ ! -s "$REPORT" ]; then
  {
    echo "**夜間トリアージ $TODAY — 生成に失敗**"
    echo "検知した異常: $ANOMALY_COUNT 件"
    echo "claude の終了コード: $STATUS（124 なら 30 分で時間切れ）"
    echo '```'
    tail -c 800 "$REPORT_DIR/night-repair-$TODAY.err" 2>/dev/null
    echo '```'
    echo "サーバーで確認: \`WBJP_ENV=$WBJP_ENV $HOME_DIR/deploy/night-repair.sh\`"
  } | post --title "夜間自己修復 $TODAY"
  exit 1
fi

if [ "${DRY_RUN:-}" = "1" ]; then
  cat "$REPORT"
  exit 0
fi

post --title "夜間自己修復 $TODAY" < "$REPORT"

# 45 日より古い控えを消す（report.sh と同じ約束）
find "$REPORT_DIR" -maxdepth 1 -type f -mtime +45 \
  \( -name 'night-repair-*.md' -o -name 'night-repair-*.err' \) \
  -delete 2>/dev/null || :
