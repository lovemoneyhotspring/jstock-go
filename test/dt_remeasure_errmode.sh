#!/usr/bin/env bash
# 本番の日の区分・単元の判定のうえで、(1) 一次の横断を本番で gap_vol の日だけに絞って測り直し、
# (2) 気配の誤差を日ごとにまとめて引き・誤差の日を復元抽出する形（DT_ERR_MODE=day_boot）で、誤差に頼る検証を測り直す。
# 根拠: vault 20-research/2026-09-jp-daytrade-sim-parity.md。1 本ずつ。実行中にこのファイルを書き換えない。
#   bash test/dt_remeasure_errmode.sh [名前 ...]   # 出力は test/out/remeasure_err/<名前>.txt
set -u
cd "$(dirname "$0")/.."
# 誤差の実測は 2026-09-24 まで（7 日）に固定する。単元の判定・日の区分の測り直しと同じ材料で比べるため
export PYTHONPATH=test DT_ERR_UNTIL=2026-09-24
PY=test/.venv/bin/python
OUT=test/out/remeasure_err
mkdir -p "$OUT"
declare -A CMD=(
  [oscillator]="env $PY test/dt_oscillator.py --top 10 --days gapvol"
  [three_day]="env $PY test/dt_three_day.py --top 10 --days gapvol"
  [rsi_family]="env DT_DAY_RULES=prod DT_AFFORD=1 DT_ERR_MODE=day_boot $PY test/dt_rsi_family.py --seeds 10"
  [rsi2_deep]="env DT_DAY_RULES=prod DT_AFFORD=1 DT_ERR_MODE=day_boot $PY test/dt_rsi2_deep.py --seeds 10"
  [prefer_lgbm_wf]="env DT_DAY_RULES=prod DT_AFFORD=1 DT_ERR_MODE=day_boot $PY test/dt_prefer_lgbm_wf.py --seeds 10"
  [k]="env DT_DAY_RULES=prod DT_AFFORD=1 DT_ERR_MODE=day_boot $PY test/dt_nscale.py --part k --ks 3 5 6 7 9 --i0 5 --seeds 10"
)
ORDER=(oscillator three_day rsi_family rsi2_deep prefer_lgbm_wf k)
[ $# -gt 0 ] && ORDER=("$@")
for name in "${ORDER[@]}"; do
  start=$(date +%s)
  echo "== $(date '+%H:%M:%S') $name: ${CMD[$name]}"
  ${CMD[$name]} > "$OUT/$name.txt" 2>&1
  echo "   終了 $? $(( $(date +%s) - start )) 秒"
done
