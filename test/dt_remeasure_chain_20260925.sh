#!/usr/bin/env bash
# 2026-09-25 の測り直しの続き（順につなぐだけ）。誤差の実測は 2026-09-24 まで（7 日）に固定。
# 1) 誤差の引き方（day_boot）と一次の横断 2) 8 日の誤差で回ってしまった 9 本を 7 日で回し直す（8 日の出力は remeasure_afford_8d に残す）
set -u
cd "$(dirname "$0")/.."
bash test/dt_remeasure_errmode.sh > test/out/remeasure_err_log.txt 2>&1
mkdir -p test/out/remeasure_afford_8d
cp test/out/remeasure_afford/{cap200,ext,now,r200,rsi2_deep,rsi_family,sector,wilder,fixed}.txt test/out/remeasure_afford_8d/
DT_AFFORD=1 DT_ERR_UNTIL=2026-09-24 bash test/dt_remeasure_dayrules.sh ext rsi_family rsi2_deep wilder now cap200 r200 sector fixed \
  > test/out/remeasure_afford_log2.txt 2>&1
