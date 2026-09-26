#!/usr/bin/env bash
# 重い検証（backtest・walk-forward）を 1 本ずつ回す。並列にするとメモリ不足で止まる（2026-09 に確認）。
# 先に走っている回があれば、終わるまでロックで待ってから始める（プロセス名で待つと、待つ側が自分自身に一致して止まる）。
#   bash test/heavy.sh test/.venv/bin/python test/dt_err_model.py > test/out/x.txt 2>&1
# PYTHONPATH は既定で test。ロックは test/out/.heavy.lock（git の管理外）。
set -eu
cd "$(dirname "$0")/.."
mkdir -p test/out
export PYTHONPATH="${PYTHONPATH:-test}"
exec flock test/out/.heavy.lock "$@"
