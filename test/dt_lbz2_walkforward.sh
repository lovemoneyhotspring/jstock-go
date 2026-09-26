#!/usr/bin/env bash
# LBZ2（平常日）と lgbm_rank（米国小幅高の日）を年ごとの walk-forward で学習し、その年だけを Go の backtest で回す。
# 年 Y のモデルは Y-1 年 12 月 31 日までで学習（テストの年を学習に含めない）。
#
#   bash test/dt_lbz2_walkforward.sh [年 ...]        # 既定 2018〜2026
#
# 出力（test/out/wf/）:
#   <Y>/lbz2・<Y>/lgbm_rank          年ごとのモデルの束
#   trades_<Y>.csv                    本番の形（config/daytrade_margin）でモデルだけ差し替えた取引
#   wide_<Y>.csv                      上限額の広い設定（test/dt_rank_allocation.py の材料）
#   trades.csv・wide.csv              全年をつないだもの
# 重い計算は test/heavy.sh で 1 本ずつ。候補表は test/out/dt_candidates_wide.parquet（test/dt_lgbm_train_spec.py の冒頭）。
set -eu
cd "$(git rev-parse --show-toplevel)"
export PYTHONPATH=test
W=test/out/wf
mkdir -p "$W"
YEARS=("$@")
[ ${#YEARS[@]} -eq 0 ] && YEARS=(2018 2019 2020 2021 2022 2023 2024 2025 2026)

for Y in "${YEARS[@]}"; do
  last="$((Y - 1))-12-31"
  for m in lbz2 lgbm_rank; do
    if [ ! -f "$W/$Y/$m/manifest.json" ]; then
      bash test/heavy.sh test/.venv/bin/python test/dt_lgbm_train_spec.py "$m" --last-day "$last" --out "$W/$Y/$m" > "$W/train_${Y}_$m.log" 2>&1
    fi
  done
  mkdir -p "$W/cfg_$Y" "$W/cfgwide_$Y"
  cat > "$W/cfg_$Y/daytrade.toml" <<EOF
extends = "../../../../config/daytrade_margin"
[signal]
model = "../$Y/lbz2/manifest.json"
model_us_low = "../$Y/lgbm_rank/manifest.json"
EOF
  cat > "$W/cfgwide_$Y/daytrade.toml" <<EOF
extends = "../cfg_$Y"
[capital]
max_capital = 10000000000
max_positions = 30
name_divisor = 1
max_order = 0
[signal]
max_per_sector = 0
EOF
  bash test/heavy.sh ./bin/daytrade backtest --config-dir "$W/cfg_$Y" --since "$Y-01-01" --until "$Y-12-31" --trades-csv "$W/trades_$Y.csv" > "$W/bt_$Y.txt" 2>&1
  bash test/heavy.sh ./bin/daytrade backtest --config-dir "$W/cfgwide_$Y" --since "$Y-01-01" --until "$Y-12-31" --trades-csv "$W/wide_$Y.csv" > "$W/btwide_$Y.txt" 2>&1
  echo "$Y: $(grep -E '^合算' "$W/bt_$Y.txt")"
done

for k in trades wide; do
  first=1
  for f in $(ls "$W"/${k}_20[0-9][0-9].csv | sort); do
    if [ $first = 1 ]; then cat "$f"; first=0; else tail -n +2 "$f"; fi
  done > "$W/$k.csv"
done
