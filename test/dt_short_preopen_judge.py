"""ショートを寄る前（8:59 の気配）に発注する形の判定: 今の形（9:00:03 の気配）と比べる。

根拠: vault 20-research/2026-09-jp-daytrade-preopen-order.md（事前登録 2026-09-18、結果 2026-09-19）。
元の /tmp/dt_preopen_order.py・/tmp/ru_short_cand.parquet・/tmp/pre_ticks.parquet は消えたので、
ノートの事前登録から書き直したもの（2026-09-26）。候補表は test/dt_short_candidates.py が作る。

  test/.venv/bin/python test/dt_short_candidates.py                        # 候補表 → test/out/dt_short_candidates.parquet（約 1 分）
  bash test/heavy.sh test/.venv/bin/python test/dt_short_preopen_judge.py --check       # 8:59 台の記録が何営業日溜まったか
  bash test/heavy.sh test/.venv/bin/python test/dt_short_preopen_judge.py --reproduce   # 2026-09-19 の数字の再現（採否に使わない）
  bash test/heavy.sh test/.venv/bin/python test/dt_short_preopen_judge.py               # 判定（材料 20 営業日から。足りなければ止まる）

事前登録（ノートのまま。結果を見て変えない）:
  母集団  test/dt_short_candidates.py の既定の表（始値のギャップ +3% 以上。プライム・貸借・売買代金 1 億以上・決算前後と売り禁を除く・ストップ高寄りも残す）。
          取引日は trade_day だけ: 12 月・米国小幅高（前夜の S&P500 0〜+1% かつ VIX ≤ 24）・ショック日
          （候補全体＝プライムで売買代金 1 億以上の全銘柄の始値のギャップの中央値 ≤ −2%、または前夜の S&P500 ≤ −2%）を休む。
          期間 2024-09-02〜2026-09-16
  今の形（9:00:03）  ティックの最初の約定が 9:00:03 より前なら寄っている: 見えるギャップ = 始値のギャップ、
          建値 = 9:00:06 以後の最初の約定値（日足の始値に比で揃える）。寄っていなければ見えるギャップ = 始値のギャップ + e
          （e は 9:00 の板の記録のうち未寄付の行から、始値のギャップの帯ごとに引く）、建値は始値
  寄る前の形（8:59）  全銘柄で見えるギャップ = 始値のギャップ + e（8:59 台の記録から帯ごとに引く）、建値は始値
  参考（判定に使わない）  誤差なし・全銘柄を始値で建てる（上限）
  選び方  見えるギャップ [+5%, +100%) かつ見える値段 < ストップ高 かつ 1 単元が 666,666 円以内、
          ギャップの大きい順に上位 3、1 注文 666,666 円（脚の予算 200 万の 1/3 ずつ）
  手仕舞い  15:20 の分足の始値（2024-11-05 より前は引け）。引けがストップ高なら翌日の始値
  コスト  流動性別（ショート 5.3 bp ＋ 売買代金 20 日中央値 10 億以上 0 / 5〜10 億 +5 / 3〜5 億 +10 / 3 億未満 +20）
  指標    1 日の損益 ÷ 脚の予算（bp/日）、建てない日は 0。20 シード平均
  採否（すべて満たせば本番へ）
    1. 日次の対応差（寄る前 − 今）が正かつ t ≥ 2.0
    2. 20 シード中 16 以上で差が正
    3. 前半（〜2025-09）と後半（2025-10〜）で差がともに正

再現（--reproduce、2026-09-26）: 誤差の材料の行数・中央値はノートの表と一致する（8:59 の +5% 以上 100 行・中央値 0.00・
絶対 1.15、9:00 未寄付 90 行・−6.85）。成績は一致しない: 今 −0.94 → 寄る前 +31.65、差 +32.59（t 3.36、20/20、
前半 +11.09 / 後半 +55.20）、建てた日 83 / 213（取引日 242）、上限 +48.54。ノートは −11.43 → +16.21、差 +27.64（t 2.36）、
172 / 247（249 日）、上限 +22.06。上限（誤差に依らない）が 2 倍あるので、元の候補表か手仕舞いの値が違っていた。
こちらの上限は Go の backtest（test/out/trades_now.csv の同じ期間のショート 392 件、引けで +115 bp/件）と整合する。
今の形の建てた日の少なさ（83 対 172）の出どころも分からない。事前登録の 3 基準の判定はどちらでも同じ（○○○）。

判定の材料:寄る前の誤差は、事前登録の「8:59:45 の記録」の後継である層にした材料（test/dt_preopen_sim.py の SNAP_SLOT。
第一は発注時の気配と 8:59:48 以降の板、補いは 8:59:30）の、第一の材料のある日が 20 日になった時点の分。
9:00 の未寄付の誤差は板の記録の slot 0900 のうち、当日始値（pDOP）の無い行（2026-09-11 から同じ最終日まで）。
"""

import argparse
import json
import os
import sys

import duckdb
import numpy as np
import pandas as pd

import dt_preopen_sim as sim
from dt_lgbm_uslow_model import NEED_DAYS, material_days
from dt_nscale import US_JSON, US_SKIP_HIGH, US_SKIP_LOW, US_VIX_OVERRIDE, tstat
from dt_wf_target import liq_cost_bp

CAND = "test/out/dt_short_candidates.parquet"
BOOK = "state/daytrade/history/book/*.parquet"
BARS = "data/jquants/equities_bars_daily/*.parquet"
START, LAST_DAY, HALF = "2024-09-02", "2026-09-16", pd.Timestamp("2025-10-01")
MIN_GAP, MAX_GAP = 0.05, 1.0
N, ORDER, LEG = 3, 666_666, 2_000_000
SHORT_BASE_BP = 5.3                   # liq_cost_bp はロングの 5.7 が土台
BANDS = [-np.inf, -5.0, -3.0, -1.0, 0.0, 3.0, 5.0, np.inf]   # ノートの誤差の表と同じ帯（%pt、右閉じ）
REPRO_ERR = ("2026-09-11", "2026-09-17")   # 2026-09-19 の数字の誤差の材料（5 営業日）
T_MIN, SEEDS_OK = 2.0, 16
EXPECT = {"now": (-11.43, -0.94), "pre": (16.21, 1.45), "diff": (27.64, 2.36), "halves": (5.30, 51.27),
          "seeds": 20, "days": (172, 247), "upper": 22.06}

ERR_SQL = f"""
WITH b AS (
  SELECT CAST(day AS DATE) d, symbol, slot, TRY_CAST(pPRP AS DOUBLE) pc, TRY_CAST(pQAP AS DOUBLE) ask,
         TRY_CAST(pQBP AS DOUBLE) bid, coalesce(TRY_CAST(pDOP AS DOUBLE), 0) dop
  FROM read_parquet('{BOOK}', union_by_name=true)
  WHERE slot = ? AND CAST(day AS DATE) BETWEEN CAST(? AS DATE) AND CAST(? AS DATE)),
v AS (
  SELECT d, symbol, pc, dop, CASE WHEN ask > 0 AND bid > 0 THEN (ask + bid) / 2 WHEN ask > 0 THEN ask WHEN bid > 0 THEN bid END vis
  FROM b WHERE pc > 0),
q AS (
  SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(O AS DOUBLE) op
  FROM read_parquet('{BARS}', union_by_name=true) WHERE CAST(Date AS DATE) BETWEEN CAST(? AS DATE) AND CAST(? AS DATE))
SELECT v.d, v.symbol, v.dop > 0 AS opened, (q.op / v.pc - 1) * 100 g, (v.vis / v.pc - 1) * 100 - (q.op / v.pc - 1) * 100 e
FROM v JOIN q ON q.d = v.d AND q.code = v.symbol || '0' WHERE v.vis IS NOT NULL AND q.op > 0
"""


def book_err(slot, since, until, unopened=False):
    e = duckdb.sql(ERR_SQL, params=[slot, since, until, since, until]).df()
    return e[~e["opened"]] if unopened else e


def pools_of(err):
    band = np.digitize(err["g"].values, BANDS[1:-1], right=True)
    return [err["e"].values[band == b] for b in range(len(BANDS) - 1)]


def draw(pools, gap, rng):
    """始値のギャップ（小数）の帯ごとに、実測の誤差 e（%pt）を独立に引く。"""
    band = np.digitize(gap * 100, BANDS[1:-1], right=True)
    e = np.zeros(len(gap))
    for b in np.unique(band):
        if len(pools[b]) == 0:
            raise SystemExit(f"誤差の実測が 1 行も無い帯があります: {b}")
        e[band == b] = rng.choice(pools[b], size=int((band == b).sum()))
    return e


def trade_days(days):
    """休まない日（trade_day）。12 月・米国小幅高・ショック日を休む。"""
    u = pd.DataFrame(json.load(open(US_JSON)))
    u["date"] = pd.to_datetime(u["date"]).astype("datetime64[ns]")
    u = u.sort_values("date")
    u["r"] = u["spx"] / u["spx"].shift(1) - 1
    d = pd.DataFrame({"d": pd.DatetimeIndex(days).astype("datetime64[ns]")})
    d = pd.merge_asof(d, u[["date", "r", "vix"]].rename(columns={"date": "ud"}), left_on="d", right_on="ud",
                      allow_exact_matches=False, tolerance=pd.Timedelta(days=6))
    vix_ok = d["vix"].isna() | (d["vix"] <= 0) | (d["vix"] <= US_VIX_OVERRIDE)
    us_low = (d["r"] >= US_SKIP_LOW) & (d["r"] < US_SKIP_HIGH) & vix_ok
    panel = max(__import__("glob").glob("data/jquants/_panel_cache/panel-*.parquet"), key=os.path.getmtime)
    mg = duckdb.sql(f"""SELECT CAST(d AS DATE) d, median(o / prev_close - 1) mg FROM read_parquet('{panel}')
                        WHERE segment = 'prime' AND turnover_med >= 1e8 AND prev_close > 0 AND o > 0
                          AND CAST(d AS DATE) >= DATE '{START}' GROUP BY 1""").df()
    mg["d"] = pd.to_datetime(mg["d"]).astype("datetime64[ns]")
    d = d.merge(mg, on="d", how="left")
    shock = (d["mg"] <= -0.02) | (d["r"] <= -0.02)
    dec = d["d"].dt.month == 12
    keep = ~(dec | us_low | shock)
    print(f"取引日: 候補のある日 {len(d)}、休み 12 月 {int(dec.sum())}・米国小幅高 {int((us_low & ~dec).sum())}・"
          f"ショック {int((shock & ~dec & ~us_low).sum())} → {int(keep.sum())} 日")
    return pd.DatetimeIndex(d.loc[keep.values, "d"])


def pick(c, vis_gap, entry, form, seed):
    """見えるギャップで帯を絞り、大きい順に上位 3（1 単元が予算内のものだけ）。損益は 1/3 ずつ。"""
    g = c.assign(vg=vis_gap, entry=entry)
    g["vp"] = g["prev_close"] * (1 + g["vg"])
    g = g[(g["vg"] >= MIN_GAP) & (g["vg"] < MAX_GAP) & (g["vp"] < g["limit_up"]) & (g["vp"] * 100 <= ORDER)]
    g = g.sort_values(["d", "vg", "code"], ascending=[True, False, True], kind="mergesort").groupby("d").head(N)
    cost = (liq_cost_bp(g["turnover_med"].values) - 5.7 + SHORT_BASE_BP) / 1e4
    g["ret"] = (g["entry"] / g["exit"] - 1 - cost) * (ORDER / LEG)
    return g.assign(form=form, seed=seed)[["d", "code", "form", "seed", "ret", "opened"]]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true", help="材料の日数を見るだけ")
    ap.add_argument("--reproduce", action="store_true", help="2026-09-19 の数字の再現（誤差は 9/11〜9/17 の slot 0859/0900）")
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--cand", default=CAND)
    ap.add_argument("--until", default=LAST_DAY, help="検証期間の最終日（事前登録は 2026-09-16）")
    a = ap.parse_args()

    n, last = material_days()
    slots = duckdb.sql(f"""SELECT slot, count(DISTINCT CAST(day AS DATE)) nd, max(CAST(day AS DATE)) mx
                           FROM read_parquet('{BOOK}', union_by_name=true)
                           WHERE slot IN ('0859', '0900', '085945') GROUP BY 1 ORDER BY 1""").df()
    print(f"寄る前の誤差の材料（層にした snap の第一、8:59:48 以降）のある日: {n} 日（最終 {last}）、判定に要る日数 {NEED_DAYS}")
    print("板の記録: " + "、".join(f"slot {r.slot} {r.nd} 日（最終 {r.mx:%Y-%m-%d}）" for r in slots.itertuples()))
    if a.check:
        print("回してよい" if n >= NEED_DAYS else f"まだ（あと {NEED_DAYS - n} 日）")
        return
    if a.reproduce:
        print(f"**再現: 誤差は slot 0859 / 0900 の {REPRO_ERR[0]}〜{REPRO_ERR[1]}。採否に使わない**")
        pre_err = book_err("0859", *REPRO_ERR)
        now_err = book_err("0900", *REPRO_ERR, unopened=True)
    else:
        if n < NEED_DAYS:
            sys.exit(f"材料が {NEED_DAYS} 日に満たないので回さない（再現は --reproduce）")
        os.environ["DT_ERR_UNTIL"] = last
        os.environ.pop("DT_ERR_SLOT", None)
        pre_err = sim.error_frame(sim.SNAP_SLOT, "2026-09-18")
        now_err = book_err("0900", "2026-09-11", last, unopened=True)
    pre_pools, now_pools = pools_of(pre_err), pools_of(now_err)
    for name, err, pools in (("寄る前（8:59 台）", pre_err, pre_pools), ("9:00 未寄付", now_err, now_pools)):
        print(f"誤差 {name}: {err['d'].nunique()} 日、帯ごとの行数 {[len(p) for p in pools]}、"
              f"+5% 以上の中央値 {np.median(pools[-1]) if len(pools[-1]) else float('nan'):+.2f}・"
              f"絶対誤差の中央値 {np.median(np.abs(pools[-1])) if len(pools[-1]) else float('nan'):.2f}")

    c = pd.read_parquet(a.cand)
    c = c[(c["d"] >= START) & (c["d"] <= a.until)].copy()
    days = trade_days(sorted(c["d"].unique()))
    c = c[c["d"].isin(days)].reset_index(drop=True)
    c["exit"] = np.where(c["c"] >= c["limit_up"], c["next_open"].fillna(c["c"]), c["px1520"].fillna(c["c"]))
    c["opened"] = (c["open_t"] < "09:00:03").fillna(False).values
    now_entry = np.where(c["opened"], c["o"] * (c["t06"] / c["open_px"]).fillna(1.0), c["o"])
    print(f"候補 {len(c):,} 行、9:00:03 に寄っている {c['opened'].mean() * 100:.1f}%、"
          f"引けストップ高（翌日の始値で返済） {(c['c'] >= c['limit_up']).mean() * 100:.1f}%")

    picks = [pick(c, c["gap"].values, c["o"].values, "upper", 0)]
    for s in range(a.seeds):
        rng = np.random.default_rng(s)
        e_pre = draw(pre_pools, c["gap"].values, rng)
        e_now = draw(now_pools, c["gap"].values, rng)
        picks.append(pick(c, c["gap"].values + e_pre / 100, c["o"].values, "pre", s))
        picks.append(pick(c, np.where(c["opened"], c["gap"].values, c["gap"].values + e_now / 100), now_entry, "now", s))
    p = pd.concat(picks, ignore_index=True)
    os.makedirs("test/out", exist_ok=True)
    p.to_parquet(f"test/out/dt_short_preopen_{'repro' if a.reproduce else 'judge'}_picks.parquet", index=False)

    daily = p.groupby(["form", "seed", "d"])["ret"].sum()
    by_seed = {f: daily[f].unstack("seed").reindex(days).fillna(0.0) for f in ("upper", "pre", "now")}
    mean = {f: x.mean(axis=1) for f, x in by_seed.items()}
    built = {f: p[p["form"] == f].groupby("seed")["d"].nunique().mean() for f in ("pre", "now")}
    bp = lambda x: x.mean() * 1e4  # noqa: E731
    print(f"\n{days.min():%Y-%m-%d}〜{days.max():%Y-%m-%d}、取引日 {len(days)} 日、流動性別コスト、{a.seeds} シード平均、bp/日（建てない日は 0）")
    print("| 形 | bp/日 | t | 前半 / 後半 | 建てた日 | 選んだうち 9:00:03 に寄っていた |")
    print("|---|---|---|---|---|---|")
    for f, name in (("now", "今の形（9:00:03）"), ("pre", "寄る前（8:59）"), ("upper", "上限（誤差なし・始値、参考）")):
        x, q = mean[f], p[p["form"] == f]
        nd = built.get(f, q["d"].nunique())
        print(f"| {name} | {bp(x):+.2f} | {tstat(x):+.2f} | {bp(x[x.index < HALF]):+.2f} / {bp(x[x.index >= HALF]):+.2f} |"
              f" {nd:.0f} | {q['opened'].mean() * 100:.1f}% |")
    d = mean["pre"] - mean["now"]
    seed_d = (by_seed["pre"] - by_seed["now"]).mean(axis=0) * 1e4
    h1, h2 = bp(d[d.index < HALF]), bp(d[d.index >= HALF])
    pos = int((seed_d > 0).sum())
    print(f"\n差（寄る前 − 今）: {bp(d):+.2f} bp/日（t {tstat(d):+.2f}）、前半 {h1:+.2f} / 後半 {h2:+.2f}、"
          f"シード別 差 > 0 {pos}/{a.seeds}（{seed_d.min():+.2f}〜{seed_d.max():+.2f}）")

    checks = [bp(d) > 0 and tstat(d) >= T_MIN, pos >= SEEDS_OK * a.seeds / 20, h1 > 0 and h2 > 0]
    print("\n事前登録の基準: 1. 差 > 0 かつ t ≥ 2.0 " + "○×"[not checks[0]] + "／2. 16/20 以上 " + "○×"[not checks[1]]
          + "／3. 前半・後半とも正 " + "○×"[not checks[2]])
    if a.reproduce:
        x = EXPECT
        print(f"2026-09-19 の数字: 今 {x['now'][0]:+.2f}（t {x['now'][1]:+.2f}）→ 寄る前 {x['pre'][0]:+.2f}（t {x['pre'][1]:+.2f}）、"
              f"差 {x['diff'][0]:+.2f}（t {x['diff'][1]:+.2f}）、前半 {x['halves'][0]:+.2f} / 後半 {x['halves'][1]:+.2f}、"
              f"{x['seeds']}/20 シード、建てた日 {x['days'][0]} / {x['days'][1]}、上限 {x['upper']:+.2f}")
        print("判定: （再現なので出さない）")
    else:
        print("判定: " + ("寄る前の発注でショートを再開してよい（事前登録の 3 基準を満たす）" if all(checks)
                        else "見送り（事前登録の基準を欠く）"))


if __name__ == "__main__":
    main()
