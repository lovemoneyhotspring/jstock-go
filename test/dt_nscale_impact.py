"""daytrade ロング: 寄成の成行買いが板寄せ値をどれだけ押し上げるか（I0 の実測）を 8:59 の 10 段の板から見積もる。

根拠: vault 20-research/2026-09-jp-daytrade-nscale.md「次の検証 1」

  test/.venv/bin/python test/dt_nscale_impact.py

見積りの置き方（上限寄りの近似）:
  寄る前の板の売り側 1..10 段を順に食べ、累積の売り数量が自分の株数に届いた段の値を寄値とする。
  I(w) = その段の値 ÷ 売り 1 段目の値 − 1（自分がいないときの寄値を売り 1 段目と置いた増分）。
  8:59 から 9:00 までに注文はさらに入る（板寄せ出来高 ≒ 1.22 × 気配の薄い側、20-research/2026-09-jp-daytrade-book-depth）
  ので、実際の押し上げはこれより小さい。10 段で届かない銘柄は「10 段目の値 + 以降は同じ刻み」で外挿し、割合を出す。
母集団: 候補表（test/out/dt_candidates.parquet）の業種上限後の順位 1〜30 のうち、板の記録がある日。
"""

import glob

import numpy as np
import pandas as pd

from dt_nscale import load

BOOK = "state/daytrade/history/book/*.parquet"
WS = [1e6, 3e6, 1e7, 3e7]


def load_book():
    rows = []
    for f in sorted(glob.glob(BOOK)):
        b = pd.read_parquet(f)
        b = b[b["slot"].astype(str) < "0900"]
        if len(b):
            rows.append(b)
    b = pd.concat(rows)
    b["day"] = pd.to_datetime(b["day"])
    # 各日・銘柄で 9:00 より前の最後の記録
    b = b.sort_values("observed_at").groupby(["day", "symbol"]).tail(1)
    return b


def impact(row, w, keep_bids=False):
    ap = np.array([row[f"pGAP{i}"] for i in range(1, 11)], dtype=float)
    av = np.array([row[f"pGAV{i}"] for i in range(1, 11)], dtype=float)
    ok = ~np.isnan(ap) & ~np.isnan(av) & (ap > 0)
    ap, av = ap[ok], av[ok]
    if len(ap) < 2:
        return np.nan, np.nan
    q = w / ap[0]
    if keep_bids:
        # 寄る前の最良気配の数量は累積（板寄せで約定しうる量）。既存の買い（買い 1 段目）が全部そのまま残り、
        # 自分の買いはその後ろに並ぶと置く＝押し上げの上限側
        bv = float(row["pGBV1"]) if pd.notna(row["pGBV1"]) else 0.0
        q += bv
    cum = np.cumsum(av)
    k = np.searchsorted(cum, q)
    if k < len(ap):
        return ap[k] / ap[0] - 1, 0.0
    tick = (ap[-1] - ap[0]) / (len(ap) - 1)
    rest = q - cum[-1]
    per = av.mean()
    extra = np.ceil(rest / max(per, 1.0))
    return (ap[-1] + extra * tick) / ap[0] - 1, 1.0


def main():
    c = load()
    b = load_book()
    days = sorted(set(b["day"]) & set(c["d"]))
    print(f"板の記録と候補表が重なる日: {[d.strftime('%m-%d') for d in days]}")
    t = c[c["d"].isin(days) & (c["rank"] <= 30)].copy()
    t["symbol"] = t["code"].str[:4]
    m = t.merge(b, left_on=["d", "symbol"], right_on=["day", "symbol"], how="inner")
    print(f"突き合わせ {len(m)} / {len(t)} 行")
    out = []
    for _, r in m.iterrows():
        base_hi, _ = impact(r, 0.0, keep_bids=True)
        for w in WS:
            i, cens = impact(r, w)
            hi, cens_hi = impact(r, w, keep_bids=True)
            out.append((r["d"], r["code"], r["rank"], r["turnover_med"], r["vol20"], w, i, cens, hi - base_hi, cens_hi))
    o = pd.DataFrame(out, columns=["d", "code", "rank", "T", "vol20", "w", "imp", "cens", "imp_hi", "cens_hi"]).dropna()
    o["imp_hi_bp"] = o["imp_hi"] * 1e4
    o["imp_bp"] = o["imp"] * 1e4
    o["rb"] = pd.cut(o["rank"], [0, 3, 10, 30], labels=["1-3", "4-10", "11-30"])
    print("\n押し上げ（bp、片道）: 平均 / 中央値 / 10 段で届かない割合")
    g = o.groupby(["w", "rb"], observed=True).agg(mean=("imp_bp", "mean"), med=("imp_bp", "median"),
                                                  cens=("cens", "mean"), hi_mean=("imp_hi_bp", "mean"),
                                                  hi_med=("imp_hi_bp", "median"), hi_cens=("cens_hi", "mean"),
                                                  n=("imp_bp", "size"))
    g.index = g.index.set_levels([f"{w/1e4:.0f}万" for w in g.index.levels[0]], level=0)
    print(g.round(2).to_string())
    # 平方根則への当てはめ: imp = kappa * sqrt(w/T)（原点を通る最小二乗）
    xs = o[(o["rank"] <= 3) & (o["w"] == 1e6)]
    for col in ["imp", "imp_hi"]:
        x = np.sqrt(o["w"] / o["T"])
        kappa = (x * o[col]).sum() / (x * x).sum()
        i0 = kappa * np.sqrt(1e6 / xs["T"]).mean()
        print(f"\n平方根則の当てはめ（{col}）: kappa {kappa*1e4:.1f} bp → 1〜3 位・100 万の I0 {i0*1e4:.2f} bp（片道）")
    # 冪を自由にした当てはめ（log-log、押し上げ 0 の行は除く）
    p = o[o["imp_hi"] > 0]
    beta = np.polyfit(np.log(p["w"] / p["T"]), np.log(p["imp_hi"]), 1)
    print(f"冪の当てはめ（imp_hi）: imp ∝ (w/T)^{beta[0]:.2f}（押し上げ > 0 の {len(p)}/{len(o)} 行）")
    o.to_parquet("test/out/nscale_impact.parquet", index=False)


if __name__ == "__main__":
    main()
