#!/usr/bin/env bash
# ロングの出口を 前引け（11:30）/ 15:20 の成行 / 大引け（15:30 の板寄せ）で比べる（2026-09-24）。
# 保険の引け注文が前引けで約定した件（vault 10-journal/2026-09-24.md）を受けて。
# 建値は日足の寄付（本番の寄成と同じ）。分足のある期間だけ。重いので 1 本ずつ回す。
# 比べ方は test/dt_exit_midday.py（日ごとの差の t）。
set -euo pipefail
cd "$(dirname "$0")/.."
since=${SINCE:-2024-11-05}
until=${UNTIL:-2026-09-18}
for exit in 1130 1520 close; do
	flag=()
	[[ $exit != close ]] && flag=(--fill-exit "${exit:0:2}:${exit:2:2}")
	bin/daytrade backtest --config-dir config/daytrade_margin --since "$since" --until "$until" \
		"${flag[@]}" --trades-csv "test/out/dt_exit_$exit.csv" > "test/out/dt_exit_$exit.txt" 2>&1
	grep -E "^(分足|ロング )" "test/out/dt_exit_$exit.txt"
done
