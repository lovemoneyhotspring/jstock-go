#!/usr/bin/env bash
# 日次レポート。中身は deploy/report.sh に統合してある（週次・月次と同じ経路）。
#
#   deploy/daily-report.sh              # 今日（Asia/Tokyo）ぶん
#   deploy/daily-report.sh 2026-09-02   # 日付を指定
#   DRY_RUN=1 deploy/daily-report.sh    # 生成するだけで Discord に送らない
#
# 既存の cron と手順書がこの名前を指しているので、入口として残す。

exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/report.sh" daily "$@"
