#!/usr/bin/env bash
# ショック日のロングの倍率（margin.shock_long_scale）を今の本番の設定で測り直す（Go の backtest、始値ちょうどの上限側）。
# 根拠: vault 20-research/2026-09-jp-shock-days.md（9/5、旧い形で ×1.5 に決めた）。2026-09-26 に今の形で確かめた。
#   bash test/dt_shock_scale_recheck.sh [倍率 ...]   # 既定 1.0 1.5 2.0。出力は test/out/shock_scale/
set -u
cd "$(dirname "$0")/.."
OUT=test/out/shock_scale
mkdir -p "$OUT"
scales=("$@")
[[ ${#scales[@]} -eq 0 ]] && scales=(1.0 1.5 2.0)
# 設定は土台（extends = "../daytrade"）ごと写し、倍率の 1 行だけ差し替える
rm -rf "$OUT/daytrade" && cp -r config/daytrade "$OUT/daytrade"
for v in "${scales[@]}"; do   # 1 本ずつ（重い検証は並列にしない）
  rm -rf "$OUT/cfg$v" && cp -r config/daytrade_margin "$OUT/cfg$v"
  sed -i "s/^shock_long_scale = .*/shock_long_scale = $v/" "$OUT/cfg$v/daytrade.toml"
  bin/daytrade backtest --config-dir "$OUT/cfg$v" --since 2019-09-17 --trades-csv "$OUT/bt$v.csv" > "$OUT/bt$v.txt" 2>&1
  echo "== ×$v"; grep "^合算\|建玉の最大" "$OUT/bt$v.txt"
done
[[ -f "$OUT/bt1.0.csv" && -f "$OUT/bt1.5.csv" ]] || exit 0
PYTHONPATH=test test/.venv/bin/python - "$OUT" <<'EOF'
import sys
import pandas as pd
out = sys.argv[1]
def day(v):
    b = pd.read_csv(f"{out}/bt{v}.csv")
    b = b[b.side == "long"]
    return b.groupby("date").agg(pnl=("pnl", "sum"), amt=("amount", "sum"), scale=("scale", "max"))
a, c = day("1.0"), day("1.5")
sh = c[c.scale > 1].index
bp = (a.pnl / a.amt * 1e4).reindex(sh)
nb = (a.pnl / a.amt * 1e4).drop(sh)
print(f"ショック日 {len(sh)} 日: ×1.0 での建玉あたり 平均 {bp.mean():+.1f} bp・中央 {bp.median():+.1f}・勝率 {(bp > 0).mean():.0%}・"
      f"t {bp.mean() / bp.std() * len(bp) ** .5:+.2f}（平常の日 {nb.mean():+.1f} bp）")
idx = pd.to_datetime(sh)
for lab, m in (("〜2022", idx < "2023-01-01"), ("2023〜", idx >= "2023-01-01")):
    print(f"  {lab}: {m.sum()} 日、平均 {bp[m].mean():+.1f} bp、勝率 {(bp[m] > 0).mean():.0%}")
print("  最悪: " + "、".join(f"{d} {v:+.0f} bp" for d, v in bp.sort_values().head(3).items()))
print(f"×1.5 の上乗せ: 合計 {(c.pnl - a.pnl).reindex(sh).sum():+,.0f} 円（ショック日 1 日あたり {(c.pnl - a.pnl).reindex(sh).mean():+,.0f} 円）")
EOF
