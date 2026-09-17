"""daytrade ロングの並べ替えモデル（LightGBM）を学習し、本番用のモデルと Go の照合用データを書く。

根拠: vault 20-research/2026-09-jp-daytrade-ml-open-quote.md（学習の設定は minute-entry と同じ）

  test/.venv/bin/python test/dt_si_pit.py /tmp/dt_candidates.parquet /tmp/dt_candidates_pit.parquet
  test/.venv/bin/python test/dt_lgbm_train.py /tmp/dt_candidates_pit.parquet 2026-09-16

入力は候補表（vault の ml-rerank「候補表の再現」の手順で作る。/tmp/dt_rebuild.py）に、
test/dt_si_pit.py で空売り残高を「判定日の前日までに公表されたもの」へ差し替えたもの。
差し替えないと公表前の値で学習し、本番（plan）と特徴量がずれる。
出力:
  config/daytrade/models/lgbm_rank.txt          本番が読むモデル（LightGBM のテキスト形式）
  pkg/daytrade/rerank/testdata/parity.json      Go の特徴量・推論が Python と一致するかの照合用

特徴量の並びと作り方は pkg/daytrade/rerank/features.go と一致させること（変えたら両方直す）。
"""

import json
import sys

import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

VOL_FLOOR = 0.02
INNER_VALID_DAYS = 250
MODEL_OUT = "config/daytrade/models/lgbm_rank.txt"
PARITY_OUT = "pkg/daytrade/rerank/testdata/parity.json"

# 並びはモデルの入力順。Go の rerank.FeatureNames と同じ
FEATS = ["gap", "key", "vol20", "ret1", "ret5", "ret20", "pos20", "prev_intraday", "turn_cap",
         "log_turn", "log_cap", "log_price", "short_interest", "earn_yield", "n_cand", "rank_pct"]
KW = dict(num_leaves=15, learning_rate=0.05, min_child_samples=100,
          subsample=0.8, subsample_freq=1, colsample_bytree=0.8,
          random_state=0, n_jobs=-1, verbose=-1)


def raw_features(g):
    """その日の候補（帯で絞り、既存規則の順位 rule_rank が付いたもの）から順位化前の値を作る。"""
    out = pd.DataFrame(index=g.index)
    out["gap"] = g["gap"]
    out["key"] = np.round(g["gap"].values, 4) / np.maximum(g["vol20"].values, VOL_FLOOR)
    out["vol20"] = g["vol20"]
    for c in ["ret1", "ret5", "ret20", "pos20", "prev_intraday", "turn_cap", "short_interest", "earn_yield"]:
        out[c] = g[c]
    out["log_turn"] = np.log1p(g["turnover_med"])
    out["log_cap"] = np.log1p(g["mkt_cap"])
    out["log_price"] = np.log(g["price"])
    out["n_cand"] = g.groupby("d")["code"].transform("size").astype(float)
    out["rank_pct"] = g["rule_rank"] / out["n_cand"]
    return out[FEATS]


def ranked(raw, days):
    """日ごとの百分位順位（同順位は平均順位、欠損は順位の母数から外し 0.5 で埋める）。"""
    r = raw.groupby(days).rank(pct=True)
    return r.fillna(0.5)


def main():
    cand_path, last_day = sys.argv[1], pd.Timestamp(sys.argv[2])
    df = pd.read_parquet(cand_path)
    df["d"] = pd.to_datetime(df["d"])
    df = df[df["d"] <= last_day].copy()
    df["price"] = df["o"]
    df = df.sort_values(["d", "key_sort", "code"], kind="mergesort").reset_index(drop=True)
    df["rule_rank"] = df.groupby("d").cumcount() + 1

    X = ranked(raw_features(df), df["d"])
    y = df.groupby("d")["y_raw"].rank(pct=True)
    days = np.array(sorted(df["d"].unique()))
    inner_start = days[len(days) - INNER_VALID_DAYS]
    core = (df["d"] < inner_start).values

    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X.values[core], y.values[core], eval_set=[(X.values[~core], y.values[~core])],
          eval_metric="l2", callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    best = m.best_iteration_ or 200
    final = LGBMRegressor(n_estimators=best, **KW)
    final.fit(X.values, y.values)
    final.booster_.save_model(MODEL_OUT)
    print(f"学習 {len(df):,} 行 / {len(days):,} 日（{days[0]:%Y-%m-%d}〜{days[-1]:%Y-%m-%d}）、木 {best} 本 → {MODEL_OUT}")

    # 照合用: 直近 3 日の候補の生の値・順位化後・予測値。欠損と同順位を含む日を選ぶ
    cases = []
    for d in days[-3:]:
        g = df[df["d"] == d]
        raw = raw_features(g)
        rk = ranked(raw, g["d"])
        pred = final.predict(rk.values)
        rows = []
        for i, idx in enumerate(g.index):
            rows.append({
                "symbol": g.at[idx, "code"],
                "rule_rank": int(g.at[idx, "rule_rank"]),
                "raw": [None if pd.isna(v) else float(v) for v in raw.loc[idx].values],
                "ranked": [float(v) for v in rk.loc[idx].values],
                "score": float(pred[i]),
            })
        cases.append({"day": f"{pd.Timestamp(d):%Y-%m-%d}", "rows": rows})
    with open(PARITY_OUT, "w") as f:
        json.dump({"features": FEATS, "cases": cases}, f)
    print(f"照合用 {sum(len(c['rows']) for c in cases)} 行 → {PARITY_OUT}")


if __name__ == "__main__":
    main()
