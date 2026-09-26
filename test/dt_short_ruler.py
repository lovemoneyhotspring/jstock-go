"""ショートに規則 R（D）: 今の inverse_vol・上位 3（1 銘柄 100 万）と、ロングの規則 R と同じ配分を寄る前の発注の模擬で比べる。

根拠と事前登録: vault 20-research/2026-09-jp-daytrade-short-reinforce.md（2026-09-26）。規則 R は [[2026-09-jp-daytrade-nscale]]。

  test/.venv/bin/python test/dt_short_candidates.py
  bash test/heavy.sh test/.venv/bin/python test/dt_short_ruler.py

基準（本番の config/daytrade_margin）: 見えるギャップ順の上位 3（1 単元が 67 万円以内）、総額 200 万を 20 日ボラ（下限 0.02）の
  逆数で按分、1 銘柄 100 万で頭打ち、100 株単位に切り捨て、1 銘柄 50 単元まで。余りは現金（selection.PickFrom と同じ順）
規則 R: 見えるギャップ順に 1 銘柄 min(売買代金 20 日中央値 × 0.2%, 200 万 ÷ 3) と残りの総額の小さい方、100 株単位、
  10 銘柄まで。1 単元がその上限に載らない銘柄は飛ばして次点。余りは現金
株数は見える値段で決め、損益は 株数 × (始値 − 手仕舞い) − コスト × 建玉金額。手仕舞いと carry は dt_short_preopen_judge と同じ
"""

import argparse

import numpy as np
import pandas as pd

import dt_short_preopen_judge as j
from dt_nscale import tstat
from dt_short_depth import load
from dt_wf_target import liq_cost_bp

TOTAL, BASE_N, BASE_BUDGET, MAX_ORDER = 2_000_000, 3, 670_000, 1_000_000
R_N, R_RATIO, R_DIV = 10, 0.002, 3
LOT, MAX_UNITS, VOL_FLOOR = 100, 50, 0.02


def allocate(g, form):
    """1 日ぶんの候補（見えるギャップの降順）から (index, 株数) を返す。"""
    out = []
    if form == "base":
        pool = g[g["vp"] * LOT <= BASE_BUDGET].head(BASE_N)
        w = 1 / np.maximum(pool["vol20"].fillna(VOL_FLOOR).values, VOL_FLOOR)
        amt = np.minimum(TOTAL * w / w.sum(), MAX_ORDER) if len(pool) else []
        for (i, r), a in zip(pool.iterrows(), amt):
            q = min(int(a // (r["vp"] * LOT)), MAX_UNITS) * LOT
            if q > 0:
                out.append((i, q))
        return out
    left = TOTAL
    cap_name = TOTAL // R_DIV
    for i, r in g.iterrows():
        if len(out) >= R_N:
            break
        cap = min(np.floor(r["turnover_med"] * R_RATIO), cap_name)
        if r["vp"] * LOT > cap:
            continue
        q = min(int(min(cap, left) // (r["vp"] * LOT)), MAX_UNITS) * LOT
        if q <= 0:
            continue
        out.append((i, q))
        left -= r["vp"] * q
    return out


def simulate(c, vg, days):
    g = c.assign(vg=vg)
    g["vp"] = g["prev_close"] * (1 + g["vg"])
    g = g[(g["vg"] >= j.MIN_GAP) & (g["vg"] < j.MAX_GAP) & (g["vp"] < g["limit_up"])]
    g = g.sort_values(["d", "vg", "code"], ascending=[True, False, True], kind="mergesort")
    rows = []
    for d, gd in g.groupby("d", sort=False):
        for form in ("base", "R"):
            for i, q in allocate(gd, form):
                rows.append((d, form, i, q))
    t = pd.DataFrame(rows, columns=["d", "form", "i", "q"])
    x = c.loc[t["i"]].reset_index(drop=True)
    t["amt"] = x["o"].values * t["q"]
    cost = (liq_cost_bp(x["turnover_med"].values) - 5.7 + j.SHORT_BASE_BP) / 1e4
    t["pnl"] = t["q"] * (x["o"].values - x["exit"].values) - cost * t["amt"]
    t["code"], t["turnover_med"] = x["code"].values, x["turnover_med"].values
    daily = t.groupby(["form", "d"])["pnl"].sum().unstack("form").reindex(days).fillna(0.0)
    return t, daily


def stats(t, x):
    cum = x.cumsum()
    dd = (cum - np.maximum(cum.cummax(), 0)).min()
    return dict(pnl=x.sum(), bp=x.mean() / TOTAL * 1e4, sharpe=x.mean() / x.std(ddof=1) * np.sqrt(245) if x.std() > 0 else 0,
                dd=dd, worst20=np.sort(t["pnl"].values)[:20].sum(), worst_day=x.min(), n=len(t),
                per_name=t["amt"].mean())


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    a = ap.parse_args()
    c, days = load()
    pre_pools = j.pools_of(j.book_err("0859", *j.REPRO_ERR))
    gap = c["gap"].values
    res = {"pre": [], "up": []}
    for s in range(a.seeds):
        rng = np.random.default_rng(s)
        res["pre"].append(simulate(c, gap + j.draw(pre_pools, gap, rng) / 100, days))
    res["up"].append(simulate(c, gap, days))

    half = j.HALF
    summ = []
    for form_run, name in (("pre", "寄る前（誤差あり）【判定】"), ("up", "誤差なし")):
        runs = res[form_run]
        per = {f: pd.DataFrame([stats(t[t["form"] == f], dl[f]) for t, dl in runs]) for f in ("base", "R")}
        mean_daily = sum(dl for _, dl in runs) / len(runs)
        d = (mean_daily["R"] - mean_daily["base"]) / TOTAL
        print(f"\n## {name}（{len(runs)} シード平均、{days.min():%Y-%m-%d}〜{days.max():%Y-%m-%d}、取引日 {len(days)}、総額 200 万）")
        print("| 配分 | 損益（円） | bp/日 | Sharpe | 最大 DD（円） | 最悪 20 取引（円） | 最悪の日（円） | 取引 | 1 取引の建玉（円） |")
        print("|---|---|---|---|---|---|---|---|---|")
        for f, lab in (("base", "今（inverse_vol・上位 3・100 万）"), ("R", "規則 R（0.2%・÷3・10 本）")):
            m = per[f].mean()
            print(f"| {lab} | {m['pnl']:+,.0f} | {m['bp']:+.2f} | {m['sharpe']:.2f} | {m['dd']:,.0f} | {m['worst20']:,.0f} | "
                  f"{m['worst_day']:,.0f} | {m['n']:.0f} | {m['per_name']:,.0f} |")
            summ.append(dict(run=form_run, form=f, **m.to_dict()))
        dd_better = (per["R"]["dd"] > per["base"]["dd"]).sum()
        w_better = (per["R"]["worst20"] > per["base"]["worst20"]).sum()
        print(f"日次差（R − 今）: {d.mean() * 1e4:+.2f} bp/日（t {tstat(d):+.2f}）、前半 {d[d.index < half].mean() * 1e4:+.2f} / "
              f"後半 {d[d.index >= half].mean() * 1e4:+.2f}。DD が小さいシード {dd_better}/{len(runs)}、最悪 20 取引が小さい {w_better}/{len(runs)}")
        t0 = runs[0][0]
        nday = t0[t0["form"] == "base"].groupby("d").size()
        one = nday[nday == 1].index
        for f in ("base", "R"):
            x = t0[(t0["form"] == f) & t0["d"].isin(one)]
            print(f"  候補（今の形で建てた銘柄）が 1 本の日 {len(one)} 日: {f} の建玉の平均 {x.groupby('d')['amt'].sum().mean():,.0f} 円・"
                  f"1 銘柄の最大 {x['amt'].max():,.0f} 円・その日の損益の最悪 {x.groupby('d')['pnl'].sum().min():,.0f} 円")
        if form_run == "pre":
            s = summ[-2:]
            chk = [s[1]["dd"] > s[0]["dd"] and s[1]["worst20"] > s[0]["worst20"]
                   and dd_better >= 16 * len(runs) / 20 and w_better >= 16 * len(runs) / 20, tstat(d) > -1.0]
    s = [x for x in summ if x["run"] == "up"]
    chk.append(s[1]["dd"] > s[0]["dd"] and s[1]["worst20"] > s[0]["worst20"])
    pd.DataFrame(summ).to_csv("test/out/dt_short_ruler_summary.csv", index=False)
    print("\n事前登録の判定（1 DD と最悪 20 取引がともに小さい・16/20 以上／2 日次差の t > −1／3 誤差なしでも 1 の向き）: "
          + "".join("○" if k else "×" for k in chk) + (" → 採用候補" if all(chk) else " → 不採用"))


if __name__ == "__main__":
    main()
