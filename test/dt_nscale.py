"""daytrade ロング: 資金が増えて N を広げたとき、4 位以降（次点）をどう選べば薄まらないか。

根拠: vault 20-research/2026-09-jp-daytrade-nscale.md
候補表は test/dt_candidates.py の出力（test/out/dt_candidates.parquet）。

  test/.venv/bin/python test/dt_nscale.py [--part rank|keybin|tail|all]

本番に合わせる規則: gap_vol（key_sort 昇順、同順位は code 昇順）→ 同じ 33 業種は 1 日 1 銘柄。
コストは往復 5.7 bp（ロングの検証の前提）。指標は「その日の選定の等加重平均 net bp」を日次で平均する
（シグナル単位だと t が膨らむ → daytrade-signal-tests-need-day-clustering）。
"""

import argparse

import numpy as np
import pandas as pd

CAND = "test/out/dt_candidates.parquet"
COST = 5.7e-4
IS_END = "2021-12-31"
SINCE = "2017-01-01"


def load():
    c = pd.read_parquet(CAND)
    c = c[c["d"] >= SINCE].copy()
    c["net"] = (c["y_raw"] - COST) * 1e4
    c = c.sort_values(["d", "key_sort", "code"], kind="mergesort").reset_index(drop=True)
    c["raw_rank"] = c.groupby("d").cumcount() + 1
    # 業種の上限: その業種で最初（順位が上）の銘柄だけ残す
    c["sec_first"] = ~c.duplicated(["d", "sector"])
    c["rank"] = np.where(c["sec_first"], c[c["sec_first"]].groupby("d").cumcount().reindex(c.index) + 1, np.nan)
    return c


def tstat(x):
    x = np.asarray(x, dtype=float)
    x = x[~np.isnan(x)]
    if len(x) < 3:
        return np.nan
    return x.mean() / (x.std(ddof=1) / np.sqrt(len(x)))


def daily_mean(df, col="net"):
    return df.groupby("d")[col].mean()


def fmt(s):
    return f"{s.mean():6.1f} bp (t {tstat(s):5.2f}, 日数 {len(s):4d})"


def split(s):
    i = s[s.index <= IS_END]
    o = s[s.index > IS_END]
    return f"全 {fmt(s)} | IS {i.mean():6.1f} (t {tstat(i):5.2f}) | OOS {o.mean():6.1f} (t {tstat(o):5.2f})"


def part_rank(c):
    print("\n## A. 順位ごとの日次平均 net bp（業種の上限あり）")
    for r in list(range(1, 11)) + [15, 20, 30]:
        s = c[c["rank"] == r].set_index("d")["net"]
        print(f"  {r:2d} 位: {split(s)}")
    print("\n  業種の上限で落ちた銘柄（元の順位 ≤ 10 のもの）")
    s = daily_mean(c[(~c["sec_first"]) & (c["raw_rank"] <= 10)])
    print(f"   {split(s)}")
    print("\n  N を広げたときのバスケット（等加重）")
    for n in [3, 4, 5, 6, 9, 12]:
        s = daily_mean(c[c["rank"] <= n])
        print(f"   N={n:2d}: {split(s)}")


def part_keybin(c):
    print("\n## B. 鍵の絶対値（gap/vol20）で区切ると順位の崖は消えるか")
    sel = c[c["rank"] <= 10].copy()
    bins = [-np.inf, -3.0, -2.0, -1.5, -1.0, -0.75, -0.5, -0.25, 0]
    sel["kb"] = pd.cut(sel["key_sort"], bins)
    sel["grp"] = np.where(sel["rank"] <= 3, "1-3", "4-10")
    g = sel.groupby(["kb", "grp"], observed=True)["net"].agg(["mean", "count"]).unstack("grp")
    print(g.round(1).to_string())
    print("\n  同じ区分の中で 4-10 位 − 1-3 位（シグナル単位の粗い見積り）")
    for kb, sub in sel.groupby("kb", observed=True):
        a = sub[sub["grp"] == "1-3"]["net"]
        b = sub[sub["grp"] == "4-10"]["net"]
        if len(a) > 30 and len(b) > 30:
            d = b.mean() - a.mean()
            se = np.sqrt(a.var() / len(a) + b.var() / len(b))
            print(f"   {str(kb):16s} 差 {d:6.1f} bp (t {d / se:5.2f})  n {len(a)}/{len(b)}")


# 滑りの平方根則: I_i = kappa * sqrt(金額 / 売買代金 20 日中央値)。kappa は「今の規模（1 銘柄 100 万）の
# N=3 の選定で金額加重平均が I0」に合わせる（20-research/2026-09-jp-daytrade-margin-80pct-compound と同じ置き方）。
FEE = 0.7e-4   # 往復 5.7 bp のうち滑り 5 bp を除いた残り（貸株料・金利）
BASE_W = 1e6
ALPHAS = ["alpha_rank", "alpha_rt"]
CAPS = [3e6, 1e7, 2e7, 3e7, 5e7]


def calib_kappa(c, i0):
    top = c[c["rank"] <= 3]
    return i0 / np.mean(np.sqrt(BASE_W / top["turnover_med"].values))


def alloc_fixed(g, n, cap_total):
    """順位 1..n に等金額。"""
    sel = g[g["rank"] <= n]
    return sel.index, np.full(len(sel), cap_total / max(len(sel), 1))


def alloc_opt(g, alpha, kappa, cap_total, max_names=60):
    """sum w_i (alpha_i - kappa sqrt(w_i/T_i)) を sum w_i <= C で最大化（限界は alpha - 1.5 kappa sqrt(w/T) = lam）。"""
    a = alpha.values
    T = g["turnover_med"].values
    ok = a > 0
    if not ok.any():
        return g.index[:0], np.array([])
    def w_of(lam):
        return np.where(a > lam, T * ((a - lam) / (1.5 * kappa)) ** 2, 0.0)
    w = w_of(0.0)
    if w.sum() > cap_total:
        lo, hi = 0.0, a.max()
        for _ in range(50):
            mid = (lo + hi) / 2
            if w_of(mid).sum() > cap_total:
                lo = mid
            else:
                hi = mid
        w = w_of(hi)
    m = w > 0
    return g.index[m], w[m]


def pnl_day(g, idx, w, kappa):
    y = g.loc[idx, "y_raw"].values
    T = g.loc[idx, "turnover_med"].values
    return float(np.sum(w * (y - FEE - kappa * np.sqrt(w / T))))


def part_capacity(c, i0=10e-4):
    kappa = calib_kappa(c, i0)
    print(f"\n## D. 資金規模ごとの損益（滑りの平方根則 I0 = {i0*1e4:.0f} bp、kappa {kappa*1e4:.1f} bp）")
    # alpha の見積りは IS（〜2021）だけから作る: 順位（業種の上限後）と鍵の区分の IS 平均
    c = c[c["rank"].notna()].copy()
    c["rb"] = pd.cut(c["rank"], [0, 3, 10, 20, 40, 1e9], labels=["1-3", "4-10", "11-20", "21-40", "41+"])
    c["kb"] = pd.cut(c["key_sort"], [-np.inf, -2.0, -1.5, -1.0, -0.5, np.inf])
    ins = c[c["d"] <= IS_END]
    a_rank = ins.groupby("rb", observed=True)["y_raw"].mean() - FEE
    a_rk = ins.groupby(["rb", "kb"], observed=True)["y_raw"].mean() - FEE
    c["alpha_rank"] = c["rb"].map(a_rank).astype(float)
    c["alpha_rk"] = [a_rk.get((r, k), np.nan) for r, k in zip(c["rb"], c["kb"])]
    c["alpha_rk"] = c["alpha_rk"].fillna(c["alpha_rank"])
    # 順位の帯 × 売買代金の絶対額 3 分位（区切りも IS で決める）
    tq_edges = ins["turnover_med"].quantile([1 / 3, 2 / 3]).values
    c["tq"] = np.searchsorted(tq_edges, c["turnover_med"].values)
    ins = c[c["d"] <= IS_END]
    a_rt = ins.groupby(["rb", "tq"], observed=True)["y_raw"].mean() - FEE
    c["alpha_rt"] = [a_rt.get((r, q), np.nan) for r, q in zip(c["rb"], c["tq"])]
    c["alpha_rt"] = c["alpha_rt"].fillna(c["alpha_rank"])
    oos = c[c["d"] > IS_END]
    days = list(oos.groupby("d"))
    caps = CAPS
    print(f"  OOS {oos['d'].nunique()} 日、1 日あたり損益の平均（万円）と年率換算 %（対資金、245 日）")
    rows = []
    for C in caps:
        res = {}
        for n in [3, 9, 20]:
            res[f"N={n}"] = [pnl_day(g, *alloc_fixed(g, n, C), kappa) for _, g in days]
        for name in ALPHAS:
            out, cnt = [], []
            for _, g in days:
                idx, w = alloc_opt(g, g[name], kappa, C)
                out.append(pnl_day(g, idx, w, kappa)); cnt.append(len(idx))
            res[f"opt:{name}(平均{np.mean(cnt):.1f}銘柄)"] = out
        line = [f"  資金 {C/1e4:5.0f} 万:"]
        for k, v in res.items():
            v = np.array(v)
            line.append(f"{k} {v.mean()/1e4:6.2f}万({v.mean()*245/C*100:4.0f}%, t{tstat(v):4.1f})")
        print("\n    ".join(line))


FEATS = ["key_sort", "gap", "vol20", "ret1", "ret5", "ret20", "pos20", "prev_intraday", "turn_cap",
         "turnover_med", "mkt_cap", "short_interest", "earn_yield"]


def part_tail(c, lo=4, hi=30):
    print(f"\n## C. 次点（{lo}〜{hi} 位）の中で成績を分ける特徴（日ごとの順位相関 IC、日次に畳んで t）")
    t = c[(c["rank"] >= lo) & (c["rank"] <= hi)].copy()
    rk = t.groupby("d")[FEATS + ["net"]].rank(pct=True)
    rk["d"] = t["d"]
    rows = []
    for f in FEATS:
        ic = rk.groupby("d").apply(lambda g: g[f].corr(g["net"]), include_groups=False).dropna()
        i, o = ic[ic.index <= IS_END], ic[ic.index > IS_END]
        rows.append((f, i.mean(), tstat(i), o.mean(), tstat(o)))
    for f, im, it, om, ot in sorted(rows, key=lambda r: -abs(r[2])):
        print(f"  {f:14s} IS {im:+.3f} (t {it:5.2f}) | OOS {om:+.3f} (t {ot:5.2f})")
    print("\n  売買代金（20 日中央値）の絶対額 3 分位 × 順位の帯（net bp の日次平均、全期間）")
    u = c[c["rank"] <= 20].copy()
    u["tq"] = pd.qcut(u["turnover_med"], 3, labels=False)   # 絶対額の 3 分位（容量の話なので日内ではなく絶対額）
    u["rb"] = pd.cut(u["rank"], [0, 3, 10, 20], labels=["1-3", "4-10", "11-20"])
    for (rb, tq), g in u.groupby(["rb", "tq"], observed=True):
        s = daily_mean(g)
        print(f"   {rb:5s} 代金{'低中高'[int(tq)]}: {split(s)}  中央値 {g['turnover_med'].median()/1e8:5.1f} 億")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--part", default="all")
    ap.add_argument("--i0", type=float, nargs="+", default=[10.0])
    a = ap.parse_args()
    c = load()
    print(f"候補 {len(c):,} 行 / {c['d'].nunique():,} 日（{c['d'].min():%Y-%m-%d}〜{c['d'].max():%Y-%m-%d}）")
    if a.part in ("rank", "all"):
        part_rank(c)
    if a.part in ("keybin", "all"):
        part_keybin(c)
    if a.part in ("tail", "all"):
        part_tail(c)
    if a.part in ("capacity", "all"):
        for i0 in a.i0:
            part_capacity(c, i0 * 1e-4)


if __name__ == "__main__":
    main()
