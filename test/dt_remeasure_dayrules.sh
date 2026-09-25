#!/usr/bin/env bash
# 日の区分を本番の形（S&P500 0〜+1% かつ VIX ≤ 24・12 月休み）にして、ナスダック近似で測った検証を測り直す。
# 根拠: vault 20-research/2026-09-jp-daytrade-sim-parity.md。重い検証は 1 本ずつ（並列はメモリ不足で止まる）。
#   bash test/dt_remeasure_dayrules.sh [名前 ...]   # 名前を省くと全部。出力は test/out/remeasure/<名前>.txt
# I0 は vault の古い記録と同じ値にそろえる（比べるのは同じ前提どうし）。実行中にこのファイルを書き換えない（bash は逐次読む）
set -u
cd "$(dirname "$0")/.."
#   DT_AFFORD=1 bash test/dt_remeasure_dayrules.sh   # 単元の判定も Go に合わせる（出力は test/out/remeasure_afford/）
export DT_DAY_RULES=prod PYTHONPATH=test
PY=test/.venv/bin/python
OUT=test/out/remeasure
[ "${DT_AFFORD:-}" = "1" ] && OUT=test/out/remeasure_afford
mkdir -p "$OUT"

declare -A CMD=(
  [k]="test/dt_nscale.py --part k --ks 3 5 6 7 9 --i0 5"
  [rule]="test/dt_nscale.py --part rule --i0 3 5 10"
  [prod]="test/dt_nscale.py --part prod --i0 3 5 10"
  [sector_m]="test/dt_nscale.py --part sector_m --i0 5 3 --seeds 3"
  [ranker]="test/dt_nscale_ranker.py"
  [ranker_liq]="test/dt_nscale_ranker.py --liq-cost"
  [prefer_lgbm_wf]="test/dt_prefer_lgbm_wf.py --seeds 10"
  [rsi_family]="test/dt_rsi_family.py --seeds 10"
  [rsi2_deep]="test/dt_rsi2_deep.py --seeds 10"
  [ext]="test/dt_nscale.py --part ext --i0 5"
  [wilder]="test/dt_wilder.py --seeds 10"
  [now]="test/dt_nscale.py --part now --i0 3 5"
  [cap200]="test/dt_nscale.py --part cap200 --i0 3 5"
  [r200]="test/dt_nscale.py --part r200 --i0 3 5"
  [sector]="test/dt_nscale.py --part sector --i0 3 5 10"
  [fixed]="test/dt_nscale.py --part fixed --caps 1e7 1.5e7 2e7 --i0 5 3"
)
ORDER=(k rule prod sector_m ranker ranker_liq prefer_lgbm_wf rsi_family rsi2_deep ext wilder now cap200 r200 sector fixed)
[ $# -gt 0 ] && ORDER=("$@")

for name in "${ORDER[@]}"; do
  start=$(date +%s)
  echo "== $(date '+%H:%M:%S') $name: ${CMD[$name]}"
  $PY ${CMD[$name]} > "$OUT/$name.txt" 2>&1
  echo "   終了 $? $(( $(date +%s) - start )) 秒"
done
