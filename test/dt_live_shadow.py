"""本番の日ごとの影の記録: 並べ方（LBZ2 / gap_vol）× 配分（F6 / R7）の 4 通りで、その日に建てたらいくらだったか。

9/28 から LBZ2 と F6 を同時に入れたので、どちらが効いたかは「採らなかった方ならその日に何を建てたか」を毎日残して後から比べる。
材料はすべて本番の記録と日足（発注はしない・立花に繋がない）:
  state/daytrade/history/evaluation   全候補の始値・引け・コスト後の bp（寄→引）、LBZ2 の順位 rank・gap_vol の順位 rule_rank、発注時の気配 price
  state/daytrade/history/open_run     その日の総額（budget × n ＋ spill ではなく ranking の n × budget）、米国小幅高（us_low）と寄指の幅、休み
  data/jquants                         売買代金 20 日中央値（規則 R の上限）と 33 業種（1 業種 1 銘柄）

  PYTHONPATH=test test/.venv/bin/python test/dt_live_shadow.py [--since 2026-09-28] [--out ~/obsidian-vault/99-attachments/daytrade-shadow]

出力（同じ日の行は置き換える）:
  daily.csv   day, variant（LBZ2×F6 など）, names, amount, pnl（コスト後・円、寄→引）, actual_pnl（本番の台帳。variant = actual の行）
  picks.csv   day, variant, code, rank, rule_rank, price, open, amount, net_bp
cron で evaluate（20:20）の後に回し、vault に commit・push する（deploy/daytrade-shadow.sh）。
"""

import argparse
import glob
import os

import duckdb
import numpy as np
import pandas as pd

H = "state/daytrade/history"
BARS = "data/jquants/equities_bars_daily/*.parquet"
MASTER = "data/jquants/equities_master/*.parquet"
RATIO = 0.002
FORMS = {"F6": [1 / 4] * 6, "R7": [1 / 7] * 10}
ORDERS = {"LBZ2": "rank", "gap_vol": "rule_rank"}
LOT = 100


def latest(kind, since):
    fs = glob.glob(f"{H}/{kind}/*.parquet")
    d = pd.concat([pd.read_parquet(f) for f in fs], ignore_index=True)
    d["day"] = pd.to_datetime(d["day"])
    return d[d["day"] >= since]


def market(days, codes):
    """売買代金 20 日中央値（当日を含まない）と 33 業種。"""
    c = duckdb.connect()
    c.register("k", pd.DataFrame({"code": sorted(set(codes))}))
    lo = (min(days) - pd.Timedelta(days=45)).strftime("%Y-%m-%d")
    tv = c.execute(f"""
        WITH b AS (SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(Va AS DOUBLE) va
                   FROM read_parquet('{BARS}', union_by_name=true) WHERE Date >= '{lo}' AND CAST(Code AS VARCHAR) IN (SELECT code FROM k))
        SELECT d, code, MEDIAN(va) OVER (PARTITION BY code ORDER BY d ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) turnover_med FROM b""").df()
    sec = c.execute(f"""SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, CAST(S33 AS VARCHAR) sector
                        FROM read_parquet('{MASTER}', union_by_name=true) WHERE Date >= '{lo}'
                        AND CAST(Code AS VARCHAR) IN (SELECT code FROM k)""").df()
    m = tv.merge(sec, on=["d", "code"], how="left")
    m["d"] = pd.to_datetime(m["d"])
    return m.rename(columns={"d": "day"})


def alloc(g, key, weights, total):
    """規則 R: key の順に min(売買代金 × 0.2%, 総額 × 重み_k, 残り)、1 単元の倍数。1 単元が載らない・同じ業種は飛ばす。"""
    g = g[g[key].notna()].sort_values(key)
    out, left, k, used = [], total, 0, set()
    for r in g.itertuples():
        if k >= len(weights):
            break
        lim = min(RATIO * (r.turnover_med if r.turnover_med == r.turnover_med else 0.0), total * weights[k])
        unit = r.price * LOT
        if not unit > 0 or lim < unit:
            continue
        if isinstance(r.sector, str) and r.sector:
            if r.sector in used:
                continue
            used.add(r.sector)
        k += 1
        sh = np.floor(min(lim, left) / unit) * LOT
        if sh > 0:
            out.append((r.Index, sh))
            left -= sh * r.price
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", default="2026-09-28")
    ap.add_argument("--out", default=os.path.expanduser("~/obsidian-vault/99-attachments/daytrade-shadow"))
    a = ap.parse_args()
    since = pd.Timestamp(a.since)

    ev = latest("evaluation", since)
    if ev.empty:
        print("評価の記録がまだ無い")
        return
    ev = ev[ev["side"] == "BUY"]
    ev = ev[ev["recorded_at"] == ev.groupby("day")["recorded_at"].transform("max")].copy()   # その日の最後の評価
    ev["code"] = ev["code"].astype(str)
    runs = latest("open_run", since).sort_values("recorded_at")
    runs = runs[runs["mode"] == "live"].groupby("day").first()   # その日の最初の回（寄る前の回）
    ev = ev.merge(market(sorted(ev["day"].unique()), ev["code"]), on=["day", "code"], how="left")

    daily, picks = [], []
    for day, g in ev.groupby("day"):
        run = runs.loc[day] if day in runs.index else None
        n, budget = g["n"].dropna().iloc[0], g["budget"].dropna().iloc[0]
        total = float(n * budget)
        trade = bool(run["trade"]) if run is not None else True
        limit = float(run["preopen_limit_pct"]) if run is not None and bool(run.get("us_low")) and pd.notna(run.get("preopen_limit_pct")) else 0.0
        for oname, key in ORDERS.items():
            for fname, ws in FORMS.items():
                v = f"{oname}×{fname}"
                got = alloc(g, key, ws, total) if trade else []
                pnl, amt, names = 0.0, 0.0, 0
                for i, sh in got:
                    r = g.loc[i]
                    if limit and r["open"] > r["prev_close"] * (1 - limit / 100):
                        continue   # 小幅高の日の寄指が届かない（始値が指値より上）
                    a_ = sh * r["open"]
                    p = a_ * r["net_bp"] / 1e4
                    pnl, amt, names = pnl + p, amt + a_, names + 1
                    picks.append(dict(day=day, variant=v, code=r["code"], rank=r["rank"], rule_rank=r["rule_rank"],
                                      price=r["price"], open=r["open"], amount=a_, net_bp=r["net_bp"]))
                daily.append(dict(day=day, variant=v, names=names, amount=amt, pnl=pnl, actual_pnl=np.nan))
        act = g[g["picked"] == True]  # noqa: E712
        daily.append(dict(day=day, variant="actual", names=int((act["filled_quantity"].fillna(0) > 0).sum()),
                          amount=float((act["filled_quantity"].fillna(0) * act["actual_entry"].fillna(0)).sum()),
                          pnl=np.nan, actual_pnl=float(act["actual_pnl"].fillna(0).sum())))

    os.makedirs(a.out, exist_ok=True)
    for name, rows in (("daily", daily), ("picks", picks)):
        new = pd.DataFrame(rows)
        path = f"{a.out}/{name}.csv"
        if os.path.exists(path) and len(new):
            old = pd.read_csv(path, parse_dates=["day"])
            new = pd.concat([old[~old["day"].isin(new["day"].unique())], new], ignore_index=True)
        new.sort_values(["day", "variant"]).to_csv(path, index=False, date_format="%Y-%m-%d")
    d = pd.DataFrame(daily)
    print(d.pivot_table(index="day", columns="variant", values=["pnl", "actual_pnl"], aggfunc="sum").round(0).to_string())


if __name__ == "__main__":
    main()
