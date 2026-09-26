"""ショートを板寄せの厚みで選ぶ（C）: 寄る前の発注の模擬の上で、厚みの代理を選定・配分に入れた 3 案を今の寄る前の形と比べる。

根拠と事前登録: vault 20-research/2026-09-jp-daytrade-short-reinforce.md（2026-09-26）。
厚みの効き [[2026-09-jp-gap-ticks]]、代理 [[2026-09-jp-daytrade-book-depth]]、寄る前の形 test/dt_short_preopen_judge.py --reproduce。

  test/.venv/bin/python test/dt_short_candidates.py                          # 候補表（約 1 分）
  bash test/heavy.sh test/.venv/bin/python test/dt_short_depth.py            # 厚みの列を足して 3 案を判定（数分）

真の厚み = 板寄せ出来高（ティックの最初の約定から 0.5 秒以内、9:31 まで）÷ 20 日平均出来高（前日までの 20 日の日足 Vo）。
代理 = 8:59 の (pAV + pBV) ÷ 20 日平均出来高。2 年分は板が無いので 真の厚み × exp(r)、r は板 slot 0859 とティックが
両方ある日の全銘柄（売買代金 1 億以上）の log(代理) − log(真) から、シードごとに独立に引く。
3 分位の切れ目は過去の候補（始値のギャップ +5% 以上）だけの累積。過去が 100 行に満たない日は切れ目なし。

案（どれも寄る前の形・上位 3・1/3 ずつが基準）:
  C1  代理が下位 3 分位の銘柄を外す（次点が繰り上がる）
  C2  見えるギャップの日内百分位 × 代理の日内百分位 で並べる
  C3  上位 3 に、その日の総額（本数 × 666,666）を代理に比例して配る。1 銘柄 100 万で頭打ち、余りは現金
"""

import argparse
import glob
import os

import duckdb
import numpy as np
import pandas as pd

import dt_short_preopen_judge as j
from dt_nscale import tstat
from dt_wf_target import liq_cost_bp

TRADES = "data/jquants/equities_trades"
DEPTH = "test/out/dt_short_depth_rows.parquet"
MAX_ORDER = 1_000_000
MIN_PAST = 100
FORMS = ("base", "C1", "C2", "C3")

AUCTION_SQL = """
WITH t AS (
  SELECT CAST(Code AS VARCHAR) code, TradingVolume v,
         CAST(substr(Time, 1, 2) AS INT) * 3600 + CAST(substr(Time, 4, 2) AS INT) * 60 + CAST(substr(Time, 7) AS DOUBLE) s
  FROM read_parquet('{f}') WHERE CAST(Code AS VARCHAR) IN (SELECT code FROM want)),
w AS (SELECT *, min(s) OVER (PARTITION BY code) s0 FROM t)
SELECT code, sum(v) FILTER (WHERE s <= s0 + 0.5) auc_vol FROM w GROUP BY 1"""


def vol20avg(since, until, codes=None):
    """前日までの 20 日の出来高の平均（その日の値は入れない）。"""
    months = pd.period_range(pd.Timestamp(since) - pd.Timedelta(days=45), until, freq="M")
    files = [f"data/jquants/equities_bars_daily/{m}.parquet" for m in months]
    files = [f for f in files if os.path.exists(f)]
    df = duckdb.sql(f"""
        WITH b AS (SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(Vo AS DOUBLE) vo
                   FROM read_parquet({files}, union_by_name=true) WHERE TRY_CAST(Vo AS DOUBLE) IS NOT NULL)
        SELECT d, code, avg(vo) OVER (PARTITION BY code ORDER BY d ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) vavg,
               count(*) OVER (PARTITION BY code ORDER BY d ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) nv
        FROM b""").df()
    df["d"] = pd.to_datetime(df["d"])
    df = df[(df["d"] >= since) & (df["nv"] >= 20)]
    return df[["d", "code", "vavg"]]


def auction_vol(pairs):
    return j_per_day(pairs, AUCTION_SQL)


def j_per_day(pairs, sql):
    out = []
    con = duckdb.connect()
    for d, g in pairs.groupby("d"):
        f = f"{TRADES}/{d:%Y-%m-%d}.parquet"
        if not os.path.exists(f):
            continue
        con.register("want", g[["code"]].drop_duplicates())
        out.append(con.sql(sql.format(f=f)).df().assign(d=d))
        con.unregister("want")
    return pd.concat(out, ignore_index=True)


def calib_residuals():
    """板 slot 0859 × ティックの日の、log(代理) − log(真の厚み)。売買代金 1 億以上の全銘柄。"""
    b = duckdb.sql(f"""
        SELECT CAST(day AS DATE) d, symbol || '0' code,
               coalesce(TRY_CAST(pAV AS DOUBLE), 0) + coalesce(TRY_CAST(pBV AS DOUBLE), 0) q
        FROM read_parquet('{j.BOOK}', union_by_name=true) WHERE slot = '0859'""").df()
    b["d"] = pd.to_datetime(b["d"])
    b = b[b["q"] > 0].drop_duplicates(["d", "code"])
    panel = max(glob.glob("data/jquants/_panel_cache/panel-*.parquet"), key=os.path.getmtime)
    tv = duckdb.sql(f"""SELECT CAST(d AS DATE) d, code, turnover_med, o / prev_close - 1 gap FROM read_parquet('{panel}')
                        WHERE CAST(d AS DATE) >= DATE '2026-09-01' AND turnover_med >= 1e8 AND segment = 'prime'""").df()
    tv["d"] = pd.to_datetime(tv["d"])
    b = b.merge(tv, on=["d", "code"])
    b = b.merge(vol20avg(b["d"].min(), b["d"].max()), on=["d", "code"])
    b = b.merge(auction_vol(b[["d", "code"]]), on=["d", "code"])
    b = b[(b["auc_vol"] > 0) & (b["vavg"] > 0)]
    b["proxy"], b["true"] = b["q"] / b["vavg"], b["auc_vol"] / b["vavg"]
    b["r"] = np.log(b["proxy"]) - np.log(b["true"])
    rho = b.groupby("d").apply(lambda g: g["proxy"].rank().corr(g["true"].rank()), include_groups=False)
    print(f"代理の較正: {b['d'].nunique()} 日（{b['d'].min():%m-%d}〜{b['d'].max():%m-%d}）、{len(b):,} 銘柄日、"
          f"日内順位相関の平均 {rho.mean():.3f}、r の中央値 {b['r'].median():+.3f}・sd {b['r'].std():.3f}")
    return b


def load(cand=j.CAND, until=j.LAST_DAY):
    """寄る前の模擬の候補（取引日・手仕舞い・今の形の列）に、真の厚みを足したもの。"""
    c = pd.read_parquet(cand)
    c = c[(c["d"] >= j.START) & (c["d"] <= until)].copy()
    days = j.trade_days(sorted(c["d"].unique()))
    c = c[c["d"].isin(days)].reset_index(drop=True)
    c["exit"] = np.where(c["c"] >= c["limit_up"], c["next_open"].fillna(c["c"]), c["px1520"].fillna(c["c"]))
    c["opened"] = (c["open_t"] < "09:00:03").fillna(False).values
    if os.path.exists(DEPTH):
        dep = pd.read_parquet(DEPTH)
    else:
        dep = c[["d", "code"]].merge(vol20avg(c["d"].min(), c["d"].max()), on=["d", "code"], how="left")
        dep = dep.merge(auction_vol(c[["d", "code"]]), on=["d", "code"], how="left")
        dep.to_parquet(DEPTH, index=False)
    c = c.merge(dep, on=["d", "code"], how="left")
    c["depth"] = c["auc_vol"] / c["vavg"]
    c.loc[~(c["depth"] > 0), "depth"] = np.nan
    print(f"候補 {len(c):,} 行・取引日 {len(days)}、真の厚みが取れない（9:31 までに約定なし・20 日平均なし） "
          f"{c['depth'].isna().mean() * 100:.1f}%（+5% 以上で {c.loc[c['gap'] >= 0.05, 'depth'].isna().mean() * 100:.1f}%）")
    return c, days


def fill_day_median(c, x):
    s = pd.Series(x, index=c.index)
    return s.fillna(s.groupby(c["d"]).transform("median")).fillna(s.median()).values


def past_cutoff(c, proxy):
    """その日より前の候補（始値のギャップ +5% 以上）の代理の 1/3 分位。過去 100 行未満は nan。"""
    m = (c["gap"] >= 0.05).values
    days = np.sort(c["d"].unique())
    cut = {}
    past = []
    for d in days:
        n = sum(len(p) for p in past)
        cut[d] = np.quantile(np.concatenate(past), 1 / 3) if n >= MIN_PAST else np.nan
        sel = m & (c["d"].values == d)
        past.append(proxy[sel])
    return c["d"].map(cut).values


def select(c, vg, entry, proxy, cut):
    """4 案の取引（d・code・form・ret）。ret は脚の予算 200 万に対する割合。"""
    g = c.assign(vg=vg, entry=entry, px=proxy, cut=cut)
    g["vp"] = g["prev_close"] * (1 + g["vg"])
    g = g[(g["vg"] >= j.MIN_GAP) & (g["vg"] < j.MAX_GAP) & (g["vp"] < g["limit_up"]) & (g["vp"] * 100 <= j.ORDER)].copy()
    cost = (liq_cost_bp(g["turnover_med"].values) - 5.7 + j.SHORT_BASE_BP) / 1e4
    g["r"] = g["entry"] / g["exit"] - 1 - cost
    key = ["d", "vg", "code"]
    asc = [True, False, True]
    out = []
    base = g.sort_values(key, ascending=asc, kind="mergesort").groupby("d").head(j.N)
    out.append(base.assign(form="base", w=j.ORDER))
    c1 = g[~(g["px"] < g["cut"])]
    out.append(c1.sort_values(key, ascending=asc, kind="mergesort").groupby("d").head(j.N).assign(form="C1", w=j.ORDER))
    g["sc"] = g.groupby("d")["vg"].rank(pct=True) * g.groupby("d")["px"].rank(pct=True)
    c2 = g.sort_values(["d", "sc", "vg", "code"], ascending=[True, False, False, True], kind="mergesort").groupby("d").head(j.N)
    out.append(c2.assign(form="C2", w=j.ORDER))
    c3 = base.copy()
    n = c3.groupby("d")["code"].transform("size")
    c3["w"] = np.minimum(n * j.ORDER * c3["px"] / c3.groupby("d")["px"].transform("sum"), MAX_ORDER)
    out.append(c3.assign(form="C3"))
    p = pd.concat(out, ignore_index=True)
    p["ret"] = p["r"] * p["w"] / j.LEG
    return p[["d", "code", "form", "ret", "w", "px", "cut"]]


def daily(p, days):
    return p.groupby(["form", "d"])["ret"].sum().unstack("form").reindex(days).reindex(columns=list(FORMS)).fillna(0.0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    a = ap.parse_args()
    calib = calib_residuals()
    r_pool = calib["r"].values
    c, days = load()
    pre_pools = j.pools_of(j.book_err("0859", *j.REPRO_ERR))
    gap, o = c["gap"].values, c["o"].values

    # 参考: 始値のギャップ +5% 以上・ストップ高寄りでない候補の 1 取引の bp を、真の厚みの 3 分位（年ごと）で（gap-ticks の表の見方）
    s5 = c[(gap >= 0.05) & (c["o"] < c["limit_up"]) & c["depth"].notna()].copy()
    s5["bp"] = (s5["o"] / s5["exit"] - 1 - (liq_cost_bp(s5["turnover_med"].values) - 5.7 + j.SHORT_BASE_BP) / 1e4) * 1e4
    s5["yr"] = s5["d"].dt.year
    s5["t3"] = s5.groupby("yr")["depth"].transform(lambda x: pd.qcut(x.rank(method="first"), 3, labels=False)) + 1
    tb = s5.groupby(["yr", "t3"])["bp"].agg(["size", "mean", tstat]).round(1)
    print("参考: 1 取引の bp（始値で建て、取引日だけ）× 真の厚みの年別 3 分位\n" + tb.to_string())
    true_px = fill_day_median(c, c["depth"].values)
    true_cut = past_cutoff(c, true_px)

    # 模擬した代理の 3 分位の一致（全期間の +5% 以上の候補を 3 分位に、book-depth の表と同じ見方）
    m5 = gap >= 0.05
    tq = pd.qcut(pd.Series(c["depth"].values[m5]).rank(method="first"), 3, labels=False).values
    ok = ~np.isnan(c["depth"].values[m5])

    runs = {k: [] for k in ("pre_proxy", "pre_true", "up_true", "up_proxy")}
    agree = []
    for s in range(a.seeds):
        rng = np.random.default_rng(s)
        vg_pre = gap + j.draw(pre_pools, gap, rng) / 100
        px = fill_day_median(c, c["depth"].values * np.exp(rng.choice(r_pool, size=len(c))))
        cut = past_cutoff(c, px)
        pq = pd.qcut(pd.Series(px[m5]).rank(method="first"), 3, labels=False).values
        agree.append([np.mean(pq[ok & (tq == k)] == k) for k in range(3)] + [np.mean((pq - tq)[ok] ** 2 == 4)])
        runs["pre_proxy"].append(daily(select(c, vg_pre, o, px, cut), days))
        runs["pre_true"].append(daily(select(c, vg_pre, o, true_px, true_cut), days))
        runs["up_proxy"].append(daily(select(c, gap, o, px, cut), days))
        if s == 0:
            runs["up_true"].append(daily(select(c, gap, o, true_px, true_cut), days))
    ag = np.mean(agree, axis=0) * 100
    print(f"模擬した代理の 3 分位の一致（+5% 以上・全期間）: 薄 {ag[0]:.1f}% / 中 {ag[1]:.1f}% / 厚 {ag[2]:.1f}%、反転 {ag[3]:.1f}%"
          "（book-depth の実測 86.5 / 75.0 / 88.9%、反転 0%）")

    half = j.HALF
    bp = lambda x: x.mean() * 1e4  # noqa: E731
    rows = []
    print(f"\n{days.min():%Y-%m-%d}〜{days.max():%Y-%m-%d}、取引日 {len(days)}、bp/日（脚の予算 200 万、建てない日 0）、{a.seeds} シード平均")
    print("| 形 | 案 | bp/日 | 差（案 − 基準） | t | 前半 / 後半 | シード 差 > 0 |")
    print("|---|---|---|---|---|---|---|")
    for run, name in (("pre_proxy", "寄る前・代理（誤差あり）【判定】"), ("pre_true", "寄る前・真の厚み"),
                      ("up_proxy", "誤差なし・代理"), ("up_true", "誤差なし・真の厚み")):
        xs = runs[run]
        mean = sum(xs) / len(xs)
        for f in FORMS:
            if f == "base":
                print(f"| {name} | 基準 | {bp(mean[f]):+.2f} | | {tstat(mean[f]):+.2f} | "
                      f"{bp(mean[f][mean.index < half]):+.2f} / {bp(mean[f][mean.index >= half]):+.2f} | |")
                continue
            d = mean[f] - mean["base"]
            sd = np.array([bp(x[f] - x["base"]) for x in xs])
            h1, h2 = bp(d[d.index < half]), bp(d[d.index >= half])
            print(f"| {name} | {f} | {bp(mean[f]):+.2f} | {bp(d):+.2f} | {tstat(d):+.2f} | {h1:+.2f} / {h2:+.2f} | "
                  f"{int((sd > 0).sum())}/{len(xs)} |")
            rows.append(dict(run=run, form=f, bp=bp(mean[f]), base=bp(mean["base"]), diff=bp(d), t=tstat(d), h1=h1, h2=h2,
                             pos=int((sd > 0).sum()), n=len(xs)))
    r = pd.DataFrame(rows)
    r.to_csv("test/out/dt_short_depth_summary.csv", index=False)
    print("\n事前登録の判定（1 差 > 0 かつ t ≥ 2／2 16/20 以上／3 前後半とも正／4 真の厚み・誤差なしでも差 > 0）")
    for f in ("C1", "C2", "C3"):
        x = r[(r["run"] == "pre_proxy") & (r["form"] == f)].iloc[0]
        chk = [x["diff"] > 0 and x["t"] >= 2.0, x["pos"] >= 16 * x["n"] / 20, x["h1"] > 0 and x["h2"] > 0,
               all(r[(r["run"] != "pre_proxy") & (r["form"] == f)]["diff"] > 0)]
        print(f"  {f}: " + "".join("○" if k else "×" for k in chk) + (" → 採用候補" if all(chk) else " → 不採用"))


if __name__ == "__main__":
    main()
