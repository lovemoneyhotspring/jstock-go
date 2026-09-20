#!/usr/bin/env bash
# 7 つの実行ファイルを bin/ に作る。cron（deploy/crontab.txt）はここを見る。
#
#   deploy/build.sh          # $WBJP_HOME/bin に作る
#   BIN_DIR=/tmp/bin deploy/build.sh
#
# 依存は Go だけ（1.27 以上）。uv も Python も要らない。

set -euo pipefail

HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
BIN_DIR="${BIN_DIR:-$HOME_DIR/bin}"

cd "$HOME_DIR"
mkdir -p "$BIN_DIR"

# -trimpath: 実行ファイルに開発機の絶対パスを埋めない
#
# bin/ へ直接ビルドしない。cron は場中 10 分ごと（9:00 台は数十秒ごと）に bin/ を叩くので、
# 書きかけの実行ファイルを掴むか、実行中のファイルを上書きできずに失敗する。同じディレクトリの
# 別名に作り、**全部できてから** mv で差し替える（同じファイルシステムの rename は一瞬で、
# 途中で 1 本でもビルドに失敗したら古い一式のまま残る）。
cmds=(wbjp accum daytrade jquants discord-post rate news)
trap 'for cmd in "${cmds[@]}"; do rm -f "$BIN_DIR/.$cmd.new"; done' EXIT
for cmd in "${cmds[@]}"; do
  echo "building $cmd..."
  go build -trimpath -o "$BIN_DIR/.$cmd.new" "./cmd/$cmd"
done
# 差し替える前に、いまの実行ファイルを 1 世代だけ $BIN_DIR/.prev に残す（ハードリンクなので容量は増えない）。
# 新しい実行ファイルが動かないとき deploy/rollback-bin.sh が戻す
mkdir -p "$BIN_DIR/.prev"
for cmd in "${cmds[@]}"; do
  [ -f "$BIN_DIR/$cmd" ] && ln -f "$BIN_DIR/$cmd" "$BIN_DIR/.prev/$cmd"
done
for cmd in "${cmds[@]}"; do
  mv -f "$BIN_DIR/.$cmd.new" "$BIN_DIR/$cmd"
done

echo
echo "できました: $BIN_DIR"
ls -l "$BIN_DIR"
