"""daytrade ロング: 資金が増えて N を広げたとき、4 位以降（次点）をどう選べば薄まらないか。

根拠: vault 20-research/2026-09-jp-daytrade-nscale.md
候補表は test/dt_candidates.py の出力（test/out/dt_candidates.parquet）。

  test/.venv/bin/python test/dt_nscale.py [--part rank|keybin|tail|all]（sector_m: 業種の上限 m × R÷7）

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
CAPS_PROD = [3e6, 1e7, 3e7, 5e7]


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


def day_rules(te):
    """日の規則の近似: ショック（候補表の全銘柄のギャップの中央値 ≤ −2%）で資金 ×1.5、米国小幅高（ナスダック 0〜+1%）で休み。
    本番は 9:00 の市場ギャップと S&P500・VIX で判定するが、手元の ^TOPIX は始値が前日終値のままで使えず、
    S&P・VIX の履歴も無いので代用する（VIX の例外も無し）。"""
    days = te["d"].unique()
    tp = te.groupby("d")["gap"].median().rename("tgap").reset_index().rename(columns={"d": "date"})
    tp["date"] = tp["date"].astype("datetime64[ns]")
    us = pd.read_parquet("data/bars/^IXIC.parquet")
    us["date"] = pd.to_datetime(us["date"]).dt.tz_localize(None).dt.normalize().astype("datetime64[ns]")
    us["r"] = us["close"] / us["close"].shift(1) - 1
    d = pd.DataFrame({"d": pd.to_datetime(sorted(days)).astype("datetime64[ns]")})
    d = pd.merge_asof(d, us[["date", "r"]].rename(columns={"date": "ud"}), left_on="d", right_on="ud",
                      allow_exact_matches=False)
    d = d.merge(tp[["date", "tgap"]], left_on="d", right_on="date", how="left")
    d["mult"] = np.where(d["tgap"] <= -0.02, 1.5, 1.0)
    d["skip"] = (d["r"] >= 0) & (d["r"] <= 0.01) & (d["mult"] == 1.0)
    return d.set_index("d")[["mult", "skip"]]


def alloc_fixed_iv(g, n, cap_total):
    """順位 1..n に 20 日ボラの逆数で配分（本番の sizing）。"""
    sel = g[g["rank"] <= n]
    iv = 1 / np.maximum(sel["vol20"].fillna(0.02).values, 0.02)
    return sel.index, cap_total * iv / iv.sum() if len(sel) else np.array([])


def seen_ranked(te, pools, seed, sector_cap=True, per_sector=1, afford=None):
    """気配の誤差を入れて見えるギャップで並べ直し、業種の上限を掛ける（y_raw は真の値）。
    per_sector は 1 業種あたりの上限（本番の signal.max_per_sector）。業種が欠けた行は 1 つの業種として数える。
    afford は見える候補の表 → 1 単元が載るか（bool の配列）。渡すと載らない銘柄を業種の上限の前に外す
    （Go の candidatePool と同じ順番。test/dt_parity_go.py の unit_affordable）。既定の None は外さない（従来の形）。"""
    from dt_preopen_sim import BANDS, seen
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    rng = np.random.default_rng(seed)
    e = np.zeros(len(te))
    for b, pool in enumerate(pools):
        if (band == b).any():
            e[band == b] = rng.choice(pool, size=int((band == b).sum()))
    g = seen(te, e)
    g = g.sort_values(["d", "key_sort", "code"], kind="mergesort")
    if afford is not None:
        g = g[afford(g)]
    if sector_cap:
        g = g[g.groupby(["d", "sector"], dropna=False).cumcount() < per_sector]
    g = g.copy()
    g["rank"] = g.groupby("d").cumcount() + 1
    g["net"] = (g["y_raw"] - COST) * 1e4
    return g.reset_index(drop=True)


def part_sector(i0s, seeds, slot):
    """次の検証 5: 規則 R（p 0.2%、Rmax 20）で業種の上限を外すと容量の足しになるか。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    caps = [1e7, 3e7, 5e7]
    acc = {}
    for seed in range(seeds):
        for cap_on in [True, False]:
            g = seen_ranked(te, pools, seed, sector_cap=cap_on)
            days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
            for i0 in i0s:
                if cap_on:
                    acc[("kappa", i0, seed)] = calib_kappa(g, i0 * 1e-4)
                kappa = acc[("kappa", i0, seed)]
                for C in caps:
                    acc.setdefault((i0, C, cap_on), []).append(pd.Series(
                        {d: pnl_day(x, *alloc_rule(x, 0.002, 20, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    print(f"\n## I. 規則 R の業種の上限あり／なし（{seeds} シード平均）")
    for i0 in i0s:
        for C in caps:
            on = pd.concat(acc[(i0, C, True)], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
            off = pd.concat(acc[(i0, C, False)], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
            dd = off - on
            ann = lambda v: v.mean() * 245 / C * 100
            print(f"  I0 {i0:>4.0f} 資金 {C/1e4:5.0f} 万: 上限あり IS {ann(on[on.index <= IS_END]):5.1f}% OOS {ann(on[on.index > IS_END]):5.1f}%"
                  f" | 上限なし IS {ann(off[off.index <= IS_END]):5.1f}% OOS {ann(off[off.index > IS_END]):5.1f}%"
                  f" | 差の t IS {tstat(dd[dd.index <= IS_END]):5.2f} OOS {tstat(dd[dd.index > IS_END]):5.2f}")


def part_sector_m(i0s, seeds, slot, ms=(1, 2, 3, None)):
    """業種の上限 m（1・2・3・なし）× 規則 R÷7・20 位（本番の max_positions）。事前登録は 2026-09-25。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    caps = [7e6, 1.5e7, 3e7, 5e7]
    fn = lambda x, c: alloc_rule(x, 0.002, 20, c, k=7)
    acc = {}
    for seed in range(seeds):
        for m in ms:
            g = seen_ranked(te, pools, seed, sector_cap=m is not None, per_sector=m or 0)
            days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
            for i0 in i0s:
                if m == ms[0]:
                    acc[("kappa", i0, seed)] = calib_kappa(g, i0 * 1e-4)
                kappa = acc[("kappa", i0, seed)]
                for C in caps:
                    v = pd.Series({d: pnl_day(x, *fn(x, C * rules.loc[d, "mult"]), kappa) for d, x in days})
                    acc.setdefault((i0, C, m), []).append(v.reindex(alld).fillna(0.0))
                    if i0 == i0s[0] and seed == 0:
                        ns = [len(fn(x, C * rules.loc[d, "mult"])[0]) for d, x in days]
                        used = [fn(x, C * rules.loc[d, "mult"])[1].sum() / (C * rules.loc[d, "mult"]) for d, x in days]
                        acc[(C, m, "n")], acc[(C, m, "n20")], acc[(C, m, "used")] = np.mean(ns), np.mean(np.array(ns) >= 20), np.mean(used)
    print(f"\n## P. 業種の上限 m × R÷7・20 位（{seeds} シード、指標はシードごとに測って平均、% は対資金）")
    for i0 in i0s:
        for C in caps:
            print(f"  I0 {i0:.0f} bp・資金 {C/1e4:.0f} 万")
            base = acc[(i0, C, ms[0])]
            for m in ms:
                ss = acc[(i0, C, m)]
                row = []
                for pl, sl in [("IS", alld <= IS_END), ("OOS", alld > IS_END)]:
                    ann = np.mean([v[sl].mean() * 245 / C * 100 for v in ss])
                    dd = np.mean([max_dd(v[sl].values) / C * 100 for v in ss])
                    t = tstat(pd.concat(ss, axis=1).mean(axis=1)[sl] - pd.concat(base, axis=1).mean(axis=1)[sl]) if m != ms[0] else 0.0
                    row.append(f"{pl} 年率 {ann:5.1f}% 最大DD {dd:5.1f}% (対m=1 t {t:5.2f})")
                lab = f"m={m}" if m else "なし"
                print(f"    {lab:4s} 銘柄数 {acc[(C, m, 'n')]:4.1f} 20本の日 {acc[(C, m, 'n20')]:4.0%} 使用率 {acc[(C, m, 'used')]:4.0%}: " + " | ".join(row))


def part_prod(i0s, seeds, slot):
    """次の検証 2: 本番の形（気配の誤差・業種の上限・逆ボラ・ショック日 ×1.5・米国小幅高の休み）で D を測り直す。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    print(f"\n## E. 本番の形で測り直す（OOS 2022〜、気配の誤差 {seeds} シード、ショック日 {int((rules['mult'] > 1).sum())} 日、"
          f"休み {int(rules['skip'].sum())} 日）")
    acc = {}
    for seed in range(seeds):
        g = seen_ranked(te, pools, seed)
        g["rb"] = pd.cut(g["rank"], [0, 3, 10, 20, 40, 1e9], labels=["1-3", "4-10", "11-20", "21-40", "41+"])
        ins = g[g["d"] <= IS_END]
        tq_edges = ins["turnover_med"].quantile([1 / 3, 2 / 3]).values
        g["tq"] = np.searchsorted(tq_edges, g["turnover_med"].values)
        ins = g[g["d"] <= IS_END]
        a_rank = ins.groupby("rb", observed=True)["y_raw"].mean() - FEE
        a_rt = ins.groupby(["rb", "tq"], observed=True)["y_raw"].mean() - FEE
        g["alpha_rank"] = g["rb"].map(a_rank).astype(float)
        g["alpha_rt"] = [a_rt.get((r, q), np.nan) for r, q in zip(g["rb"], g["tq"])]
        g["alpha_rt"] = g["alpha_rt"].fillna(g["alpha_rank"])
        oos = g[g["d"] > IS_END]
        days = [(d, x) for d, x in oos.groupby("d") if not rules.loc[d, "skip"]]
        for i0 in i0s:
            kappa = calib_kappa(g, i0 * 1e-4)
            for C in CAPS_PROD:
                for n in [3, 9, 20]:
                    acc.setdefault((i0, C, f"N={n} 逆ボラ"), []).append(pd.Series(
                        {d: pnl_day(x, *alloc_fixed_iv(x, n, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
                for name in ALPHAS:
                    acc.setdefault((i0, C, f"最適 {name}"), []).append(pd.Series(
                        {d: pnl_day(x, *alloc_opt(x, x[name], kappa, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
    alld = sorted(set(te[te["d"] > IS_END]["d"]))
    for (i0, C, name), ss in acc.items():
        v = pd.concat(ss, axis=1).reindex(alld).fillna(0.0).mean(axis=1)   # 休みの日は 0、シードは平均
        base = pd.concat(acc[(i0, C, "N=3 逆ボラ")], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
        diff = v - base
        print(f"  I0 {i0:>4.0f} 資金 {C/1e4:5.0f} 万 {name:18s} 年率 {v.mean()*245/C*100:5.1f}% (t {tstat(v):4.1f})"
              f"  N=3 との日次差 t {tstat(diff) if name != 'N=3 逆ボラ' else 0:5.2f}")


def alloc_rule(g, p, rmax, cap_total, cap_name=np.inf, k=3, cap_top=None):
    """規則 R: 順位順に w = min(p × 売買代金, 資金 ÷ k, 1 銘柄の上限) を割り当て、資金か順位 rmax で止める。
    1 銘柄の上限は cap_name（cap_top を渡したら 1〜3 位だけ cap_top）。"""
    sel = g[g["rank"] <= rmax]
    cap_i = np.where(sel["rank"].values <= 3, cap_top, cap_name) if cap_top is not None else cap_name
    w = np.minimum(np.minimum(p * sel["turnover_med"].values, cap_total / k), cap_i)
    cum = np.cumsum(w)
    w = np.where(cum <= cap_total, w, np.maximum(cap_total - (cum - w), 0.0))
    m = w > 0
    return sel.index[m], w[m]


def part_rule(i0s, seeds, slot):
    """次の検証 3（事前登録済み）: 規則 R のつまみを IS で選び、OOS で N=3 逆ボラと比べる。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    grid = [(p, r) for p in [0.001, 0.002, 0.005, 0.01] for r in [10, 20]]
    caps = [1e7, 3e7, 5e7]
    acc = {}
    for seed in range(seeds):
        g = seen_ranked(te, pools, seed)
        days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
        for i0 in i0s:
            kappa = calib_kappa(g, i0 * 1e-4)
            for C in caps:
                acc.setdefault((i0, C, "N3"), []).append(pd.Series(
                    {d: pnl_day(x, *alloc_fixed_iv(x, 3, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
                for p, r in grid:
                    acc.setdefault((i0, C, (p, r)), []).append(pd.Series(
                        {d: pnl_day(x, *alloc_rule(x, p, r, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    avg = {k: pd.concat(v, axis=1).reindex(alld).fillna(0.0).mean(axis=1) for k, v in acc.items()}
    print(f"\n## F. 規則 R（事前登録）: IS で選んだつまみの OOS（{seeds} シード平均）")
    for i0 in i0s:
        for C in caps:
            ann = lambda v: v.mean() * 245 / C * 100
            isb = {k[2]: ann(v[v.index <= IS_END]) for k, v in avg.items() if k[0] == i0 and k[1] == C and k[2] != "N3"}
            best = max(isb, key=isb.get)
            v, b = avg[(i0, C, best)], avg[(i0, C, "N3")]
            vo, bo = v[v.index > IS_END], b[b.index > IS_END]
            print(f"  I0 {i0:>4.0f} 資金 {C/1e4:5.0f} 万: IS 最良 p {best[0]*100:.1f}% Rmax {best[1]}"
                  f"（IS {isb[best]:5.1f}% / N3 {ann(b[b.index <= IS_END]):5.1f}%）→ OOS {ann(vo):5.1f}% 対 N3 {ann(bo):5.1f}%"
                  f"、日次差 t {tstat(vo - bo):5.2f}")


EXT = "test/out/dt_candidates_ext.parquet"


def pool_of(c):
    return np.select([(c["segment"] == "prime") & (c["cap_tercile"] > 1), c["segment"] == "prime"],
                     ["a:プライム中上", "b:プライム小型"], "c:スタンダード")


def part_ext(i0s, seeds, slot):
    """次の検証 4 ①②（事前登録済み）: 母集団を広げて規則 R（p 0.2%、Rmax 20）で比べる。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet(EXT)
    te = te[(te["d"] >= SINCE) & (te["d"] <= "2026-09-18")].copy()
    te["pool"] = pool_of(te)
    # 記述: 母集団ごとに（真の始値で）並べたときの順位の帯の成績と売買代金
    print("\n## G. 母集団ごとの順位の帯（真の始値のギャップ < 0、各母集団の中で業種の上限）")
    d0 = te[te["gap"] < 0].sort_values(["d", "key_sort", "code"], kind="mergesort")
    for pool, g in d0.groupby("pool"):
        g = g[~g.duplicated(["d", "sector"])].copy()
        g["rank"] = g.groupby("d").cumcount() + 1
        g["net"] = (g["y_raw"] - COST) * 1e4
        for lo, hi in [(1, 3), (4, 10), (11, 20)]:
            x = g[(g["rank"] >= lo) & (g["rank"] <= hi)]
            s_ = daily_mean(x)
            print(f"  {pool} {lo:2d}-{hi:2d} 位: {split(s_)}  代金 中央値 {x['turnover_med'].median()/1e8:5.1f} 億")
    pools_e = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    variants = {"a": ["a:プライム中上"], "b": ["a:プライム中上", "b:プライム小型"],
                "c": ["a:プライム中上", "b:プライム小型", "c:スタンダード"]}
    caps = [1e7, 3e7, 5e7]
    acc = {}
    for seed in range(seeds):
        for v, pl in variants.items():
            g = seen_ranked(te[te["pool"].isin(pl)], pools_e, seed)
            days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
            for i0 in i0s:
                # kappa は母集団によらず同じ値にする（(a) の N=3 で I0 に合わせる）
                if v == "a":
                    acc[("kappa", i0, seed)] = calib_kappa(g, i0 * 1e-4)
                kappa = acc[("kappa", i0, seed)]
                for C in caps:
                    acc.setdefault((i0, C, v), []).append(pd.Series(
                        {d: pnl_day(x, *alloc_rule(x, 0.002, 20, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
                    if v == "a":
                        acc.setdefault((i0, C, "a N=3"), []).append(pd.Series(
                            {d: pnl_day(x, *alloc_fixed_iv(x, 3, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    alld = alld[alld > IS_END]
    print(f"\n## H. 母集団を広げた規則 R（OOS、{seeds} シード平均、日次差は (a) の規則 R と比べる）")
    for i0 in i0s:
        for C in caps:
            base = pd.concat(acc[(i0, C, "a")], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
            line = []
            for v in ["a N=3", "a", "b", "c"]:
                x = pd.concat(acc[(i0, C, v)], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
                t = tstat(x - base) if v not in ("a",) else 0.0
                line.append(f"{v} {x.mean()*245/C*100:5.1f}%(t{t:5.2f})")
            print(f"  I0 {i0:>4.0f} 資金 {C/1e4:5.0f} 万: " + "  ".join(line))


def alloc_iv_cap(g, n, cap_total, cap_name, spill):
    """N=n の逆ボラ配分に 1 銘柄の上限 cap_name を掛ける。spill なら、あふれた分を n+1 位以降へ順に
    （各 cap_name まで、20 位まで）回す。spill でなければ現金のまま（本番の max_order と同じ）。"""
    idx, w = alloc_fixed_iv(g, n, cap_total)
    w = np.minimum(w, cap_name)
    left = cap_total - w.sum()
    if spill and left > 1:
        ext = g[(g["rank"] > n) & (g["rank"] <= 20)]
        add = np.minimum(np.full(len(ext), cap_name), np.maximum(left - (np.cumsum(np.full(len(ext), cap_name)) - cap_name), 0))
        m = add > 0
        idx = idx.append(ext.index[m])
        w = np.concatenate([w, add[m]])
    return idx, w


def part_cap200(i0s, seeds, slot, caps, cap_name=2e6):
    """1 銘柄の上限 200 万: A 上限なし / B 上限・余りは現金 / C 上限・余りは 4 位以降へ。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    acc = {}
    for seed in range(seeds):
        g = seen_ranked(te, pools, seed)
        days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
        for i0 in i0s:
            kappa = calib_kappa(g, i0 * 1e-4)
            for C in caps:
                for lab, fn in [("A", lambda x, c: alloc_fixed_iv(x, 3, c)),
                                ("B", lambda x, c: alloc_iv_cap(x, 3, c, cap_name, False)),
                                ("C", lambda x, c: alloc_iv_cap(x, 3, c, cap_name, True))]:
                    acc.setdefault((i0, C, lab), []).append(pd.Series(
                        {d: pnl_day(x, *fn(x, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
                    if seed == 0 and i0 == i0s[0]:
                        acc[(C, lab, "cap")] = np.mean([fn(x, C * rules.loc[d, "mult"])[1].sum() / (C * rules.loc[d, "mult"])
                                                         for d, x in days])
                        acc[(C, lab, "bind")] = np.mean([(alloc_fixed_iv(x, 3, C * rules.loc[d, "mult"])[1] > cap_name).any()
                                                          for d, x in days])
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    print(f"\n## K. 1 銘柄の上限 {cap_name/1e4:.0f} 万（{seeds} シード平均、休みの日は 0）")
    for C in caps:
        print(f"  資金 {C/1e4:.0f} 万: 上限が効く日 {acc[(C, 'A', 'bind')]:.0%}、資金の使用率 A {acc[(C, 'A', 'cap')]:.0%} "
              f"B {acc[(C, 'B', 'cap')]:.0%} C {acc[(C, 'C', 'cap')]:.0%}")
    for i0 in i0s:
        for C in caps:
            a_ = pd.concat(acc[(i0, C, "A")], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
            for lab in ["B", "C"]:
                x = pd.concat(acc[(i0, C, lab)], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
                out = []
                for pl, sl in [("IS", a_.index <= IS_END), ("OOS", a_.index > IS_END)]:
                    dd = (x - a_)[sl]
                    out.append(f"{pl} A {a_[sl].mean()*245/C*100:5.1f}% → {x[sl].mean()*245/C*100:5.1f}% "
                               f"({dd.mean()*245/1e4:+6.1f} 万円/年, t {tstat(dd):5.2f})")
                print(f"  I0 {i0:>3.0f} 資金 {C/1e4:4.0f} 万 {lab}: " + " | ".join(out))


def part_rvc(i0s, seeds, slot, caps, cap_name=2e6):
    """規則 R（p 0.2%・20 位）と C（N=3 逆ボラ・1 銘柄 200 万・余りは 4 位以降）を同じ条件で直接比べる。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    acc = {}
    fns = [("A", lambda x, c: alloc_fixed_iv(x, 3, c)),
           ("C", lambda x, c: alloc_iv_cap(x, 3, c, cap_name, True)),
           ("R", lambda x, c: alloc_rule(x, 0.002, 20, c))]
    for seed in range(seeds):
        g = seen_ranked(te, pools, seed)
        days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
        for i0 in i0s:
            kappa = calib_kappa(g, i0 * 1e-4)
            for C in caps:
                for lab, fn in fns:
                    acc.setdefault((i0, C, lab), []).append(pd.Series(
                        {d: pnl_day(x, *fn(x, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    print(f"\n## L. 規則 R と C（1 銘柄 {cap_name/1e4:.0f} 万・余りは次点）の直接比較（{seeds} シード平均）")
    for i0 in i0s:
        for C in caps:
            v = {lab: pd.concat(acc[(i0, C, lab)], axis=1).reindex(alld).fillna(0.0).mean(axis=1) for lab, _ in fns}
            out = []
            for pl, sl in [("IS", alld <= IS_END), ("OOS", alld > IS_END)]:
                dd = (v["R"] - v["C"])[sl]
                out.append(f"{pl} A {v['A'][sl].mean()*245/C*100:5.1f}% C {v['C'][sl].mean()*245/C*100:5.1f}% "
                           f"R {v['R'][sl].mean()*245/C*100:5.1f}% R−C {dd.mean()*245/1e4:+6.1f} 万円/年 (t {tstat(dd):5.2f})")
            print(f"  I0 {i0:>3.0f} 資金 {C/1e4:4.0f} 万: " + " | ".join(out))


def part_r200(i0s, seeds, slot, caps, cap_name=2e6):
    """規則 R（p 0.2%・20 位）と、規則 R に 1 銘柄 200 万の上限を重ねた R200 を同じ条件で比べる。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    acc = {}
    fns = [("A", lambda x, c: alloc_fixed_iv(x, 3, c)),
           ("R", lambda x, c: alloc_rule(x, 0.002, 20, c)),
           ("R200", lambda x, c: alloc_rule(x, 0.002, 20, c, cap_name))]
    for seed in range(seeds):
        g = seen_ranked(te, pools, seed)
        days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
        for i0 in i0s:
            kappa = calib_kappa(g, i0 * 1e-4)
            for C in caps:
                for lab, fn in fns:
                    acc.setdefault((i0, C, lab), []).append(pd.Series(
                        {d: pnl_day(x, *fn(x, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
                    if seed == 0 and i0 == i0s[0]:
                        acc[(C, lab, "use")] = np.mean([fn(x, C * rules.loc[d, "mult"])[1].sum() / (C * rules.loc[d, "mult"])
                                                        for d, x in days])
                        acc[(C, lab, "n")] = np.mean([len(fn(x, C * rules.loc[d, "mult"])[0]) for d, x in days])
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    print(f"\n## M. 規則 R と、規則 R ＋ 1 銘柄 {cap_name/1e4:.0f} 万（{seeds} シード平均、休みの日は 0）")
    for C in caps:
        print(f"  資金 {C/1e4:.0f} 万: 資金の使用率 R {acc[(C, 'R', 'use')]:.0%} R200 {acc[(C, 'R200', 'use')]:.0%}、"
              f"銘柄数 R {acc[(C, 'R', 'n')]:.1f} R200 {acc[(C, 'R200', 'n')]:.1f}")
    for i0 in i0s:
        for C in caps:
            v = {lab: pd.concat(acc[(i0, C, lab)], axis=1).reindex(alld).fillna(0.0).mean(axis=1) for lab, _ in fns}
            out = []
            for pl, sl in [("IS", alld <= IS_END), ("OOS", alld > IS_END)]:
                dd = (v["R200"] - v["R"])[sl]
                out.append(f"{pl} A {v['A'][sl].mean()*245/C*100:5.1f}% R {v['R'][sl].mean()*245/C*100:5.1f}% "
                           f"R200 {v['R200'][sl].mean()*245/C*100:5.1f}% 差 {dd.mean()*245/1e4:+6.1f} 万円/年 (t {tstat(dd):5.2f})")
            print(f"  I0 {i0:>3.0f} 資金 {C/1e4:4.0f} 万: " + " | ".join(out))


def max_dd(x):
    cum = np.cumsum(x)
    return float(np.max(np.maximum.accumulate(np.concatenate([[0.0], cum]))[1:] - cum))


def part_k(i0s, seeds, slot, caps, ks=(3, 5, 6, 9)):
    """規則 R の 1 銘柄の上限を資金 ÷ k（k = 3, 5, 6, 9）にしたときの利益・最悪日・最大 DD（シードごとに測って平均）。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    fns = [("N=3", lambda x, c: alloc_fixed_iv(x, 3, c))] + \
          [(f"R÷{k}", (lambda k: lambda x, c: alloc_rule(x, 0.002, 20, c, k=k))(k)) for k in ks]
    acc = {}
    for seed in range(seeds):
        g = seen_ranked(te, pools, seed)
        days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
        for i0 in i0s:
            kappa = calib_kappa(g, i0 * 1e-4)
            for C in caps:
                for lab, fn in fns:
                    v = pd.Series({d: pnl_day(x, *fn(x, C * rules.loc[d, "mult"]), kappa) for d, x in days})
                    acc.setdefault((i0, C, lab), []).append(v.reindex(alld).fillna(0.0))
                    if i0 == i0s[0] and seed == 0:
                        acc[(C, lab, "n")] = np.mean([len(fn(x, C * rules.loc[d, "mult"])[0]) for d, x in days])
                        acc[(C, lab, "top")] = np.mean([fn(x, C * rules.loc[d, "mult"])[1].max() / (C * rules.loc[d, "mult"])
                                                        for d, x in days])
    print(f"\n## N. 規則 R の 1 銘柄の上限を資金 ÷ k に（{seeds} シード、指標はシードごとに測って平均、% は対資金）")
    for i0 in i0s:
        for C in caps:
            print(f"  I0 {i0:.0f} bp・資金 {C/1e4:.0f} 万")
            base = acc[(i0, C, "R÷3")]
            for lab, _ in fns:
                ss = acc[(i0, C, lab)]
                row = []
                for pl, sl in [("IS", alld <= IS_END), ("OOS", alld > IS_END)]:
                    ann = np.mean([v[sl].mean() * 245 / C * 100 for v in ss])
                    worst = np.mean([v[sl].min() / C * 100 for v in ss])
                    dd = np.mean([max_dd(v[sl].values) / C * 100 for v in ss])
                    t = tstat(pd.concat(ss, axis=1).mean(axis=1)[sl] - pd.concat(base, axis=1).mean(axis=1)[sl]) if lab != "R÷3" else 0.0
                    row.append(f"{pl} 年率 {ann:5.1f}% 最悪日 {worst:5.2f}% 最大DD {dd:5.1f}% (対R÷3 t {t:5.2f})")
                print(f"    {lab:4s} 銘柄数 {acc[(C, lab, 'n')]:4.1f} 最大の1銘柄 {acc[(C, lab, 'top')]:.0%}: " + " | ".join(row))


def alloc_prod(g, cap_total, base=1.67e6, max_n=20):
    """本番の形の近似: N = floor(資金 ÷ 1 注文の基準 167 万)（3 以上 max_n まで）、逆ボラ（spill_to_long と同じ式）。
    本番の capital.max_positions は 10 だが、規則 R の 20 位に揃えて 20 で測る。"""
    n = min(max(3, int(cap_total // base)), max_n)
    return alloc_fixed_iv(g, n, cap_total)


def part_fixed(i0s, seeds, slot, caps):
    """1 銘柄の上限を固定額にした規則 R（売買代金 × 0.2%・20 位は同じ）と、1〜3 位 180 万・他 90 万の段つき。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    fns = [("本番N", lambda x, c: alloc_prod(x, c)),
           ("R÷6", lambda x, c: alloc_rule(x, 0.002, 20, c, k=6)),
           ("R÷9", lambda x, c: alloc_rule(x, 0.002, 20, c, k=9))] + \
          [(f"固定{m}万", (lambda m: lambda x, c: alloc_rule(x, 0.002, 20, c, cap_name=m * 1e4, k=1))(m)) for m in (60, 90, 120, 150)] + \
          [("段180/90", lambda x, c: alloc_rule(x, 0.002, 20, c, cap_name=9e5, k=1, cap_top=1.8e6))]
    acc = {}
    for seed in range(seeds):
        g = seen_ranked(te, pools, seed)
        days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
        for i0 in i0s:
            kappa = calib_kappa(g, i0 * 1e-4)
            for C in caps:
                for lab, fn in fns:
                    v = pd.Series({d: pnl_day(x, *fn(x, C * rules.loc[d, "mult"]), kappa) for d, x in days})
                    acc.setdefault((i0, C, lab), []).append(v.reindex(alld).fillna(0.0))
                    if i0 == i0s[0] and seed == 0:
                        ws = [fn(x, C * rules.loc[d, "mult"]) for d, x in days]
                        acc[(C, lab, "n")] = np.mean([len(i) for i, _ in ws])
                        acc[(C, lab, "use")] = np.mean([w.sum() / (C * rules.loc[d, "mult"]) for (_, w), (d, _) in zip(ws, days)])
    print(f"\n## O. 1 銘柄の上限を固定額に（{seeds} シード、指標はシードごとに測って平均、% は対資金、IS / OOS）")
    for i0 in i0s:
        for C in caps:
            print(f"  I0 {i0:.0f} bp・資金 {C/1e4:.0f} 万")
            base = acc[(i0, C, "本番N")]
            for lab, _ in fns:
                ss = acc[(i0, C, lab)]
                cells = []
                for sl in [alld <= IS_END, alld > IS_END]:
                    ann = np.mean([v[sl].mean() * 245 / C * 100 for v in ss])
                    yen = np.mean([v[sl].mean() * 245 / 1e4 for v in ss])
                    worst = np.mean([v[sl].min() / C * 100 for v in ss])
                    dd = np.mean([max_dd(v[sl].values) / C * 100 for v in ss])
                    t = tstat(pd.concat(ss, axis=1).mean(axis=1)[sl] - pd.concat(base, axis=1).mean(axis=1)[sl]) if lab != "本番N" else 0.0
                    cells.append((ann, yen, worst, dd, t))
                (a1, y1, w1, d1, t1), (a2, y2, w2, d2, t2) = cells
                print(f"    {lab:7s} 銘柄数 {acc[(C, lab, 'n')]:4.1f} 使用率 {acc[(C, lab, 'use')]:4.0%}: 年率 {a1:5.1f} / {a2:5.1f}%"
                      f"（{y1:6.1f} / {y2:6.1f} 万円/年）最悪日 {w1:5.2f} / {w2:5.2f}% 最大DD {d1:5.1f} / {d2:5.1f}%"
                      f" 対本番 t {t1:5.2f} / {t2:5.2f}")


def part_now(i0s, seeds, slot, caps):
    """今の資金（ロング 500〜700 万）で規則 R（p 0.2%、Rmax 20 に固定）と N=3 逆ボラを比べる。"""
    from dt_preopen_sim import error_pools
    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    pools = error_pools(slot, "2026-09-11")
    rules = day_rules(te)
    acc = {}
    for seed in range(seeds):
        g = seen_ranked(te, pools, seed)
        days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
        for i0 in i0s:
            kappa = calib_kappa(g, i0 * 1e-4)
            for C in caps:
                acc.setdefault((i0, C, "N3"), []).append(pd.Series(
                    {d: pnl_day(x, *alloc_fixed_iv(x, 3, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
                acc.setdefault((i0, C, "R"), []).append(pd.Series(
                    {d: pnl_day(x, *alloc_rule(x, 0.002, 20, C * rules.loc[d, "mult"]), kappa) for d, x in days}))
                acc.setdefault((i0, C, "n"), []).append(pd.Series(
                    {d: len(alloc_rule(x, 0.002, 20, C * rules.loc[d, "mult"])[0]) for d, x in days}))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    print(f"\n## J. 今の資金で規則 R と N=3（{seeds} シード平均、休みの日は 0）")
    for i0 in i0s:
        for C in caps:
            r = pd.concat(acc[(i0, C, "R")], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
            n = pd.concat(acc[(i0, C, "N3")], axis=1).reindex(alld).fillna(0.0).mean(axis=1)
            k = pd.concat(acc[(i0, C, "n")], axis=1).mean(axis=1)
            for lab, sl in [("IS ", r.index <= IS_END), ("OOS", r.index > IS_END)]:
                dd = (r - n)[sl]
                print(f"  I0 {i0:>3.0f} 資金 {C/1e4:4.0f} 万 {lab}: N=3 {n[sl].mean()*245/C*100:5.1f}% / 規則 R "
                      f"{r[sl].mean()*245/C*100:5.1f}%  差 {dd.mean()*245/1e4:6.1f} 万円/年 (t {tstat(dd):5.2f})"
                      f"  規則 R の銘柄数 平均 {k.mean():.1f}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--part", default="all")
    ap.add_argument("--i0", type=float, nargs="+", default=[10.0])
    ap.add_argument("--seeds", type=int, default=3)
    ap.add_argument("--caps", type=float, nargs="*", default=None, help="資金（円）。--part fixed / k")
    ap.add_argument("--ks", type=int, nargs="*", default=[3, 5, 6, 9], help="資金 ÷ k の k。--part k だけ（3 は必ず含める）")
    ap.add_argument("--slot", default="0859")
    a = ap.parse_args()
    c = load()
    print(f"候補 {len(c):,} 行 / {c['d'].nunique():,} 日（{c['d'].min():%Y-%m-%d}〜{c['d'].max():%Y-%m-%d}）")
    if a.part in ("rank", "all"):
        part_rank(c)
    if a.part in ("keybin", "all"):
        part_keybin(c)
    if a.part == "rvc":
        part_rvc(a.i0, a.seeds, a.slot, [5e6, 7e6, 1e7, 3e7])
        return
    if a.part == "fixed":
        part_fixed(a.i0, a.seeds, a.slot, a.caps or [5e6, 7e6, 1e7, 3e7])
        return
    if a.part == "k":
        part_k(a.i0, a.seeds, a.slot, a.caps or [5e6, 7e6, 1e7, 3e7, 5e7], tuple(a.ks))
        return
    if a.part == "r200":
        part_r200(a.i0, a.seeds, a.slot, [7e6, 1e7, 3e7, 5e7])
        return
    if a.part == "cap200":
        part_cap200(a.i0, a.seeds, a.slot, [5e6, 7e6, 1e7])
        return
    if a.part == "now":
        part_now(a.i0, a.seeds, a.slot, [5e6, 7e6])
        return
    if a.part == "sector":
        part_sector(a.i0, a.seeds, a.slot)
        return
    if a.part == "sector_m":
        part_sector_m(a.i0, a.seeds, a.slot)
        return
    if a.part == "ext":
        part_ext(a.i0, a.seeds, a.slot)
        return
    if a.part == "rule":
        part_rule(a.i0, a.seeds, a.slot)
        return
    if a.part == "prod":
        part_prod(a.i0, a.seeds, a.slot)
        return
    if a.part in ("tail", "all"):
        part_tail(c)
    if a.part in ("capacity", "all"):
        for i0 in a.i0:
            part_capacity(c, i0 * 1e-4)


if __name__ == "__main__":
    main()
