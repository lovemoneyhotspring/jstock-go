#!/usr/bin/env bash
# 5 つの実行ファイルを bin/ に作る。cron（deploy/crontab.txt）はここを見る。
#
#   deploy/build.sh          # $WBJP_HOME/bin に作る
#   BIN_DIR=/tmp/bin deploy/build.sh
#
# 依存は Go だけ（1.26 以上）。uv も Python も要らない。

set -euo pipefail

HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
BIN_DIR="${BIN_DIR:-$HOME_DIR/bin}"

cd "$HOME_DIR"
mkdir -p "$BIN_DIR"

# -trimpath: 実行ファイルに開発機の絶対パスを埋めない
for cmd in wbjp accum daytrade jquants discord-post; do
  echo "building $cmd..."
  go build -trimpath -o "$BIN_DIR/$cmd" "./cmd/$cmd"
done

echo
echo "できました: $BIN_DIR"
ls -l "$BIN_DIR"
