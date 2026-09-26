#!/usr/bin/env bash
# 重い検証（backtest・walk-forward）を 1 本ずつ回す。並列にするとメモリ不足で止まる（2026-09 に確認）。
# 先に走っている回があれば、終わるまでロックで待ってから始める（プロセス名で待つと、待つ側が自分自身に一致して止まる）。
#   bash test/heavy.sh test/.venv/bin/python test/dt_err_model.py > test/out/x.txt 2>&1
# **呼んだ場所の作業ディレクトリ（git worktree を含む）で動く**。2026-09-26 まではこのスクリプトの置き場所へ移っていたため、
# worktree から呼んだ学習が本番の作業ディレクトリに書き出した。ロックは git の共通ディレクトリ（worktree 間で共有）に置く。
# PYTHONPATH は既定で test。
set -eu
cd "$(git rev-parse --show-toplevel)"
export PYTHONPATH="${PYTHONPATH:-test}"
exec flock "$(git rev-parse --git-common-dir)/heavy.lock" "$@"
