"""寄る前の発注（寄成）の模擬: 気配の誤差を入れて並べ、始値で建てたときのロングの成績。

根拠: vault 20-research/2026-09-jp-daytrade-preopen-order.md
（元の /tmp/dt_preopen_order.py は消えたので、ノートの事前登録から書き直したもの。2026-09-20。
8:59:45 の記録が 20 営業日溜まったら、--slot 085945 で測り直す）

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  test/.venv/bin/python test/dt_preopen_sim.py [--slot 0859] [--seeds 20] [--err-since 2026-09-11]

形:
  upper    誤差なし。始値のギャップで並べて始値で建てる（上限）
  preopen  見えるギャップ = 始値のギャップ + e。e は板の記録（--slot の時刻）と日足の始値の実測から、
           始値のギャップの帯ごとに独立に引く。見えるギャップで帯 [-1, 0) を絞って順位を付け直し、始値で建てる
  limit    （--limit-on-open のときだけ）preopen と同じ選定で、寄付条件つきの指値を見える値段に置く。
           始値が見える値段以下（e >= 0）の銘柄だけ約定し、約定しない枠は現金のまま。
           **未検証の案。結果を見る前に vault に事前登録を書いてから回すこと**

元の検証との違い（数字は完全には一致しない）:
  - 手仕舞いは日足の引け（元は 15:20 の分足の始値）
  - ロングだけ（ショートの候補表 /tmp/ru_short_cand.parquet も消えていて、ショートは paused 中）
  - 「今の形（9:00:03 のザラ場成行）」は入れていない（ティックの突き合わせが要る。寄成に切り替えたので比較対象でなくなった）
  - 候補は始値のギャップ +3% 未満まで広げてある（元は負だけ）。誤差で負に見える銘柄が候補に入る
学習は 2024-08 以前（本番と同じ設定、test/dt_lgbm_train.py）。12 月は除く。コストは流動性別。
"""

import argparse
import os

import duckdb
import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

from dt_candidates import limit_down
from dt_lgbm_train import INNER_VALID_DAYS, KW, VOL_FLOOR, ranked, raw_features
from dt_wf_target import evaluate, liq_cost_bp

CAND = "test/out/dt_candidates_wide.parquet"
BOOK = "state/daytrade/history/book/*.parquet"
BARS = "data/jquants/equities_bars_daily/*.parquet"
TEST_START = "2024-09-02"
HALF = "2025-10-01"
BANDS = [-np.inf, -5.0, -3.0, -1.0, 0.0, 3.0, np.inf]  # 始値のギャップ（%）。元の検証と同じ区切り

ERR_SQL = f"""
WITH b AS (
  SELECT CAST(day AS DATE) d, symbol, TRY_CAST(pPRP AS DOUBLE) pc, TRY_CAST(pQAP AS DOUBLE) ask, TRY_CAST(pQBP AS DOUBLE) bid
  FROM read_parquet('{BOOK}', union_by_name=true) WHERE slot = ? AND CAST(day AS DATE) >= CAST(? AS DATE)),
v AS (
  SELECT d, symbol, pc, CASE WHEN ask > 0 AND bid > 0 THEN (ask + bid) / 2 WHEN ask > 0 THEN ask WHEN bid > 0 THEN bid END vis
  FROM b WHERE pc > 0),
q AS (
  SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(O AS DOUBLE) op
  FROM read_parquet('{BARS}', union_by_name=true) WHERE CAST(Date AS DATE) >= CAST(? AS DATE))
SELECT v.d, (q.op / v.pc - 1) * 100 g, (v.vis / v.pc - 1) * 100 - (q.op / v.pc - 1) * 100 e
FROM v JOIN q ON q.d = v.d AND q.code = v.symbol || '0' WHERE v.vis IS NOT NULL AND q.op > 0
"""


def error_pools(slot, since):
    """帯ごとの誤差 e（見えるギャップ − 始値のギャップ、%pt）の実測。"""
    err = duckdb.sql(ERR_SQL, params=[slot, since, since]).df()
    err["band"] = np.digitize(err["g"], BANDS[1:-1], right=True)
    pools = [err.loc[err["band"] == b, "e"].values for b in range(len(BANDS) - 1)]
    print(f"誤差の実測: slot {slot}、{err['d'].nunique()} 日、帯ごとの行数 {[len(p) for p in pools]}、"
          f"絶対誤差の中央値 {[round(float(np.median(np.abs(p))), 2) if len(p) else None for p in pools]}")
    return pools


def train(df):
    """2024-08 以前の「始値のギャップが負」の候補で学習する（本番の学習と同じ設定）。"""
    tr = df[(df["d"] < TEST_START) & (df["gap"] < 0)].copy()
    tr = with_rule_rank(tr)
    X = ranked(raw_features(tr), tr["d"]).values
    y = tr.groupby("d")["y_raw"].rank(pct=True).values
    days = np.array(sorted(tr["d"].unique()))
    core = (tr["d"] < days[len(days) - INNER_VALID_DAYS]).values
    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    final = LGBMRegressor(n_estimators=m.best_iteration_ or 200, **KW)
    final.fit(X, y)
    print(f"学習 {len(tr):,} 行、木 {final.n_estimators} 本")
    return final


def with_rule_rank(g):
    g = g.copy()
    g["key_sort"] = np.where(g["vol20"].notna(),
                             np.round(g["gap"], 4) / np.maximum(g["vol20"].fillna(VOL_FLOOR), VOL_FLOOR), np.inf)
    g = g.sort_values(["d", "key_sort", "code"], kind="mergesort")
    g["rule_rank"] = g.groupby("d").cumcount() + 1
    return g


def seen(te, e_pct):
    """見えるギャップで候補を作り直す。gap・price・順位は見える値、y_raw と gap_true は実際の値。"""
    g = te.copy()
    g["gap_true"] = g["gap"]
    g["gap"] = g["gap"] + e_pct / 100
    g["price"] = g["prev_close"] * (1 + g["gap"])
    g = g[(g["gap"] >= -1.0) & (g["gap"] < 0.0) & (g["price"] > limit_down(g["prev_close"].values))]
    return with_rule_rank(g)


def run(model, g, form, ranker, seed, limit_on_open):
    score = model.predict(ranked(raw_features(g), g["d"]).values) if ranker == "lgbm" else -g["key_sort"].values
    p = evaluate(g, score, f"{form}/{ranker}", seed).merge(g[["d", "code", "gap_true"]], on=["d", "code"])
    p["filled"] = (p["gap_true"] <= p["gap"]) if limit_on_open else True
    return p


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--slot", default="0859")
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--err-since", default="2026-09-11")
    ap.add_argument("--limit-on-open", action="store_true")
    ap.add_argument("--cand", default=CAND, help="候補表。元の検証の再現には負のギャップだけの test/out/dt_candidates.parquet")
    a = ap.parse_args()

    pools = error_pools(a.slot, a.err_since)
    df = pd.read_parquet(a.cand)
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    model = train(df)
    te = df[(df["d"] >= TEST_START) & (df["d"].dt.month != 12)].copy()
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    # 候補に出てくる帯だけ見る（候補表を +3% で切っていれば最上位の帯は引かない）
    if empty := [int(b) for b in np.unique(band) if len(pools[b]) == 0]:
        raise SystemExit(f"誤差の実測が 1 行も無い帯があります: {empty}（記録の日数が足りない）")

    picks = []
    exact = seen(te, np.zeros(len(te)))
    for ranker in ("lgbm", "gap_vol"):
        picks.append(run(model, exact, "upper", ranker, 0, False))
    for s in range(a.seeds):
        rng = np.random.default_rng(s)
        e = np.empty(len(te))
        for b, pool in enumerate(pools):
            e[band == b] = rng.choice(pool, size=int((band == b).sum()))
        g = seen(te, e)
        for ranker in ("lgbm", "gap_vol"):
            picks.append(run(model, g, "preopen", ranker, s, False))
            if a.limit_on_open:
                picks.append(run(model, g, "limit", ranker, s, True))
    p = pd.concat(picks, ignore_index=True)
    p["ret"] = np.where(p["filled"], p["w"] * (p["y_raw"] - liq_cost_bp(p["turnover_med"].values) / 1e4), 0.0)
    tag = os.path.splitext(os.path.basename(a.cand))[0].replace("dt_candidates", "").strip("_") or "narrow"
    p.to_parquet(f"test/out/dt_preopen_sim_{a.slot}_{tag}_picks.parquet", index=False)

    days = pd.DatetimeIndex(sorted(te["d"].unique()))
    daily = p.groupby(["variant", "seed", "d"])["ret"].sum()
    series = {v: daily[v].groupby("d").mean().reindex(days).fillna(0.0) for v in daily.index.get_level_values(0).unique()}
    print(f"\n{days[0]:%Y-%m-%d}〜{days[-1]:%Y-%m-%d}、{len(days)} 日（12 月を除く）、流動性別コスト、bp/日（建てない日は 0）")
    print("| 形 | bp/日 | t | 前半 / 後半 | −5% 以下の割合（実際の始値） | 約定率 |")
    print("|---|---|---|---|---|---|")
    for v, x in series.items():
        q = p[p["variant"] == v]
        t = x.mean() / (x.std(ddof=1) / np.sqrt(len(x)))
        print(f"| {v} | {x.mean() * 1e4:+.2f} | {t:.2f} | {x[x.index < HALF].mean() * 1e4:+.2f} / {x[x.index >= HALF].mean() * 1e4:+.2f} |"
              f" {(q['gap_true'] <= -0.05).mean() * 100:.1f}% | {q['filled'].mean() * 100:.0f}% |")
    for form in [f for f in ("preopen", "limit") if f"{f}/lgbm" in series]:
        d = series[f"{form}/lgbm"] - series[f"{form}/gap_vol"]
        print(f"{form}: lgbm − gap_vol = {d.mean() * 1e4:+.2f} bp/日（t {d.mean() / (d.std(ddof=1) / np.sqrt(len(d))):.2f}）")
    lost = series["upper/lgbm"] - series["preopen/lgbm"]
    print(f"誤差で失うぶん（lgbm、上限 − preopen）= {lost.mean() * 1e4:+.2f} bp/日")


if __name__ == "__main__":
    main()
