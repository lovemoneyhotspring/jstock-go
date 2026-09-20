#!/usr/bin/env bash
# 実行ファイルを 1 世代前（deploy/build.sh が bin/.prev に残したもの）へ戻す。
#
#   deploy/rollback-bin.sh
#
# 新しい実行ファイルが動かないときの復旧。ただし**設定が実行ファイルより新しい**ときは戻すと
# 悪化する（古い実行ファイルは新しい項目を読めない。9/19 の事故）——その場合は build.sh で作り直す。
# 一式そろっていなければ何もしない（半端に戻さない）。全部の .rb（コピー）を作ってから mv するので、
# コピーの途中で失敗しても bin/ に新旧は混ざらない（mv は同じファイルシステムの rename）。
set -euo pipefail

HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
BIN_DIR="${BIN_DIR:-$HOME_DIR/bin}"
cmds=(wbjp accum daytrade jquants discord-post rate news)

for cmd in "${cmds[@]}"; do
  if [ ! -f "$BIN_DIR/.prev/$cmd" ]; then
    echo "rollback-bin: $BIN_DIR/.prev/$cmd が無いので戻しません（一式そろっていない）" >&2
    exit 1
  fi
done
# 戻す前の実行ファイルも .prev に入れ替えて残す（戻しすぎたら、もう一度実行すれば進める）
for cmd in "${cmds[@]}"; do
  cp -p "$BIN_DIR/.prev/$cmd" "$BIN_DIR/.$cmd.rb"
done
for cmd in "${cmds[@]}"; do
  ln -f "$BIN_DIR/$cmd" "$BIN_DIR/.prev/$cmd"
  mv -f "$BIN_DIR/.$cmd.rb" "$BIN_DIR/$cmd"
done
echo "rollback-bin: 1 世代前の実行ファイルに戻しました（$BIN_DIR）"
