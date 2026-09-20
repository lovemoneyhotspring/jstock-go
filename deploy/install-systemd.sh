#!/usr/bin/env bash
# cron とは別系統の実行役（systemd のユーザタイマー）を入れる。sudo は要らない。
#
#   deploy/install-systemd.sh            # 入れて有効にする
#   deploy/install-systemd.sh --remove   # 外す
#
#   jstock-close-net … 平日 15:22・15:26。cron の close が走っていなければ代わりに走らせる
#   jstock-guard     … 平日 8:42・15:12。crontab が消えていれば戻し、8:42 は寄る前の点検を直す
#
# ユーザタイマーはログアウト後・再起動後も動く（loginctl の linger が有効なこと。確認する）。
set -euo pipefail

HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
UNIT_DIR="$HOME/.config/systemd/user"
UNITS=(jstock-close-net jstock-guard)

if [ "${1:-}" = "--remove" ]; then
  for u in "${UNITS[@]}"; do
    systemctl --user disable --now "$u.timer" 2>/dev/null || true
    rm -f "$UNIT_DIR/$u.service" "$UNIT_DIR/$u.timer"
  done
  systemctl --user daemon-reload
  echo "install-systemd: 外しました"
  exit 0
fi

if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != "yes" ]; then
  echo "install-systemd: linger が無効です。ログアウト後にタイマーが止まります。次を実行してください:" >&2
  echo "  sudo loginctl enable-linger $USER" >&2
  exit 1
fi
for tool in jq flock timeout; do
  command -v "$tool" >/dev/null || { echo "install-systemd: $tool が見つかりません" >&2; exit 1; }
done

# 実行ファイルの場所（go は deploy/build.sh の作り直しが使う）を、ユニットの PATH に固定する
path_dirs=""
for tool in go jq flock timeout; do
  d=$(dirname "$(command -v "$tool")")
  case ":$path_dirs:" in *":$d:"*) ;; *) path_dirs="${path_dirs:+$path_dirs:}$d" ;; esac
done
unit_path="$path_dirs:/usr/local/bin:/usr/bin:/bin"

mkdir -p "$UNIT_DIR"
for u in "${UNITS[@]}"; do
  for ext in service timer; do
    sed -e "s#@HOME_DIR@#$HOME_DIR#g" -e "s#@PATH@#$unit_path#g" \
      "$HOME_DIR/deploy/systemd/$u.$ext" > "$UNIT_DIR/$u.$ext"
  done
done
systemctl --user daemon-reload
for u in "${UNITS[@]}"; do
  systemd-analyze --user verify "$UNIT_DIR/$u.service" "$UNIT_DIR/$u.timer" 2>&1 | grep -v '^$' || true
  systemctl --user enable --now "$u.timer"
done
echo "install-systemd: 有効にしました"
systemctl --user list-timers 'jstock-*' --no-pager
