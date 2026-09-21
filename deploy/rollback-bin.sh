#!/usr/bin/env bash
# 実行ファイルを 1 世代前（deploy/build.sh が bin/.prev に残したもの）へ戻す。
#
#   deploy/rollback-bin.sh
#
# 新しい実行ファイルが動かないときの復旧。ただし**設定が実行ファイルより新しい**ときは戻すと
# 悪化する（古い実行ファイルは新しい項目を読めない。9/19 の事故）——その場合は build.sh で作り直す。
# 一式そろっていなければ何もしない（半端に戻さない）。
#
# 入れ替えは「検査 → 下ごしらえ → rename」の 3 段。**失敗しうる操作（存在の検査・cp・ln）は
# 全コマンドぶんを先に済ませ**、bin/ と .prev/ の中身を動かすのは最後の mv（同じファイルシステムの
# rename）だけにする。以前は検査が .prev/<cmd> しか見ず、ln と mv をコマンドごとに交互に回していたので、
# bin/<cmd> が 1 つ欠けているとそこで set -e に打ち切られ、前半だけ戻った bin/（新旧の混在）と
# .rb の残骸が残った（2026-09-21 のレビュー）。
#
# bin/<cmd> が無いコマンドは**戻すだけ**にする（.prev/<cmd> はそのまま）。この道具は復旧用で、
# 実行ファイルが欠けているのはまさに戻したい場面——そこで全体を断ると guard-preopen.sh の自動復旧も
# 止まる。build.sh も無い bin/<cmd> は .prev に残さず進むので、それと揃う。ただしそのコマンドの
# 新しい版はどこにも残らないので、「もう一度実行して進める」は使えない（進めるなら build.sh）。
# bin/<cmd> が在るのに通常のファイルでない（ディレクトリなど）ときは、何も動かさずに終わる。
set -euo pipefail

HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
BIN_DIR="${BIN_DIR:-$HOME_DIR/bin}"
cmds=(wbjp accum daytrade jquants discord-post rate news)

# --- 検査: 両側を全コマンドぶん見る。1 つでも駄目なら何も動かさない ------------------------------
restore_only=()
for cmd in "${cmds[@]}"; do
  if [ ! -f "$BIN_DIR/.prev/$cmd" ]; then
    echo "rollback-bin: $BIN_DIR/.prev/$cmd が無いので戻しません（一式そろっていない）" >&2
    exit 1
  fi
  if [ ! -e "$BIN_DIR/$cmd" ]; then
    restore_only+=("$cmd")
  elif [ ! -f "$BIN_DIR/$cmd" ]; then
    echo "rollback-bin: $BIN_DIR/$cmd が通常のファイルではないので戻しません（何も動かしていない）" >&2
    exit 1
  fi
done

# --- 下ごしらえ: 戻す側は .<cmd>.rb（コピー）、.prev に残す側は .prev/.<cmd>.keep（ハードリンク）---
# ここで失敗しても bin/<cmd> と .prev/<cmd> はまだ 1 つも動いていない。残骸は trap が消す
trap 'for cmd in "${cmds[@]}"; do rm -f "$BIN_DIR/.$cmd.rb" "$BIN_DIR/.prev/.$cmd.keep"; done' EXIT
for cmd in "${cmds[@]}"; do
  cp -p "$BIN_DIR/.prev/$cmd" "$BIN_DIR/.$cmd.rb"
  # 戻す前の実行ファイルも .prev に入れ替えて残す（戻しすぎたら、もう一度実行すれば進める）
  if [ -f "$BIN_DIR/$cmd" ]; then
    ln -f "$BIN_DIR/$cmd" "$BIN_DIR/.prev/.$cmd.keep"
  fi
done

# --- 入れ替え: rename だけ ---------------------------------------------------------------
for cmd in "${cmds[@]}"; do
  if [ -f "$BIN_DIR/.prev/.$cmd.keep" ]; then
    mv -f "$BIN_DIR/.prev/.$cmd.keep" "$BIN_DIR/.prev/$cmd"
  fi
  mv -f "$BIN_DIR/.$cmd.rb" "$BIN_DIR/$cmd"
done
echo "rollback-bin: 1 世代前の実行ファイルに戻しました（$BIN_DIR）"
if [ "${#restore_only[@]}" -gt 0 ]; then
  echo "rollback-bin: ${restore_only[*]} は bin/ に無かったので戻しただけ（.prev はそのまま。進めるなら deploy/build.sh）"
fi
