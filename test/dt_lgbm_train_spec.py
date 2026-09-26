"""daytrade ロングの並べ替えモデルを「束」（マニフェスト＋モデル＋照合用データ）で学習して書き出す。

本番（Go）は signal.model / model_us_low が指すマニフェスト（.json）を読み、特徴量の並び・モデル（複数なら予測の平均）・
plan の特徴量の版・並べる前の売買代金の下限をそこから取る（pkg/daytrade/rerank/spec.go）。学習し直したモデルは
束ごと置き換えればよく、Go の rerank.featureFuncs にある特徴量だけを使うなら bin の作り直しは要らない。

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_train_spec.py lbz2        # 既定の組（PRESETS）で学習して書く
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_train_spec.py lbz2 --last-day 2026-09-15 --out /tmp/x  # 検証用

出力（--out の既定は config/daytrade/models/<名前>）:
  manifest.json                  Go が読むマニフェスト（features・models・plan_features・min_turnover・note）
  model_<k>.txt                  LightGBM のテキスト形式（bagging の本数だけ）
  pkg/daytrade/rerank/testdata/parity_<名前>.json   Go の順位化・推論が Python と一致するかの照合用（既定の出力のときだけ）

特徴量の式は pkg/daytrade/rerank/features.go の featureFuncs と一致させること（raw_named）。
"""

import argparse
import json
import os

import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

from dt_lgbm_gauss import target
from dt_lgbm_train import FEATS, INNER_VALID_DAYS, KW, ranked, raw_features
from dt_lgbm_bag import with_two_day
from dt_preopen_sim import with_rule_rank

CAND = "test/out/dt_candidates_wide.parquet"
MODELS_DIR = "config/daytrade/models"
PARITY_DIR = "pkg/daytrade/rerank/testdata"
PLAN_FEATURES = {"ret_d2": 2, "down2": 2}   # Go の rerank.planFeaturesOf と同じ

# 組の既定。features は Go の featureFuncs の名前、target は教師データ（dt_lgbm_gauss.target）
PRESETS = {
    "lbz2": dict(
        features=FEATS + ["ret_d2", "down2"], target="z", bags=5, min_turnover=5e8,
        note="LBZ2: 順位の特徴量 16 ＋ 前々日の騰落・2 日続落、教師データは寄→引の日ごとの頑健な z（±3）、"
             "bagging 5 本（random_state 0〜4）、並べる前に売買代金 5 億未満を外す。平常日用。"
             "根拠 vault 20-research/2026-09-jp-daytrade-raw-feats.md（test/dt_lgbm_bag.py・dt_lbz2_floor.py）"),
    "lgbm_rank": dict(
        features=FEATS, target="rank", bags=1, min_turnover=0.0,
        note="従来の 1 本（dt_lgbm_train.py と同じ形）。順位の特徴量 16、教師データは日ごとの百分位"),
}


def raw_named(g, names):
    """Go の featureFuncs と同じ式で、names の並びの順位化する前の特徴量を作る。"""
    raw = raw_features(g).replace([np.inf, -np.inf], np.nan)
    extra = {
        "ret_d2": g["ret_d2"].astype(float),
        # 片方でも取れなければ 0（NaN < 0 は偽）
        "down2": ((g["ret1"] < 0) & (g["ret_d2"] < 0)).astype(float),
    }
    return pd.DataFrame({n: (raw[n] if n in raw.columns else extra[n]) for n in names}, index=g.index)


def load(cand, last_day):
    df = pd.read_parquet(cand)
    df["d"] = pd.to_datetime(df["d"])
    if last_day:
        df = df[df["d"] <= pd.Timestamp(last_day)]
    df = df[df["gap"] < 0].copy()          # ロングの帯（Go の min_gap〜0）
    df["price"] = df["o"]
    df = with_two_day(df).rename(columns={"r_d2": "ret_d2"})
    return with_rule_rank(df)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("name", choices=sorted(PRESETS))
    ap.add_argument("--cand", default=CAND)
    ap.add_argument("--last-day", default=None, help="学習の最終日（既定は候補表の最終日）")
    ap.add_argument("--out", default=None, help="書き出し先のディレクトリ（既定 config/daytrade/models/<名前>）")
    a = ap.parse_args()
    p = PRESETS[a.name]
    out = a.out or os.path.join(MODELS_DIR, a.name)
    feats = p["features"]

    df = load(a.cand, a.last_day)
    X = ranked(raw_named(df, feats), df["d"]).values
    y = target(df, p["target"])
    days = np.array(sorted(df["d"].unique()))
    core = (df["d"] < days[len(days) - INNER_VALID_DAYS]).values
    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    best = m.best_iteration_ or 200
    os.makedirs(out, exist_ok=True)
    models, files = [], []
    for k in range(p["bags"]):
        mk = LGBMRegressor(n_estimators=best, **{**KW, "random_state": k}).fit(X, y)
        f = f"model_{k}.txt"
        mk.booster_.save_model(os.path.join(out, f))
        models.append(mk)
        files.append(f)
    manifest = {
        "name": a.name, "features": feats, "models": files,
        "plan_features": max([1] + [PLAN_FEATURES.get(n, 1) for n in feats]),
        "min_turnover": p["min_turnover"],
        "note": f"{p['note']}。学習 {days[0]:%Y-%m-%d}〜{days[-1]:%Y-%m-%d}（{len(days):,} 日・{len(df):,} 行）、木 {best} 本。"
                f"test/dt_lgbm_train_spec.py {a.name} で作成",
    }
    with open(os.path.join(out, "manifest.json"), "w") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
    print(f"学習 {len(df):,} 行 / {len(days):,} 日（{days[0]:%Y-%m-%d}〜{days[-1]:%Y-%m-%d}）、木 {best} 本 × {p['bags']} → {out}")
    if a.out:
        return  # 検証用。照合用データは既定の出力のときだけ

    # 照合用: 直近 3 日の候補の生の値・順位化後・予測値（bagging の平均）
    cases = []
    for d in days[-3:]:
        g = df[df["d"] == d]
        raw = raw_named(g, feats)
        rk = ranked(raw, g["d"])
        pred = np.mean([mk.predict(rk.values) for mk in models], axis=0)
        cases.append({"day": f"{pd.Timestamp(d):%Y-%m-%d}", "rows": [
            {"symbol": g.at[idx, "code"], "rule_rank": int(g.at[idx, "rule_rank"]),
             "raw": [None if pd.isna(v) else float(v) for v in raw.loc[idx].values],
             "ranked": [float(v) for v in rk.loc[idx].values], "score": float(pred[i])}
            for i, idx in enumerate(g.index)]})
    parity = os.path.join(PARITY_DIR, f"parity_{a.name}.json")
    with open(parity, "w") as f:
        json.dump({"spec": os.path.join(out, "manifest.json"), "features": feats, "cases": cases}, f)
    print(f"照合用 {sum(len(c['rows']) for c in cases)} 行 → {parity}")


if __name__ == "__main__":
    main()
