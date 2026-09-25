#!/usr/bin/env bash
# 2026-09-25 に不採用にしたロングの採用候補 4 本（ストキャス RSI・RSI(2)・案 A・LightGBM の日の優先）を、
# 気配の誤差の材料が溜まってから回し直す。根拠と再提案の条件: vault 20-research/2026-09-jp-daytrade-remeasure-afford-err.md
#   bash test/dt_recheck_rejected_longs.sh --check   # 材料の日数を数えるだけ（重くない。いつ回してよいか）
#   bash test/dt_recheck_rejected_longs.sh [名前 ...] # 回す（1 本ずつ。出力は test/out/recheck_longs/<名前>.txt）
# 誤差の材料は層にした既定（8:59:52 の発注時の気配 → 8:59:48 以降の板 → 8:59:30）。DT_ERR_SLOT は付けない
# （9/25 までの 8:59:00 の snap で再現する測り直しとは材料が違う）。最終日は回した日の前日に固定する。
set -u
cd "$(dirname "$0")/.."
export PYTHONPATH=test
PY=test/.venv/bin/python
NEED_DAYS=20   # 判定に使う誤差の材料の日数（下限 MIN_ERR_DAYS 10 の倍。9/25 に 7 → 8 日で t が反転した）

days() {
  $PY - <<'EOF'
import dt_preopen_sim as s
s.MIN_SNAP_DAYS = 0
e = s.error_frame("snap", "2026-09-11")
first = e.loc[e["src"] != "085930", "d"]
print(first.nunique(), str(first.max())[:10] if len(first) else "-")
EOF
}

read -r n last < <(days)
echo "8:59:48 以降の材料のある日: ${n} 日（最終 ${last}）、判定に要る日数 ${NEED_DAYS}"
if [[ "${1:-}" == "--check" ]]; then
  [[ "$n" -ge "$NEED_DAYS" ]] && echo "回してよい" || echo "まだ（あと $((NEED_DAYS - n)) 日）"
  exit 0
fi
if [[ "$n" -lt "$NEED_DAYS" ]]; then
  echo "材料が ${NEED_DAYS} 日に満たないので回さない" >&2
  exit 1
fi

export DT_ERR_UNTIL="$last"
OUT=test/out/recheck_longs
mkdir -p "$OUT"
declare -A CMD=(
  [rsi_family]="env DT_DAY_RULES=prod DT_AFFORD=1 DT_ERR_MODE=day_boot $PY test/dt_rsi_family.py --seeds 10"
  [rsi2_deep]="env DT_DAY_RULES=prod DT_AFFORD=1 DT_ERR_MODE=day_boot $PY test/dt_rsi2_deep.py --seeds 10"
  [prefer_lgbm_wf]="env DT_DAY_RULES=prod DT_AFFORD=1 DT_ERR_MODE=day_boot $PY test/dt_prefer_lgbm_wf.py --seeds 10"
)
ORDER=(rsi_family rsi2_deep prefer_lgbm_wf)
names=("$@")
[[ ${#names[@]} -eq 0 ]] && names=("${ORDER[@]}")
for name in "${names[@]}"; do
  [[ -n "${CMD[$name]:-}" ]] || { echo "知らない名前: $name（${ORDER[*]}）" >&2; exit 2; }
  echo "== $name（誤差の材料 ${n} 日、${DT_ERR_UNTIL} まで）"
  ${CMD[$name]} > "$OUT/$name.txt" 2>&1 || echo "失敗: $name（$OUT/$name.txt）"
  tail -5 "$OUT/$name.txt"
done
