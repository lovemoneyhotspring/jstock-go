"""規則 R の寄成が、立花の「成行は制限値幅の上限 × 株数で信用新規建可能額を拘束する」で弾かれないか。
根拠: 立花 取引ルール（https://www.e-shiten.jp/TorihikiRule/rule/order-op.html「信用取引・新規建て」）。
規則 R の総額は 建可能額 × capacity_ratio（0.68）。寄成 10 本の拘束の合計 Σ 株数 × (前日終値 + 値幅) が
建可能額を超えると、超えた後の注文は余力不足で弾かれる。
候補表は test/dt_candidates.py の出力（test/out/dt_candidates.parquet）。株数は始値で決める（本番は 8:59:50 の気配）。
  test/.venv/bin/python test/dt_limitup_lock.py [--total 7582401] [--ratio 0.68]
出力: 日ごとの 拘束 ÷ 総額（lock_x）と 拘束 ÷ 建可能額（lock_k）の分布、何本目で建可能額を超えるか。
"""
import argparse
import numpy as np
import pandas as pd

CAND = "test/out/dt_candidates.parquet"
LIMITS = [(100, 30), (200, 50), (500, 80), (700, 100), (1000, 150), (1500, 300), (2000, 400), (3000, 500),
          (5000, 700), (7000, 1000), (10000, 1500), (15000, 3000), (20000, 4000), (30000, 5000), (50000, 7000),
          (70000, 10000), (100000, 15000), (150000, 30000), (200000, 40000), (300000, 50000)]


def width(p):
    for bound, w in LIMITS:
        if p < bound:
            return w
    return 70000


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--total", type=float, default=7_582_401, help="長短合計（建可能額 × ratio）。9/24 の値が既定")
    ap.add_argument("--ratio", type=float, default=0.68)
    ap.add_argument("--n", type=int, default=10)
    ap.add_argument("--div", type=int, default=7)
    ap.add_argument("--turn", type=float, default=0.002)
    ap.add_argument("--since", default="2017-01-01")
    a = ap.parse_args()
    cap_k = a.total / a.ratio
    c = pd.read_parquet(CAND, columns=["d", "code", "o", "prev_close", "key_sort", "turnover_med", "sector"])
    c = c[c["d"] >= a.since].sort_values(["d", "key_sort", "code"], kind="mergesort")
    c = c.drop_duplicates(["d", "sector"])  # 業種の上限 1
    rows = []
    for d, g in c.groupby("d", sort=True):
        left, name_cap, n, lock, over_at = a.total, a.total / a.div, 0, 0.0, 0
        for r in g.itertuples():
            if n >= a.n or left < 1:
                break
            amt = min(r.turnover_med * a.turn, name_cap, left)
            sh = np.floor(amt / r.o / 100) * 100
            if sh <= 0:
                continue
            n += 1
            left -= sh * r.o
            lock += sh * (r.prev_close + width(r.prev_close))
            if lock > cap_k and not over_at:
                over_at = n
        used = a.total - left
        rows.append((d, n, used, lock, over_at))
    df = pd.DataFrame(rows, columns=["d", "n", "used", "lock", "over_at"])
    df = df[df.n > 0]
    df["lock_x"] = df.lock / df.used
    df["lock_k"] = df.lock / cap_k
    q = [0.5, 0.9, 0.99, 1.0]
    print(f"日数 {len(df)}  総額 {a.total:,.0f}  建可能額 {cap_k:,.0f}（ratio {a.ratio}）")
    print("拘束 ÷ 使った額 :", df.lock_x.quantile(q).round(3).to_dict())
    print("拘束 ÷ 建可能額 :", df.lock_k.quantile(q).round(3).to_dict())
    bad = df[df.over_at > 0]
    print(f"建可能額を超える日 {len(bad)} / {len(df)}（{len(bad) / len(df):.1%}）  超える本目: {bad.over_at.value_counts().sort_index().to_dict()}")
    print("最近 10 日:\n", df.tail(10)[["d", "n", "used", "lock_k", "over_at"]].round({"used": 0, "lock_k": 3}).to_string(index=False))


if __name__ == "__main__":
    main()
