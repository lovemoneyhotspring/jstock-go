#!/usr/bin/env bash
# JPX日経中小型株指数の採用（毎年 8 月）の予定を前もって Discord で知らせる（test/jsm_remind.py が文面を決める）。
# 1 か月前・2 週間前からのカウントダウン・発表の日・買う日・売る日の前営業日・売る日。発注はしない。
# cron: 30 7 * 7,8 *。ログは state/logs/jsm-remind.log。
set -u
cd "$(dirname "$0")/.."
HOME_DIR="$(pwd)"
POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"
echo "== $(date '+%F %T')"
MSG="$(test/.venv/bin/python test/jsm_remind.py "$@" 2>>state/logs/jsm-remind.err)" || { echo "jsm_remind.py が失敗"; tail -3 state/logs/jsm-remind.err; exit 1; }
[ -n "$MSG" ] || { echo "今日の知らせは無い"; exit 0; }
TITLE="$(echo "$MSG" | head -1)"
BODY="$(echo "$MSG" | tail -n +2)"
echo "$TITLE"
[ -x "$POST_BIN" ] || { echo "discord-post が無い"; exit 0; }
( if [ -f "$HOME_DIR/.env" ]; then set -a; . "$HOME_DIR/.env"; set +a; fi
  printf '%s\n' "$BODY" | timeout -k 10 120 "$POST_BIN" --title "$TITLE" ) || echo "Discord に送れなかった: $TITLE"
