"""本番の記録（気配・母集団の履歴）から、並べ方を後から並べ直して成績を比べる。

根拠: vault 20-research/2026-09-jp-daytrade-rankby-preopen-fair.md
2026-09-20 に本番の並べ方を gap_vol に戻したので、LightGBM の予測値は順位表（history/ranking）に
残らなくなった。気配（history/quotes）と母集団（history/plan）は毎朝残るので、そこから
「その朝の気配で lgbm / gap_vol / 素の gap ならどれを建てたか」を作り直し、当日の日足で採点する。
模擬ではなく**本番が実際に見た気配**での比較なので、誤差のモデルが要らない。

  test/.venv/bin/python test/dt_live_shadow.py [--since 2026-09-24] [--model config/daytrade/models/lgbm_rank.txt]

- 使う回: その日の最初の回（寄る前の 8:59:45 の回があればそれ）。出力の round に時刻（JST）を出す
- 候補: plan の eligible、気配が usable、見えるギャップ [-1, 0)、ストップ安でない
- 建て方・コストは test/dt_wf_target.py と同じ（上位 3・業種 1 銘柄・逆ボラ配分・流動性別コスト）。始値で建てて引けで手仕舞う
- 見送りの日（米国小幅高・12 月など）も並べ直して採点する（traded = False）。「休んだ日に建てていたら」が分かる
- 特徴量の作り方は test/dt_lgbm_train.py（Go の rerank と照合済み）をそのまま使う
出力: test/out/dt_live_shadow.parquet（日 × 並べ方）、標準出力に日ごとの表と累計
"""

import argparse

import duckdb
import lightgbm as lgb
import numpy as np
import pandas as pd

from dt_candidates import limit_down
from dt_lgbm_train import ranked, raw_features
from dt_preopen_sim import with_rule_rank
from dt_wf_target import evaluate, liq_cost_bp

HIST = "state/daytrade/history"
BARS = "data/jquants/equities_bars_daily/*.parquet"
OUT = "test/out/dt_live_shadow.parquet"

SQL = f"""
WITH q0 AS (
  SELECT *, min(recorded_at) OVER (PARTITION BY day, run_id) run_at
  FROM read_parquet('{HIST}/quotes/*.parquet', union_by_name=true) WHERE CAST(day AS DATE) >= CAST(? AS DATE)),
q AS (SELECT * FROM q0 QUALIFY run_at = min(run_at) OVER (PARTITION BY day)),
p AS (
  SELECT * FROM read_parquet('{HIST}/plan/*.parquet', union_by_name=true)
  QUALIFY recorded_at = max(recorded_at) OVER (PARTITION BY day)),
b AS (
  SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(O AS DOUBLE) o, TRY_CAST(C AS DOUBLE) c
  FROM read_parquet('{BARS}', union_by_name=true) WHERE CAST(Date AS DATE) >= CAST(? AS DATE))
SELECT CAST(q.day AS DATE) d, q.symbol code, q.run_at, q.price, q.prev_close, q.gap, q.from_book,
       p.vol20, p.ret1, p.ret5, p.ret20, p.pos20, p.prev_intraday, p.turnover_med, p.mkt_cap,
       p.short_interest, p.earn_yield, p.sector, b.o, b.c
FROM q JOIN p ON CAST(p.day AS DATE) = CAST(q.day AS DATE) AND p.symbol = q.symbol
JOIN b ON b.d = CAST(q.day AS DATE) AND b.code = q.symbol || '0'
WHERE p.eligible AND q.usable AND q.price > 0 AND q.prev_close > 0 AND b.o > 0 AND b.c > 0
"""

TRADED_SQL = f"""
SELECT CAST(day AS DATE) d, bool_or(picked AND NOT coalesce(skipped, false)) traded
FROM read_parquet('{HIST}/ranking/*.parquet', union_by_name=true) WHERE side = 'BUY' GROUP BY 1
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", default="2026-09-24", help="寄成の本番は 2026-09-24 から。それより前は 9:00 過ぎの回の気配")
    ap.add_argument("--model", default="config/daytrade/models/lgbm_rank.txt")
    a = ap.parse_args()

    df = duckdb.sql(SQL, params=[a.since, a.since]).df()
    if df.empty:
        print(f"{a.since} 以降で、気配・母集団・日足が揃った日がありません")
        return
    for c in ("price", "prev_close", "gap", "vol20", "turnover_med", "mkt_cap"):
        df[c] = df[c].astype(float)
    df["d"] = pd.to_datetime(df["d"])
    df["sector"] = df["sector"].fillna("")
    df["gap"] = df["price"] / df["prev_close"] - 1  # 丸める前の値で帯を見る（selection と同じ）
    df["gap_true"] = df["o"] / df["prev_close"] - 1
    df["y_raw"] = df["c"] / df["o"] - 1
    df["turn_cap"] = df["turnover_med"] / df["mkt_cap"].replace(0.0, np.nan)
    g = df[(df["gap"] >= -1.0) & (df["gap"] < 0.0) & (df["price"] > limit_down(df["prev_close"].values))]
    g = with_rule_rank(g).reset_index(drop=True)

    model = lgb.Booster(model_file=a.model)
    score = model.predict(ranked(raw_features(g), g["d"]).values)
    traded = duckdb.sql(TRADED_SQL).df()
    traded["d"] = pd.to_datetime(traded["d"])

    rows = []
    for ranker, s in (("gap_vol", -g["key_sort"].values), ("lgbm", score), ("gap", -g["gap"].values)):
        p = evaluate(g, s, ranker, 0).merge(g[["d", "code", "gap_true", "from_book", "run_at"]], on=["d", "code"])
        p["ret"] = p["w"] * (p["y_raw"] - liq_cost_bp(p["turnover_med"].values) / 1e4)
        d = p.groupby("d").agg(round=("run_at", "first"), picks=("code", lambda x: " ".join(x)), ret_bp=("ret", lambda x: x.sum() * 1e4),
                               seen_gap=("gap", "mean"), true_gap=("gap_true", "mean"),
                               n_true_pos=("gap_true", lambda x: int((x >= 0).sum()))).reset_index()
        rows.append(d.assign(ranker=ranker))
    r = pd.concat(rows, ignore_index=True).merge(traded, on="d", how="left")
    r["traded"] = r["traded"].fillna(False).astype(bool)
    r["round"] = pd.to_datetime(r["round"], utc=True).dt.tz_convert("Asia/Tokyo").dt.strftime("%H:%M:%S")
    r[["seen_gap", "true_gap"]] *= 100
    r.to_parquet(OUT, index=False)

    print(r.sort_values(["d", "ranker"]).round(2).to_string(index=False))
    print("\n## 累計（bp/日、流動性別コスト、始値で建てて引けで手仕舞い）")
    for label, q in (("全部の日", r), ("本番が建てた日", r[r["traded"]]), ("見送った日", r[~r["traded"]])):
        if q.empty:
            continue
        w = q.pivot(index="d", columns="ranker", values="ret_bp")
        line = "、".join(f"{k} {w[k].mean():+.1f}" for k in w.columns)
        diff = w["lgbm"] - w["gap_vol"]
        t = diff.mean() / (diff.std(ddof=1) / np.sqrt(len(diff))) if len(diff) > 2 and diff.std(ddof=1) > 0 else float("nan")
        print(f"{label}（{len(w)} 日）: {line}、lgbm − gap_vol {diff.mean():+.1f}（t {t:.2f}）、"
              f"選定が実際は正のギャップだった割合 " + "、".join(f"{k} {v * 100:.0f}%" for k, v in (q.groupby("ranker")["n_true_pos"].sum() / (3 * q.groupby("ranker").size())).items()))


if __name__ == "__main__":
    main()
